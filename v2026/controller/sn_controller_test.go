package controller

import (
	"math/big"
	"testing"

	"github.com/urnetwork/connect/v2026"

	"github.com/urfoundation/sn/v2026/merkle"

	"github.com/urnetwork/server/v2026/model"
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
		connect.AssertEqual(t, err, nil)
		if i == 0 {
			root = leafRoot
		} else {
			connect.AssertEqual(t, leafRoot, root)
		}

		claimLeaf := merkle.PayoutLeaf(leaf.Coldkey, big.NewInt(int64(leaf.ShareBps)))
		connect.AssertEqual(t, merkle.Verify(root, claimLeaf, proof), true)
	}

	// a proof must not verify for a different leaf
	otherLeaf := merkle.PayoutLeaf(leaves[1].Coldkey, big.NewInt(int64(leaves[1].ShareBps)))
	_, proof, err := snPoolClaimProof(leaves, leaves[0].Coldkey, leaves[0].ShareBps)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, merkle.Verify(root, otherLeaf, proof), false)
}

func TestSnPoolClaimProofColdkeyNotInSet(t *testing.T) {
	leaves := testSnPayoutLeaves(5)

	// a coldkey with no leaf gets no proof
	var unknown [32]byte
	unknown[0] = 0xff
	_, _, err := snPoolClaimProof(leaves, unknown, 100)
	connect.AssertNotEqual(t, err, nil)

	// a known coldkey with the wrong share is a different leaf: no proof
	_, _, err = snPoolClaimProof(leaves, leaves[0].Coldkey, leaves[0].ShareBps+1)
	connect.AssertNotEqual(t, err, nil)
}

func TestSnPoolClaimProofSingleLeaf(t *testing.T) {
	leaves := testSnPayoutLeaves(1)

	// a single-leaf tree has an empty (non-nil) proof and root == leaf hash
	root, proof, err := snPoolClaimProof(leaves, leaves[0].Coldkey, leaves[0].ShareBps)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, len(proof), 0)

	claimLeaf := merkle.PayoutLeaf(leaves[0].Coldkey, big.NewInt(int64(leaves[0].ShareBps)))
	connect.AssertEqual(t, root, [32]byte(claimLeaf))
	connect.AssertEqual(t, merkle.Verify(root, claimLeaf, proof), true)

	wireProof := snProofBytes(proof)
	connect.AssertNotEqual(t, wireProof, nil)
	connect.AssertEqual(t, len(wireProof), 0)
}

func TestSnNoIdBytes(t *testing.T) {
	// the noId word is the 32-byte big-endian uint256 the contract keys by
	word := snNoIdBytes(1)
	connect.AssertEqual(t, len(word), 32)
	connect.AssertEqual(t, word[31], byte(1))
	connect.AssertEqual(t, new(big.Int).SetBytes(word).Uint64(), uint64(1))

	word = snNoIdBytes(0x0102030405060708)
	connect.AssertEqual(t, len(word), 32)
	connect.AssertEqual(t, new(big.Int).SetBytes(word).Uint64(), uint64(0x0102030405060708))
	for _, b := range word[:24] {
		connect.AssertEqual(t, b, byte(0))
	}
}
