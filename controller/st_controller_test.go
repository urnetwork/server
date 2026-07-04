package controller

// st_controller_test.go — offline unit tests for the pure st computation
// helpers (no pg/redis/chain): the payout-share computation (per-coldkey
// aggregation, a_min reliability damping, bps flooring + on-chain rollover
// remainder), the deposit sizing against the D-3 per-epoch cap, the
// block -> wall-clock estimate, and the payout tree/proof round trip
// against sn/merkle.

import (
	"math"
	"math/big"
	"testing"
	"time"

	"github.com/go-playground/assert/v2"

	"github.com/urnetwork/sn/merkle"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// testStColdkey builds a distinct coldkey from a marker byte.
func testStColdkey(marker byte) [32]byte {
	var coldkey [32]byte
	coldkey[0] = marker
	return coldkey
}

// testStOrderedIds returns two random network ids in ascending order.
func testStOrderedIds() (server.Id, server.Id) {
	a := server.NewId()
	b := server.NewId()
	if b.Less(a) {
		a, b = b, a
	}
	return a, b
}

func TestStComputePayoutSharesAggregatesPerColdkey(t *testing.T) {
	lowId, highId := testStOrderedIds()
	sharedColdkey := testStColdkey(2)
	otherColdkey := testStColdkey(1)

	// two networks behind one coldkey must produce exactly one leaf (the
	// contract dedups claims by (noId, coldkey)), with the smallest network
	// id as the representative
	entries := []*StShareEntry{
		{NetworkId: highId, Coldkey: sharedColdkey, UsageBytes: 600, Assignments: 10, Confirmations: 10},
		{NetworkId: lowId, Coldkey: sharedColdkey, UsageBytes: 400, Assignments: 10, Confirmations: 10},
		{NetworkId: server.NewId(), Coldkey: otherColdkey, UsageBytes: 1000, Assignments: 10, Confirmations: 10},
	}
	shares := stComputePayoutShares(entries, 1)

	assert.Equal(t, 2, len(shares))
	// canonical order: ascending coldkey bytes
	assert.Equal(t, otherColdkey, shares[0].Coldkey)
	assert.Equal(t, sharedColdkey, shares[1].Coldkey)
	// equal aggregate weights (600+400 vs 1000) split the pool evenly
	assert.Equal(t, 5000, shares[0].ShareBps)
	assert.Equal(t, 5000, shares[1].ShareBps)
	assert.Equal(t, lowId, shares[1].NetworkId)
}

func TestStComputePayoutSharesReliabilityAMin(t *testing.T) {
	// a network observed fewer than a_min times has its reliability damped
	// against a_min: full 4/4 confirmations at a_min=8 count as 4/8
	entries := []*StShareEntry{
		{NetworkId: server.NewId(), Coldkey: testStColdkey(1), UsageBytes: 1000, Assignments: 100, Confirmations: 100},
		{NetworkId: server.NewId(), Coldkey: testStColdkey(2), UsageBytes: 1000, Assignments: 4, Confirmations: 4},
	}
	shares := stComputePayoutShares(entries, 8)

	// weights 1000 and 500: floor(10000×2/3)=6666, floor(10000×1/3)=3333
	assert.Equal(t, 2, len(shares))
	assert.Equal(t, 6666, shares[0].ShareBps)
	assert.Equal(t, 3333, shares[1].ShareBps)
	assert.Equal(t, true, shares[0].ShareBps+shares[1].ShareBps <= 10000)
}

func TestStComputePayoutSharesFloorRollover(t *testing.T) {
	// three equal weights floor to 3333 each; the 1 bps remainder stays in
	// the pool (rolls over on chain by design)
	entries := []*StShareEntry{}
	for i := byte(1); i <= 3; i += 1 {
		entries = append(entries, &StShareEntry{
			NetworkId:     server.NewId(),
			Coldkey:       testStColdkey(i),
			UsageBytes:    1000,
			Assignments:   10,
			Confirmations: 10,
		})
	}
	shares := stComputePayoutShares(entries, 1)

	assert.Equal(t, 3, len(shares))
	bpsTotal := 0
	for _, share := range shares {
		assert.Equal(t, 3333, share.ShareBps)
		bpsTotal += share.ShareBps
	}
	assert.Equal(t, 9999, bpsTotal)
}

func TestStComputePayoutSharesSingle(t *testing.T) {
	shares := stComputePayoutShares([]*StShareEntry{
		{NetworkId: server.NewId(), Coldkey: testStColdkey(1), UsageBytes: 1, Assignments: 1, Confirmations: 1},
	}, 1)
	assert.Equal(t, 1, len(shares))
	assert.Equal(t, 10000, shares[0].ShareBps)
}

func TestStComputePayoutSharesSkips(t *testing.T) {
	// no entries -> no shares
	assert.Equal(t, 0, len(stComputePayoutShares(nil, 8)))

	// zero usage, zero confirmations, and confirmations without assignments
	// (clamped to assignments) all produce no leaf
	shares := stComputePayoutShares([]*StShareEntry{
		{NetworkId: server.NewId(), Coldkey: testStColdkey(1), UsageBytes: 0, Assignments: 10, Confirmations: 10},
		{NetworkId: server.NewId(), Coldkey: testStColdkey(2), UsageBytes: 1000, Assignments: 10, Confirmations: 0},
		{NetworkId: server.NewId(), Coldkey: testStColdkey(3), UsageBytes: 1000, Assignments: 0, Confirmations: 5},
	}, 8)
	assert.Equal(t, 0, len(shares))

	// confirmations can never exceed assignments (defensive clamp): 50/5 at
	// a_min=1 counts as 5/5, not 10x
	oneColdkey := testStColdkey(4)
	twoColdkey := testStColdkey(5)
	shares = stComputePayoutShares([]*StShareEntry{
		{NetworkId: server.NewId(), Coldkey: oneColdkey, UsageBytes: 1000, Assignments: 5, Confirmations: 50},
		{NetworkId: server.NewId(), Coldkey: twoColdkey, UsageBytes: 1000, Assignments: 5, Confirmations: 5},
	}, 1)
	assert.Equal(t, 2, len(shares))
	assert.Equal(t, 5000, shares[0].ShareBps)
	assert.Equal(t, 5000, shares[1].ShareBps)

	// a sub-bps weight floors to zero and is dropped (the contract rejects
	// shareBps == 0 claims)
	shares = stComputePayoutShares([]*StShareEntry{
		{NetworkId: server.NewId(), Coldkey: testStColdkey(6), UsageBytes: 1, Assignments: 1, Confirmations: 1},
		{NetworkId: server.NewId(), Coldkey: testStColdkey(7), UsageBytes: 100000, Assignments: 1, Confirmations: 1},
	}, 1)
	assert.Equal(t, 1, len(shares))
	assert.Equal(t, testStColdkey(7), shares[0].Coldkey)
	assert.Equal(t, 9999, shares[0].ShareBps)
}

func TestStComputePayoutSharesDeterministic(t *testing.T) {
	// the output is canonical (ascending coldkey bytes) regardless of the
	// input order
	idA := server.NewId()
	idB := server.NewId()
	idC := server.NewId()
	forward := []*StShareEntry{
		{NetworkId: idA, Coldkey: testStColdkey(3), UsageBytes: 700, Assignments: 20, Confirmations: 17},
		{NetworkId: idB, Coldkey: testStColdkey(1), UsageBytes: 500, Assignments: 4, Confirmations: 3},
		{NetworkId: idC, Coldkey: testStColdkey(2), UsageBytes: 900, Assignments: 12, Confirmations: 12},
	}
	reversed := []*StShareEntry{forward[2], forward[1], forward[0]}

	forwardShares := stComputePayoutShares(forward, 8)
	reversedShares := stComputePayoutShares(reversed, 8)

	assert.Equal(t, len(forwardShares), len(reversedShares))
	bpsTotal := 0
	for i := range forwardShares {
		assert.Equal(t, forwardShares[i].Coldkey, reversedShares[i].Coldkey)
		assert.Equal(t, forwardShares[i].ShareBps, reversedShares[i].ShareBps)
		assert.Equal(t, forwardShares[i].NetworkId, reversedShares[i].NetworkId)
		bpsTotal += forwardShares[i].ShareBps
	}
	assert.Equal(t, true, bpsTotal <= 10000)
}

func TestStBuildShareEntriesExcludesHeadBound(t *testing.T) {
	// three provider networks, each a distinct coldkey served by one verify
	// client; the middle client is promoted to the head tier (its ckey has an
	// active head binding) and must contribute nothing to any pool.
	netA, netB, netC := server.NewId(), server.NewId(), server.NewId()
	clientA, clientB, clientC := server.NewId(), server.NewId(), server.NewId()
	coldkeyA, coldkeyB, coldkeyC := testStColdkey(1), testStColdkey(2), testStColdkey(3)
	promotedCkey := [32]byte{0xAA, 0xBB}

	usages := []*model.StNetworkUsage{
		{NetworkId: netA, PayoutByteCount: 1000},
		{NetworkId: netB, PayoutByteCount: 1000},
		{NetworkId: netC, PayoutByteCount: 1000},
	}
	reliabilities := []*model.StClientReliability{
		{ClientId: clientA, NetworkId: netA, Assignments: 10, Confirmations: 10},
		{ClientId: clientB, NetworkId: netB, Assignments: 10, Confirmations: 10},
		{ClientId: clientC, NetworkId: netC, Assignments: 10, Confirmations: 10},
	}
	wallets := map[server.Id][32]byte{netA: coldkeyA, netB: coldkeyB, netC: coldkeyC}
	clientCkeys := map[server.Id][32]byte{
		clientA: {0x01},
		clientB: promotedCkey, // promoted to the head tier
		clientC: {0x03},
	}
	headBoundCkeys := map[[32]byte]bool{promotedCkey: true}

	// the promoted network is dropped before the per-coldkey aggregation
	entries := stBuildShareEntries(usages, reliabilities, wallets, clientCkeys, headBoundCkeys)
	assert.Equal(t, 2, len(entries))
	for _, entry := range entries {
		assert.NotEqual(t, netB, entry.NetworkId)
		assert.NotEqual(t, coldkeyB, entry.Coldkey)
	}

	// the promoted coldkey earns no leaf; the remaining two split the whole
	// pool (usage is redistributed, not burned) and Σ shares stays <= 10000
	shares := stComputePayoutShares(entries, 1)
	assert.Equal(t, 2, len(shares))
	bpsTotal := 0
	for _, share := range shares {
		assert.NotEqual(t, coldkeyB, share.Coldkey)
		bpsTotal += share.ShareBps
	}
	assert.Equal(t, 5000, shares[0].ShareBps)
	assert.Equal(t, 5000, shares[1].ShareBps)
	assert.Equal(t, true, bpsTotal <= 10000)

	// control: with no active head bindings every network contributes, and a
	// nil ckey map is safe
	allEntries := stBuildShareEntries(usages, reliabilities, wallets, nil, nil)
	assert.Equal(t, 3, len(allEntries))
	allShares := stComputePayoutShares(allEntries, 1)
	allTotal := 0
	for _, share := range allShares {
		allTotal += share.ShareBps
	}
	assert.Equal(t, 3, len(allShares))
	assert.Equal(t, true, allTotal <= 10000)
}

func TestStDepositSizeRao(t *testing.T) {
	gib := int64(1) << 30

	// zero usage, zero rate, or a zero cap all disable the deposit
	assert.Equal(t, "0", stDepositSizeRao(0, 1000, 1000000).String())
	assert.Equal(t, "0", stDepositSizeRao(gib, 0, 1000000).String())
	assert.Equal(t, "0", stDepositSizeRao(gib, 1000, 0).String())

	// rate applies per GiB, flooring sub-rao remainders
	assert.Equal(t, "1000", stDepositSizeRao(gib, 1000, 1000000).String())
	assert.Equal(t, "4", stDepositSizeRao(3*(gib/2), 3, 1000000).String())

	// the per-epoch cap is the custody blast-radius control (D-3): the
	// deposit never exceeds it even when usage says otherwise
	assert.Equal(t, "500", stDepositSizeRao(10*gib, 100, 500).String())

	// usage × rate beyond uint64 still clamps exactly to the cap
	assert.Equal(t, "12345", stDepositSizeRao(math.MaxInt64, math.MaxUint64, 12345).String())
}

func TestStEstimateBlockTime(t *testing.T) {
	headTime := time.Date(2026, time.July, 2, 12, 0, 0, 0, time.UTC)
	headBlock := uint64(1000)

	// future and past blocks offset from the head anchor at ~12s/block
	assert.Equal(t, headTime.Add(1200*time.Second), stEstimateBlockTime(headBlock, headTime, 1100, 12))
	assert.Equal(t, headTime.Add(-1200*time.Second), stEstimateBlockTime(headBlock, headTime, 900, 12))
	assert.Equal(t, headTime, stEstimateBlockTime(headBlock, headTime, 1000, 12))

	// a non-positive cadence falls back to the ~12s default
	assert.Equal(t, headTime.Add(120*time.Second), stEstimateBlockTime(headBlock, headTime, 1010, 0))
}

func TestStBuildPayoutTreeProofs(t *testing.T) {
	// leaves in LeafIndex order, as GetStPayoutLeaves returns them
	shareBps := []int{4000, 3000, 1500, 1000, 400}
	leaves := make([]*model.StPayoutLeaf, len(shareBps))
	for i, bps := range shareBps {
		leaves[i] = &model.StPayoutLeaf{
			Epoch:     7,
			NoId:      1,
			NetworkId: server.NewId(),
			Coldkey:   testStColdkey(byte(i + 1)),
			ShareBps:  bps,
			LeafIndex: i,
		}
	}
	tree, err := stBuildPayoutTree(leaves)
	assert.Equal(t, nil, err)
	root := tree.Root()
	assert.NotEqual(t, [32]byte{}, root)

	for _, leaf := range leaves {
		proof, err := tree.ProofAt(leaf.LeafIndex)
		assert.Equal(t, nil, err)
		hashedLeaf := merkle.PayoutLeaf(leaf.Coldkey, big.NewInt(int64(leaf.ShareBps)))
		assert.Equal(t, true, merkle.Verify(root, hashedLeaf, proof))
		// a tampered share must not verify
		tamperedLeaf := merkle.PayoutLeaf(leaf.Coldkey, big.NewInt(int64(leaf.ShareBps)+1))
		assert.Equal(t, false, merkle.Verify(root, tamperedLeaf, proof))
	}
}

func TestStBuildPayoutTreeSingleLeaf(t *testing.T) {
	leaf := &model.StPayoutLeaf{
		Epoch:     7,
		NoId:      1,
		NetworkId: server.NewId(),
		Coldkey:   testStColdkey(1),
		ShareBps:  10000,
		LeafIndex: 0,
	}
	tree, err := stBuildPayoutTree([]*model.StPayoutLeaf{leaf})
	assert.Equal(t, nil, err)

	// a single-leaf tree's root is the leaf hash and its proof is empty
	hashedLeaf := merkle.PayoutLeaf(leaf.Coldkey, big.NewInt(int64(leaf.ShareBps)))
	assert.Equal(t, [32]byte(hashedLeaf), tree.Root())
	proof, err := tree.ProofAt(0)
	assert.Equal(t, nil, err)
	assert.Equal(t, 0, len(proof))
	assert.Equal(t, true, merkle.Verify(tree.Root(), hashedLeaf, proof))

	// an empty leaf set can never produce a committable root
	_, err = stBuildPayoutTree(nil)
	assert.NotEqual(t, nil, err)
}
