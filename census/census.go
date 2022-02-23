package census

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"math/big"

	"github.com/iden3/go-iden3-crypto/babyjub"
	"github.com/iden3/go-iden3-crypto/poseidon"
	"github.com/vocdoni/arbo"
	"go.vocdoni.io/dvote/db"
)

var (
	hashLen   int    = arbo.HashFunctionPoseidon.Len()
	maxLevels int    = 64
	maxNLeafs uint64 = uint64(math.MaxUint64)
	maxKeyLen int    = int(math.Ceil(float64(maxLevels) / float64(8))) //nolint:gomnd
)

var (
	// ErrCensusNotClosed is used when trying to do some action with the Census
	// that needs the Census to be closed
	ErrCensusNotClosed = errors.New("Census not closed yet")
	// ErrCensusClosed is used when trying to add keys to a census and the census
	// is already closed
	ErrCensusClosed = errors.New("Census closed, can not add more keys")
	// ErrMaxNLeafsReached is used when trying to add a number of new publicKeys
	// which would exceed the maximum number of keys in the census.
	ErrMaxNLeafsReached = fmt.Errorf("MaxNLeafs (%d) reached", maxNLeafs)
)

// Census contains the MerkleTree with the PublicKeys
type Census struct {
	tree     *arbo.Tree
	db       db.Database
	Editable bool
}

// Options is used to pass the parameters to load a new Census
type Options struct {
	// DB defines the database that will be used for the census
	DB db.Database
}

// NewCensus loads the census
func NewCensus(opts Options) (*Census, error) {
	arboConfig := arbo.Config{
		Database:     opts.DB,
		MaxLevels:    maxLevels,
		HashFunction: arbo.HashFunctionPoseidon,
		// ThresholdNLeafs: not specified, use the default
	}

	wTx := opts.DB.WriteTx()
	defer wTx.Discard()

	tree, err := arbo.NewTreeWithTx(wTx, arboConfig)
	if err != nil {
		return nil, err
	}

	c := &Census{
		tree:     tree,
		Editable: true,
		db:       opts.DB,
	}

	// if nextIndex is not set in the db, initialize it to 0
	_, err = c.getNextIndex(wTx)
	if err != nil {
		err = c.setNextIndex(wTx, 0)
		if err != nil {
			return nil, err
		}
	}

	// commit the db.WriteTx
	if err := wTx.Commit(); err != nil {
		return nil, err
	}

	return c, nil
}

var dbKeyNextIndex = []byte("nextIndex")

func (c *Census) setNextIndex(wTx db.WriteTx, nextIndex uint64) error {
	b := make([]byte, 8)
	binary.LittleEndian.PutUint64(b, uint64(nextIndex))
	if err := wTx.Set(dbKeyNextIndex, b); err != nil {
		return err
	}
	return nil
}

func (c *Census) getNextIndex(rTx db.ReadTx) (uint64, error) {
	b, err := rTx.Get(dbKeyNextIndex)
	if err != nil {
		return 0, err
	}
	nextIndex := binary.LittleEndian.Uint64(b)
	return nextIndex, nil
}

// TODO probably not needed
// func hashPubK(pubK babyjub.PublicKey) (*big.Int, error) {
//         pubKHash, err := poseidon.Hash([]*big.Int{pubK.X, pubK.Y})
//         if err != nil {
//                 return nil, err
//         }
//         return pubKHash, nil
// }

func hashPubKBytes(pubK babyjub.PublicKey) ([]byte, error) {
	pubKHash, err := poseidon.Hash([]*big.Int{pubK.X, pubK.Y})
	if err != nil {
		return nil, err
	}
	return arbo.BigIntToBytes(hashLen, pubKHash), nil
}

// AddPublicKeys adds the batch of given PublicKeys, assigning incremental
// indexes to each one.
func (c *Census) AddPublicKeys(pubKs []babyjub.PublicKey) ([]arbo.Invalid, error) {
	if !c.Editable {
		return nil, ErrCensusClosed
	}
	wTx := c.db.WriteTx()
	defer wTx.Discard()

	nextIndex, err := c.getNextIndex(wTx)
	if err != nil {
		return nil, err
	}

	if nextIndex+uint64(len(pubKs)) > maxNLeafs {
		return nil, fmt.Errorf("%s, current index: %d, trying to add %d keys",
			ErrMaxNLeafsReached, nextIndex, len(pubKs))
	}
	var indexes [][]byte
	var pubKHashes [][]byte
	for i := 0; i < len(pubKs); i++ {
		// overflow in index should not be possible, as previously the
		// number of keys being added is already checked
		index := arbo.BigIntToBytes(maxKeyLen, big.NewInt(int64(int(nextIndex)+i))) //nolint:gomnd
		indexes = append(indexes[:], index)

		// store the mapping between PublicKey->Index
		pubKComp := pubKs[i].Compress()
		if err := wTx.Set(pubKComp[:], index); err != nil {
			return nil, err
		}

		pubKHashBytes, err := hashPubKBytes(pubKs[i])
		if err != nil {
			return nil, err
		}
		pubKHashes = append(pubKHashes, pubKHashBytes)
	}

	invalids, err := c.tree.AddBatchWithTx(wTx, indexes, pubKHashes)
	if err != nil {
		return invalids, err
	}
	if len(invalids) != 0 {
		return invalids, fmt.Errorf("Can not add %d PublicKeys", len(invalids))
	}

	// TODO check overflow
	if err = c.setNextIndex(wTx, (nextIndex)+uint64(len(pubKs))); err != nil {
		return nil, err
	}

	// commit the db.WriteTx
	if err := wTx.Commit(); err != nil {
		return nil, err
	}

	return nil, nil
}

// GetProof returns the leaf Value and the MerkleProof compressed for the given
// PublicKey
func (c *Census) GetProof(pubK babyjub.PublicKey) ([]byte, error) {
	if c.Editable {
		// if Editable is true, means that the Census is still being
		// updated. MerkleProofs will be generated once the Census is
		// closed for the final CensusRoot
		return nil, ErrCensusNotClosed
	}

	rTx := c.db.ReadTx()
	defer rTx.Discard()

	// get index of pubK
	pubKComp := pubK.Compress()
	indexBytes, err := rTx.Get(pubKComp[:])
	if err != nil {
		return nil, err
	}
	indexU64 := binary.LittleEndian.Uint64(indexBytes)
	index := arbo.BigIntToBytes(maxKeyLen, big.NewInt(int64(indexU64))) //nolint:gomnd

	_, leafV, s, existence, err := c.tree.GenProof(index)
	if err != nil {
		return nil, err
	}
	if !existence {
		// proof of non-existence currently not needed in the current use case
		return nil,
			fmt.Errorf("publicKey does not exist in the census (%x)", pubKComp[:])
	}
	hashPubKBytes, err := hashPubKBytes(pubK)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(leafV, hashPubKBytes) {
		return nil,
			fmt.Errorf("leafV!=pubK: %x!=%x", leafV, pubK)
	}
	return s, nil
}

// CheckProof checks a given MerkleProof of the given PublicKey (& index)
// for the given CensusRoot
func CheckProof(root, proof []byte, index int, pubK babyjub.PublicKey) (bool, error) {
	indexBytes := arbo.BigIntToBytes(maxKeyLen, big.NewInt(int64(index))) //nolint:gomnd
	hashPubK, err := hashPubKBytes(pubK)
	if err != nil {
		return false, err
	}

	return arbo.CheckProof(arbo.HashFunctionPoseidon, indexBytes, hashPubK, root, proof)
}
