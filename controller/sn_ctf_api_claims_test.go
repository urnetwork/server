package controller

// sn_ctf_api_claims_test.go — how GET /sn/ctf/client reads vault2's claimed
// (sn_ctf_api_claims.go): at one finalized block per answer, read once, in
// bounded batches, each value kept for snCtfClaimedTtl; and that a chain that
// is down or slow never holds the answer past snCtfClaimsTimeout, nor leaves a
// read running after it. Nothing here reads a database.

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/holiman/uint256"

	"github.com/urnetwork/server"
)

// snCtfClaimedChain is vault2 as the claim states read it: claimed(pool, i) is
// i·10 + 7. It records each read, and can refuse or hold one until its
// context ends.
type snCtfClaimedChain struct {
	StLedgerV2Client
	head uint64

	lock           sync.Mutex
	finalizedReads int
	batches        [][]uint64
	blocks         []uint64
	// refuse the next finalized read, or the batch with this number (from 1)
	refuseFinalized bool
	refuseBatch     int
	// hold every read until its context ends
	hold bool
	// reads in progress
	inFlight atomic.Int32
}

func (self *snCtfClaimedChain) wait(ctx context.Context) error {
	self.inFlight.Add(1)
	defer self.inFlight.Add(-1)
	if self.hold {
		return snCtfHoldUntilDone(ctx)
	}
	return nil
}

// snCtfHoldUntilDone holds a read until its context ends, or three seconds,
// so that a read without a deadline fails a test rather than hang it.
func snCtfHoldUntilDone(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(3 * time.Second):
		return errors.New("held for three seconds without a deadline")
	}
}

func (self *snCtfClaimedChain) LedgerV2FinalizedBlock(ctx context.Context) (uint64, error) {
	if err := self.wait(ctx); err != nil {
		return 0, err
	}
	self.lock.Lock()
	defer self.lock.Unlock()
	self.finalizedReads += 1
	if self.refuseFinalized {
		self.refuseFinalized = false
		return 0, errors.New("the finalized head cannot be read")
	}
	return self.head, nil
}

func (self *snCtfClaimedChain) LedgerV2ClaimedAt(ctx context.Context, noId uint64, block uint64, indexes []uint64) ([]uint256.Int, error) {
	if err := self.wait(ctx); err != nil {
		return nil, err
	}
	self.lock.Lock()
	defer self.lock.Unlock()
	if len(self.batches)+1 == self.refuseBatch {
		self.refuseBatch = 0
		return nil, errors.New("rpc down")
	}
	self.batches = append(self.batches, slices.Clone(indexes))
	self.blocks = append(self.blocks, block)
	values := make([]uint256.Int, len(indexes))
	for i, index := range indexes {
		values[i].SetUint64(index*10 + 7)
	}
	return values, nil
}

// reads returns and forgets the reads made: the finalized reads, each batch's
// size and the blocks they were read at.
func (self *snCtfClaimedChain) reads() (int, []int, []uint64) {
	self.lock.Lock()
	defer self.lock.Unlock()
	sizes := []int{}
	for _, batch := range self.batches {
		sizes = append(sizes, len(batch))
	}
	finalized, blocks := self.finalizedReads, self.blocks
	self.finalizedReads, self.batches, self.blocks = 0, nil, nil
	return finalized, sizes, blocks
}

// withSnCtfClaimedChain reads vault2 from chain with an empty cache, on a
// clock the test moves.
func withSnCtfClaimedChain(t testing.TB, chain *snCtfClaimedChain) *time.Time {
	t.Helper()
	SetStLedgerV2Client(chain)
	snCtfResetClaimedCache()
	now := time.Date(2026, 12, 7, 12, 0, 0, 0, time.UTC)
	clock := snCtfClaimedNow
	snCtfClaimedNow = func() time.Time { return now }
	t.Cleanup(func() {
		SetStLedgerV2Client(nil)
		snCtfResetClaimedCache()
		snCtfClaimedNow = clock
	})
	return &now
}

func snCtfClaimedTestIndexes(from uint64, to uint64) []uint64 {
	indexes := []uint64{}
	for index := from; index < to; index += 1 {
		indexes = append(indexes, index)
	}
	return indexes
}

func requireSnCtfClaimed(t testing.TB, what string, claimed map[uint64]uint256.Int, indexes []uint64) {
	t.Helper()
	if len(claimed) != len(indexes) {
		t.Fatalf("%s: %d values, want %d", what, len(claimed), len(indexes))
	}
	for _, index := range indexes {
		if value, ok := claimed[index]; !ok || value.Uint64() != index*10+7 {
			t.Fatalf("%s: claimed of %d is %v (%t), want %d", what, index, value.Dec(), ok, index*10+7)
		}
	}
}

