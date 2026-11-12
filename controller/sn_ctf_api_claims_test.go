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
// i·10 + 7, plus grown once the test has the accounts claimed further. It
// records each read, and can refuse or hold one until its context ends.
type snCtfClaimedChain struct {
	StLedgerV2Client
	head  uint64
	grown uint64

	lock           sync.Mutex
	finalizedReads int
	batches        [][]uint64
	blocks         []uint64
	// refuse the next finalized read, or the batch with this number (from 1)
	refuseFinalized bool
	refuseBatch     int
	// hold every read until its context ends
	hold bool
	// called as each batch is read, as a slow read takes time
	onBatch func()
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
	if self.onBatch != nil {
		self.onBatch()
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
		values[i].SetUint64(index*10 + 7 + self.grown)
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
	requireSnCtfClaimedGrown(t, what, claimed, indexes, 0)
}

// requireSnCtfClaimedGrown requires the values the chain answered while its
// accounts were claimed grown further.
func requireSnCtfClaimedGrown(t testing.TB, what string, claimed map[uint64]uint256.Int, indexes []uint64, grown uint64) {
	t.Helper()
	if len(claimed) != len(indexes) {
		t.Fatalf("%s: %d values, want %d", what, len(claimed), len(indexes))
	}
	for _, index := range indexes {
		if value, ok := claimed[index]; !ok || value.Uint64() != index*10+7+grown {
			t.Fatalf("%s: claimed of %d is %v (%t), want %d", what, index, value.Dec(), ok, index*10+7+grown)
		}
	}
}

// One answer reads the finalized block once and every account at it, in
// batches of at most stMaximumEVMRPCBatchCalls, the indexes in their order. A
// value is answered as read for snCtfClaimedTtl from when its read began, so
// an answer reads only the accounts no answer read lately; past it, it is read
// again. The time is about 30 s, but under the 30 s between the apps' reads
// after a claim, so that each of those reads the chain again.
func TestSnCtfClaimedOfReadsOneBlockInBatchesAndKeepsThem(t *testing.T) {
	if snCtfClaimedTtl < 20*time.Second || 30*time.Second <= snCtfClaimedTtl {
		t.Fatalf("claimed is kept %s, want about 30 s, under the apps' 30 s between reads after a claim", snCtfClaimedTtl)
	}
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

	// 5 s before they expire the 120 are answered as read, and only 10 new
	// ones are read, at the block the finalized head is at now
	*now = now.Add(snCtfClaimedTtl - 5*time.Second)
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
	// the first 120 expire, the last 10 not yet
	*now = now.Add(5 * time.Second)
	requireSnCtfClaimed(t, "120 expired", snCtfClaimedOf(ctx, cfg, pool, more), more)
	if finalized, sizes, _ := chain.reads(); finalized != 1 || !slices.Equal(sizes, []int{50, 50, 20}) {
		t.Fatalf("120 expired: %d finalized reads, batches %v", finalized, sizes)
	}

	// a value is kept from when its read began: one whose batch took 4 s is
	// read again snCtfClaimedTtl after the read began, though it finished 4 s
	// later, so a slow read is never kept longer than a quick one
	snCtfResetClaimedCache()
	chain.onBatch = func() { *now = now.Add(4 * time.Second) }
	snCtfClaimedOf(ctx, cfg, pool, []uint64{1})
	chain.onBatch = nil
	chain.reads()
	*now = now.Add(snCtfClaimedTtl - 4*time.Second)
	snCtfClaimedOf(ctx, cfg, pool, []uint64{1})
	if _, sizes, _ := chain.reads(); !slices.Equal(sizes, []int{1}) {
		t.Fatalf("a slow read %s after it began: batches %v, want it read again", snCtfClaimedTtl, sizes)
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

// Stale if the read fails: once an account's claimed was read, an answer whose
// fresh read of it does not finish in time, or fails, answers the last value
// read, however old, so a slow chain never takes a won line away; claimed only
// grows, so that value errs only by reading a claim a little late. A first read
// that does not finish in time leaves the account out, its wins null. A value
// answered this way is not fresh: the next answer reads the chain again.
func TestSnCtfClaimedOfAnswersTheLastValueWhenAReadFails(t *testing.T) {
	chain := &snCtfClaimedChain{head: 9_702_100}
	now := withSnCtfClaimedChain(t, chain)
	cfg := ledgerV2TestConfig()
	pool := uint64(1<<32 + 1)
	indexes := snCtfClaimedTestIndexes(0, 120)
	timedOut := func(indexes []uint64) map[uint64]uint256.Int {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		return snCtfClaimedOf(ctx, cfg, pool, indexes)
	}

	// a first read that times out: nothing to answer
	chain.hold = true
	if claimed := timedOut(indexes); len(claimed) != 0 {
		t.Fatalf("a first read that timed out answered %d values", len(claimed))
	}
	chain.hold = false
	requireSnCtfClaimed(t, "read", snCtfClaimedOf(context.Background(), cfg, pool, indexes), indexes)
	chain.reads()

	// past snCtfClaimedTtl, while the accounts were claimed further, each way a
	// fresh read can fail answers the values read before
	*now = now.Add(2 * snCtfClaimedTtl)
	chain.grown = 1_000
	chain.hold = true
	requireSnCtfClaimedGrown(t, "a read that timed out", timedOut(indexes), indexes, 0)
	chain.hold = false
	chain.refuseFinalized = true
	requireSnCtfClaimedGrown(t, "the finalized head refused", snCtfClaimedOf(context.Background(), cfg, pool, indexes), indexes, 0)
	SetStLedgerV2Client(nil)
	requireSnCtfClaimedGrown(t, "no chain client", snCtfClaimedOf(context.Background(), cfg, pool, indexes), indexes, 0)
	// the last values are those accounts' alone: the same indexes of another
	// vault, pool or deployment have none
	otherVault, otherDeployment := ledgerV2TestConfig(), ledgerV2TestConfig()
	otherVault.VaultV2[19] ^= 0xff
	otherDeployment.ContractAddress[19] ^= 0xff
	if otherDeployment.DeploymentKey() == cfg.DeploymentKey() {
		t.Fatal("the other deployment has the same key")
	}
	for name, other := range map[string]map[uint64]uint256.Int{
		"another vault":      snCtfClaimedOf(context.Background(), otherVault, pool, indexes),
		"another pool":       snCtfClaimedOf(context.Background(), cfg, pool+1, indexes),
		"another deployment": snCtfClaimedOf(context.Background(), otherDeployment, pool, indexes),
	} {
		if len(other) != 0 {
			t.Fatalf("%s: %d values answered as last read", name, len(other))
		}
	}
	SetStLedgerV2Client(chain)
	chain.reads()
	// the second batch refused: the first is read fresh, the rest answered as
	// read before
	chain.refuseBatch = 2
	claimed := snCtfClaimedOf(context.Background(), cfg, pool, indexes)
	if len(claimed) != len(indexes) {
		t.Fatalf("the second batch refused: %d values, want %d", len(claimed), len(indexes))
	}
	requireSnCtfClaimedGrown(t, "the first batch", snCtfClaimedOnly(claimed, indexes[:50]), indexes[:50], 1_000)
	requireSnCtfClaimedGrown(t, "the batches not read", snCtfClaimedOnly(claimed, indexes[50:]), indexes[50:], 0)
	// those answered as read before were not read fresh: the next answer reads
	// them, and only them
	chain.reads()
	requireSnCtfClaimedGrown(t, "read again", snCtfClaimedOf(context.Background(), cfg, pool, indexes), indexes, 1_000)
	if _, sizes, _ := chain.reads(); !slices.Equal(sizes, []int{50, 20}) {
		t.Fatalf("read again: batches %v, want the 70 answered as read before", sizes)
	}
	// an account never read on this instance has no last value
	*now = now.Add(2 * snCtfClaimedTtl)
	chain.hold = true
	claimed = timedOut(slices.Concat(indexes[:1], snCtfClaimedTestIndexes(120, 130)))
	requireSnCtfClaimedGrown(t, "a read that timed out, one account read before", claimed, indexes[:1], 1_000)
}

// Of two reads of one account the cache keeps the larger claimed and the later
// time: claimed only grows, so a read that began earlier and finished later, or
// one at an older finalized block, never lowers the value kept, which can be
// answered long after while reads fail.
func TestSnCtfClaimedKeepNeverLowersAValue(t *testing.T) {
	snCtfResetClaimedCache()
	t.Cleanup(snCtfResetClaimedCache)
	cfg := ledgerV2TestConfig()
	key := snCtfClaimedKey{deploymentKey: cfg.DeploymentKey(), vault: cfg.VaultV2, poolId: 1<<32 + 1, index: 3}
	at := time.Date(2026, 12, 7, 12, 0, 0, 0, time.UTC)
	cache := &snCtfClaimedCache
	cache.lock.Lock()
	defer cache.lock.Unlock()
	kept := func(what string, claimed uint64, readTime time.Time) {
		t.Helper()
		if read := snCtfClaimedLast(key); read == nil || read.claimed.Uint64() != claimed || !read.readTime.Equal(readTime) {
			t.Fatalf("%s: kept %+v, want %d read at %s", what, read, claimed, readTime)
		}
	}
	snCtfClaimedKeep(key, *uint256.NewInt(1_600), at.Add(2*time.Second))
	snCtfClaimedKeep(key, *uint256.NewInt(600), at)
	kept("an older read finishing later", 1_600, at.Add(2*time.Second))
	snCtfClaimedKeep(key, *uint256.NewInt(2_000), at.Add(3*time.Second))
	kept("a newer read", 2_000, at.Add(3*time.Second))
	snCtfClaimedKeep(key, *uint256.NewInt(2_500), at.Add(time.Second))
	kept("a larger value read earlier, at a later block", 2_500, at.Add(3*time.Second))
}

// Answers that run at once, their reads failing now and then while the clock
// moves past the time a value is fresh, keep the cache whole: each key it maps
// is in its recency list, once, and it holds no more than its limit.
func TestSnCtfClaimedOfKeepsTheCacheWholeUnderConcurrentAnswers(t *testing.T) {
	limit := snCtfClaimedCacheLimit
	snCtfClaimedCacheLimit = 40
	t.Cleanup(func() { snCtfClaimedCacheLimit = limit })
	chain := &snCtfFlakyClaimedChain{snCtfClaimedChain: &snCtfClaimedChain{head: 9_702_100}}
	withSnCtfClaimedChain(t, chain.snCtfClaimedChain)
	SetStLedgerV2Client(chain)
	var elapsed atomic.Int64
	start := time.Date(2026, 12, 7, 12, 0, 0, 0, time.UTC)
	snCtfClaimedNow = func() time.Time { return start.Add(time.Duration(elapsed.Load())) }
	cfg := ledgerV2TestConfig()
	pool := uint64(1<<32 + 1)
	var group sync.WaitGroup
	for g := range 8 {
		group.Add(1)
		go func() {
			defer group.Done()
			for i := range 25 {
				first := uint64((g*7 + i) % 60)
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				snCtfClaimedOf(ctx, cfg, pool, snCtfClaimedTestIndexes(first, first+10))
				cancel()
				elapsed.Add(int64(snCtfClaimedTtl / 8))
			}
		}()
	}
	group.Wait()
	cache := &snCtfClaimedCache
	cache.lock.Lock()
	defer cache.lock.Unlock()
	if len(cache.reads) != cache.recency.Len() || snCtfClaimedCacheLimit < cache.recency.Len() || cache.recency.Len() == 0 {
		t.Fatalf("%d keys mapped, %d in the recency list, limit %d", len(cache.reads), cache.recency.Len(), snCtfClaimedCacheLimit)
	}
	for element := cache.recency.Front(); element != nil; element = element.Next() {
		if read := element.Value.(*snCtfClaimedRead); cache.reads[read.key] != element {
			t.Fatalf("index %d's element is not the one its key maps", read.key.index)
		}
	}
}

// snCtfFlakyClaimedChain refuses every third batch of claimed.
type snCtfFlakyClaimedChain struct {
	*snCtfClaimedChain
	calls atomic.Int64
}

func (self *snCtfFlakyClaimedChain) LedgerV2ClaimedAt(ctx context.Context, noId uint64, block uint64, indexes []uint64) ([]uint256.Int, error) {
	if self.calls.Add(1)%3 == 0 {
		return nil, errors.New("rpc down")
	}
	return self.snCtfClaimedChain.LedgerV2ClaimedAt(ctx, noId, block, indexes)
}

// snCtfClaimedOnly is the values of claimed at indexes.
func snCtfClaimedOnly(claimed map[uint64]uint256.Int, indexes []uint64) map[uint64]uint256.Int {
	only := map[uint64]uint256.Int{}
	for _, index := range indexes {
		if value, ok := claimed[index]; ok {
			only[index] = value
		}
	}
	return only
}

// The values kept are bounded, the least recently used dropped first: an
// account answered from the cache is used, so it stays; the one used least
// recently is dropped, and with no read since, it has no last value.
func TestSnCtfClaimedOfKeepsTheMostRecentlyUsed(t *testing.T) {
	limit := snCtfClaimedCacheLimit
	if limit < 100_000 {
		t.Fatalf("the cache keeps %d accounts, want a few hundred thousand", limit)
	}
	snCtfClaimedCacheLimit = 3
	t.Cleanup(func() { snCtfClaimedCacheLimit = limit })
	chain := &snCtfClaimedChain{head: 9_702_100}
	now := withSnCtfClaimedChain(t, chain)
	cfg := ledgerV2TestConfig()
	pool := uint64(1<<32 + 1)
	ctx := context.Background()

	snCtfClaimedOf(ctx, cfg, pool, []uint64{0, 1, 2})
	// 0 is answered from the cache, so 1 is now the least recently used
	*now = now.Add(time.Second)
	snCtfClaimedOf(ctx, cfg, pool, []uint64{0})
	snCtfClaimedOf(ctx, cfg, pool, []uint64{3})
	if kept := snCtfClaimedCache.recency.Len(); kept != 3 || len(snCtfClaimedCache.reads) != 3 {
		t.Fatalf("%d values kept, %d indexed, want 3", kept, len(snCtfClaimedCache.reads))
	}
	*now = now.Add(2 * snCtfClaimedTtl)
	chain.refuseFinalized = true
	requireSnCtfClaimed(t, "the kept values", snCtfClaimedOf(ctx, cfg, pool, []uint64{0, 1, 2, 3}), []uint64{0, 2, 3})
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
