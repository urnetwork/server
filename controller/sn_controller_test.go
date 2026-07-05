package controller

import (
	"math/big"
	"testing"

	"github.com/go-playground/assert/v2"

	"github.com/urnetwork/sn/merkle"

	"github.com/urnetwork/server/model"
)

// testSnPayoutLeaves builds a synthetic committed leaf set in leaf_index
// order, one distinct coldkey per leaf.
func testSnPayoutLeaves(n int) []*model.StPayoutLeaf {
	leaves := make([]*model.StPayoutLeaf, n)
	for i := 0; i < n; i += 1 {
		var coldkey [32]byte
		coldkey[0] = byte(i + 1)
		coldkey[31] = 0xa0
		leaves[i] = &model.StPayoutLeaf{
			Epoch:     7,
			NoId:      1,
			Coldkey:   coldkey,
			ShareBps:  (i + 1) * 100,
			LeafIndex: i,
		}
	}
	return leaves
}

func TestSnPoolClaimProof(t *testing.T) {
	leaves := testSnPayoutLeaves(5)

	// every leaf's proof must verify against the one shared root
	var root [32]byte
	for i, leaf := range leaves {
		leafRoot, proof, err := snPoolClaimProof(leaves, leaf.Coldkey, leaf.ShareBps)
		assert.Equal(t, err, nil)
		if i == 0 {
			root = leafRoot
		} else {
			assert.Equal(t, leafRoot, root)
		}

		claimLeaf := merkle.PayoutLeaf(leaf.Coldkey, big.NewInt(int64(leaf.ShareBps)))
		assert.Equal(t, merkle.Verify(root, claimLeaf, proof), true)
	}

	// a proof must not verify for a different leaf
	otherLeaf := merkle.PayoutLeaf(leaves[1].Coldkey, big.NewInt(int64(leaves[1].ShareBps)))
	_, proof, err := snPoolClaimProof(leaves, leaves[0].Coldkey, leaves[0].ShareBps)
	assert.Equal(t, err, nil)
	assert.Equal(t, merkle.Verify(root, otherLeaf, proof), false)
}

func TestSnPoolClaimProofColdkeyNotInSet(t *testing.T) {
	leaves := testSnPayoutLeaves(5)

	// a coldkey with no leaf gets no proof
	var unknown [32]byte
	unknown[0] = 0xff
	_, _, err := snPoolClaimProof(leaves, unknown, 100)
	assert.NotEqual(t, err, nil)

	// a known coldkey with the wrong share is a different leaf: no proof
	_, _, err = snPoolClaimProof(leaves, leaves[0].Coldkey, leaves[0].ShareBps+1)
	assert.NotEqual(t, err, nil)
}

func TestSnPoolClaimProofSingleLeaf(t *testing.T) {
	leaves := testSnPayoutLeaves(1)

	// a single-leaf tree has an empty (non-nil) proof and root == leaf hash
	root, proof, err := snPoolClaimProof(leaves, leaves[0].Coldkey, leaves[0].ShareBps)
	assert.Equal(t, err, nil)
	assert.Equal(t, len(proof), 0)

	claimLeaf := merkle.PayoutLeaf(leaves[0].Coldkey, big.NewInt(int64(leaves[0].ShareBps)))
	assert.Equal(t, root, [32]byte(claimLeaf))
	assert.Equal(t, merkle.Verify(root, claimLeaf, proof), true)

	wireProof := snProofBytes(proof)
	assert.NotEqual(t, wireProof, nil)
	assert.Equal(t, len(wireProof), 0)
}

func TestSnNoIdBytes(t *testing.T) {
	// the noId word is the 32-byte big-endian uint256 the contract keys by
	word := snNoIdBytes(1)
	assert.Equal(t, len(word), 32)
	assert.Equal(t, word[31], byte(1))
	assert.Equal(t, new(big.Int).SetBytes(word).Uint64(), uint64(1))

	word = snNoIdBytes(0x0102030405060708)
	assert.Equal(t, len(word), 32)
	assert.Equal(t, new(big.Int).SetBytes(word).Uint64(), uint64(0x0102030405060708))
	for _, b := range word[:24] {
		assert.Equal(t, b, byte(0))
	}
}