// One answer reads the finalized block once and every account at it, in
// batches of at most stMaximumEVMRPCBatchCalls, the indexes in their order. A
// value is answered as read for snCtfClaimedTtl, so an answer reads only the
// accounts no answer read lately; past it, it is read again.
func TestSnCtfClaimedOfReadsOneBlockInBatchesAndKeepsThem(t *testing.T) {
	chain := &snCtfClaimedChain{head: 9_702_100}
	now := withSnCtfClaimedChain(t, chain)
	cfg := ledgerV2TestConfig()
	pool := uint64(1<<32 + 1)
	ctx := context.Background()

	first := snCtfClaimedTestIndexes(0, 120)
	requireSnCtfClaimed(t, "120 accounts", snCtfClaimedOf(ctx, cfg, pool, first), first)
	finalized, sizes, blocks := chain.reads()
	if finalized != 1 || !slices.Equal(sizes, []int{50, 50, 20}) || !slices.Equal(blocks, []uint64{9_702_100, 9_702_100, 9_702_100}) {
		t.Fatalf("120 accounts: %d finalized reads, batches %v at %v; want one, 50, 50 and 20 at its block", finalized, sizes, blocks)
	}

	// 25 s later the 120 are answered as read, and only 10 new ones are read,
	// at the block the finalized head is at now
	*now = now.Add(25 * time.Second)
	chain.head = 9_702_102
	more := snCtfClaimedTestIndexes(0, 130)
	requireSnCtfClaimed(t, "130 accounts, 120 kept", snCtfClaimedOf(ctx, cfg, pool, slices.Concat(more, []uint64{3, 125})), more)
	if finalized, sizes, blocks := chain.reads(); finalized != 1 || !slices.Equal(sizes, []int{10}) || !slices.Equal(blocks, []uint64{9_702_102}) {
		t.Fatalf("10 new accounts: %d finalized reads, batches %v at %v", finalized, sizes, blocks)
	}
	// nothing new: no chain read at all
	snCtfClaimedOf(ctx, cfg, pool, first)
	if finalized, sizes, _ := chain.reads(); finalized != 0 || len(sizes) != 0 {
		t.Fatalf("every account kept: %d finalized reads, batches %v", finalized, sizes)
	}
	// the first 120 expire at 30 s, the last 10 not yet
	*now = now.Add(5 * time.Second)
	requireSnCtfClaimed(t, "120 expired", snCtfClaimedOf(ctx, cfg, pool, more), more)
	if finalized, sizes, _ := chain.reads(); finalized != 1 || !slices.Equal(sizes, []int{50, 50, 20}) {
		t.Fatalf("120 expired: %d finalized reads, batches %v", finalized, sizes)
	}

	// another pool, or another vault, is another account
	other := ledgerV2TestConfig()
	other.VaultV2[19] ^= 0xff
	snCtfClaimedOf(ctx, other, pool, []uint64{1})
	snCtfClaimedOf(ctx, cfg, pool+1, []uint64{1})
	if finalized, sizes, _ := chain.reads(); finalized != 2 || !slices.Equal(sizes, []int{1, 1}) {
		t.Fatalf("other pools and vaults: %d finalized reads, batches %v", finalized, sizes)
	}
}

// A refused read leaves out only what it would have read: the finalized head
// refused reads nothing; a refused batch keeps the batches read before it,
// which the next answer does not read again.
func TestSnCtfClaimedOfKeepsWhatWasRead(t *testing.T) {
	chain := &snCtfClaimedChain{head: 9_702_100}
	withSnCtfClaimedChain(t, chain)
	cfg := ledgerV2TestConfig()
	pool := uint64(1<<32 + 1)
	ctx := context.Background()
	indexes := snCtfClaimedTestIndexes(0, 120)

	chain.refuseFinalized = true
	if claimed := snCtfClaimedOf(ctx, cfg, pool, indexes); len(claimed) != 0 {
		t.Fatalf("%d values without a finalized head", len(claimed))
	}
	if finalized, sizes, _ := chain.reads(); finalized != 1 || len(sizes) != 0 {
		t.Fatalf("without a finalized head: %d finalized reads, batches %v", finalized, sizes)
	}

	chain.refuseBatch = 2
	requireSnCtfClaimed(t, "the second batch refused", snCtfClaimedOf(ctx, cfg, pool, indexes), indexes[:50])
	if _, sizes, _ := chain.reads(); !slices.Equal(sizes, []int{50}) {
		t.Fatalf("the second batch refused: batches %v", sizes)
	}
	requireSnCtfClaimed(t, "the rest", snCtfClaimedOf(ctx, cfg, pool, indexes), indexes)
	if _, sizes, _ := chain.reads(); !slices.Equal(sizes, []int{50, 20}) {
		t.Fatalf("the rest: batches %v, want only the 70 not read", sizes)
	}

	// with no chain client configured, nothing is read and nothing fails
	SetStLedgerV2Client(nil)
	snCtfResetClaimedCache()
	if claimed := snCtfClaimedOf(ctx, cfg, pool, indexes); len(claimed) != 0 {
		t.Fatalf("%d values without a chain", len(claimed))
	}
}

// A chain that holds a read gets nothing read once the context ends, and the
// call returns then, with no read left running: the finalized head held, and
// a batch held after the head answered.
func TestSnCtfClaimedOfEndsWithItsContext(t *testing.T) {
	cfg := ledgerV2TestConfig()
	pool := uint64(1<<32 + 1)
	for name, clientOf := range map[string]func(*snCtfClaimedChain) StLedgerV2Client{
		"the finalized head": func(chain *snCtfClaimedChain) StLedgerV2Client {
			chain.hold = true
			return chain
		},
		"a batch": func(chain *snCtfClaimedChain) StLedgerV2Client {
			return &snCtfHeldBatches{snCtfClaimedChain: chain}
		},
	} {
		chain := &snCtfClaimedChain{head: 9_702_100}
		withSnCtfClaimedChain(t, chain)
		SetStLedgerV2Client(clientOf(chain))
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		start := time.Now()
		claimed := snCtfClaimedOf(ctx, cfg, pool, snCtfClaimedTestIndexes(0, 10))
		elapsed := time.Since(start)
		cancel()
		if len(claimed) != 0 || elapsed < 50*time.Millisecond || 2*time.Second < elapsed {
			t.Fatalf("%s held: %d values after %s, want none after 50ms", name, len(claimed), elapsed)
		}
		if inFlight := chain.inFlight.Load(); inFlight != 0 {
			t.Fatalf("%s held: %d reads still running after the call returned", name, inFlight)
		}
	}
}

// snCtfHeldBatches is the chain with every batch held until its context ends.
type snCtfHeldBatches struct {
	*snCtfClaimedChain
}

func (self *snCtfHeldBatches) LedgerV2ClaimedAt(ctx context.Context, noId uint64, block uint64, indexes []uint64) ([]uint256.Int, error) {
	self.inFlight.Add(1)
	defer self.inFlight.Add(-1)
	return nil, snCtfHoldUntilDone(ctx)
}

// snCtfHeldSource is a source whose claim states never come: AccountClaims
// waits for its context to end, and records the time it was given. It gives
// up after three seconds, so that an answer without a deadline fails the test
// rather than hang it.
type snCtfHeldSource struct {
	snCtfTestSource
	deadline time.Duration
	reading  atomic.Int32
}

func (self *snCtfHeldSource) AccountClaims(ctx context.Context, _ *snCtfView, needs []*snCtfClaimNeed) map[[32]byte]*snCtfAccountClaims {
	self.reading.Add(1)
	defer self.reading.Add(-1)
	if deadline, ok := ctx.Deadline(); ok {
		self.deadline = time.Until(deadline)
	}
	select {
	case <-ctx.Done():
	case <-time.After(3 * time.Second):
	}
	return map[[32]byte]*snCtfAccountClaims{}
}

// A chain that never answers holds GET /sn/ctf/client's answer only as long
// as snCtfClaimsTimeout: the answer then comes, every win whose claim state
// needs a read null and the others as they are, and the read has ended.
func TestSnCtfClientAnswerGivesTheClaimStatesTheirTime(t *testing.T) {
	timeout := snCtfClaimsTimeout
	snCtfClaimsTimeout = 100 * time.Millisecond
	t.Cleanup(func() { snCtfClaimsTimeout = timeout })
	if timeout != 5*time.Second {
		t.Fatalf("the claim states' time is %s, want about five seconds", timeout)
	}

	view := snCtfWinsTestView()
	client := server.NewId()
	coldkey := [32]byte{0xa1}
	wins := []*snCtfWin{
		snCtfWinsTestWin(8, snCtfRoleProvider, "is", 1, client, coldkey, 100, 0, false),
		snCtfWinsTestWin(10, snCtfRoleProvider, "de", 2, client, coldkey, 100, 0, false),
		snCtfWinsTestWin(11, snCtfRoleProvider, "is", 1, client, coldkey, 100, 30, true),
		snCtfWinsTestWin(12, snCtfRoleProvider, "is", 1, client, coldkey, 100, 0, false),
	}
	source := &snCtfHeldSource{snCtfTestSource: snCtfTestSource{view: view, wins: wins}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	start := time.Now()
	result, err := snCtfClientAnswer(ctx, source, view, snCtfClientScope{NetworkId: server.NewId()}, 8)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if elapsed < 100*time.Millisecond || 2*time.Second < elapsed {
		t.Fatalf("the answer came after %s, want the claim states' 100ms", elapsed)
	}
	if deadline := source.deadline; deadline <= 0 || 100*time.Millisecond < deadline {
		t.Fatalf("the claim states were given %s", deadline)
	}
	if reading := source.reading.Load(); reading != 0 {
		t.Fatalf("%d claim state reads still running after the answer", reading)
	}
	requireSnCtfEqual(t, "claimed", snCtfWinsTestClaimed(result.Wins), map[string]string{
		"8 provider is 1": "null", "10 provider de 2": "null", "11 provider is 1": "false", "12 provider is 1": "false",
	})
	// the request's own end ends the read too, at once
	cancel()
	start = time.Now()
	if _, err := snCtfClientAnswer(ctx, source, view, snCtfClientScope{NetworkId: server.NewId()}, 8); err != nil || 50*time.Millisecond < time.Since(start) {
		t.Fatalf("a canceled request: %v after %s", err, time.Since(start))
	}
}
