package controller

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urfoundation/sn/stabi"

	"github.com/urnetwork/server/model"
)

// the mainnet boundaries as the coordinator proxy
// 0xC18925925E2B7bb9059b7d696b8c92762AE86406 reports them (epochStartBlock and
// epochEndBlock read on snow's archive), with the two mined start times
var (
	testStatsEpoch0Start = time.Unix(1_791_482_892, 0).UTC() // block 9,240,028, 2026-10-08 18:08:12
	testStatsEpoch1Start = time.Unix(1_791_569_340, 0).UTC() // block 9,247,228, 2026-10-09 18:09:00
	// 50,400 blocks at 12 s
	testStatsEpochWeek = 50_400 * 12 * time.Second
)

const (
	testStatsEpoch0StartBlock = 9_240_028
	testStatsEpoch1StartBlock = 9_247_228
	testStatsEpoch2StartBlock = 9_297_628
	testStatsEpoch3StartBlock = 9_348_028
	testStatsEpoch4StartBlock = 9_398_428
)

// The built-in schedule is the coordinator's arithmetic: currentEpoch,
// epochStartBlock and epochEndBlock over policyByIndex(0) and (1).
func TestStatsEpochScheduleMirrorsTheCoordinator(t *testing.T) {
	schedule := statsMainnetEpochSchedule
	if err := schedule.validate(); err != nil {
		t.Fatal(err)
	}
	if epoch, ok := schedule.epochAt(testStatsEpoch0StartBlock - 1); ok {
		t.Fatalf("a block before the coordinator's first policy is epoch %d, want none", epoch)
	}
	for _, c := range []struct {
		block uint64
		epoch uint64
	}{
		{testStatsEpoch0StartBlock, 0},
		{testStatsEpoch1StartBlock - 1, 0},
		{testStatsEpoch1StartBlock, 1},
		{testStatsEpoch2StartBlock - 1, 1},
		{testStatsEpoch2StartBlock, 2},
		{testStatsEpoch3StartBlock - 1, 2},
		{testStatsEpoch3StartBlock, 3},
	} {
		if epoch, ok := schedule.epochAt(c.block); !ok || epoch != c.epoch {
			t.Fatalf("epochAt(%d) = %d, %t, want %d", c.block, epoch, ok, c.epoch)
		}
	}
	for _, c := range []struct {
		epoch uint64
		start uint64
		end   uint64
	}{
		// the 7,200-block bootstrap epoch
		{0, testStatsEpoch0StartBlock, testStatsEpoch1StartBlock},
		{1, testStatsEpoch1StartBlock, testStatsEpoch2StartBlock},
		{2, testStatsEpoch2StartBlock, testStatsEpoch3StartBlock},
		// settlement v2's first epoch
		{3, testStatsEpoch3StartBlock, testStatsEpoch4StartBlock},
	} {
		start, startOk := schedule.startBlock(c.epoch)
		end, endOk := schedule.endBlock(c.epoch)
		if !startOk || !endOk || start != c.start || end != c.end {
			t.Fatalf("epoch %d blocks = [%d, %d), want [%d, %d)", c.epoch, start, end, c.start, c.end)
		}
	}
}

// A schedule read from the chain is held to what schedulePolicy guarantees.
func TestStatsEpochScheduleValidate(t *testing.T) {
	later := append(statsEpochSchedule{}, statsMainnetEpochSchedule...)
	later = append(later, statsEpochPolicy{EffectiveEpoch: 4, EffectiveBlock: testStatsEpoch4StartBlock, EpochBlocks: 25_200})
	if err := later.validate(); err != nil {
		t.Fatalf("a policy scheduled at its epoch's start: %v", err)
	}
	for name, schedule := range map[string]statsEpochSchedule{
		"empty":         {},
		"no length":     {{EffectiveEpoch: 0, EffectiveBlock: 10, EpochBlocks: 0}},
		"not following": {{EffectiveEpoch: 1, EffectiveBlock: 10, EpochBlocks: 5}, {EffectiveEpoch: 1, EffectiveBlock: 15, EpochBlocks: 5}},
		"off boundary":  {{EffectiveEpoch: 0, EffectiveBlock: 10, EpochBlocks: 5}, {EffectiveEpoch: 2, EffectiveBlock: 21, EpochBlocks: 5}},
	} {
		if err := schedule.validate(); err == nil {
			t.Fatalf("%s schedule validated", name)
		}
	}
}

// The built-in clock, before any chain read, rolls 0 -> 1 -> 2 -> 3 at the
// coordinator's boundary blocks, projected at 12 s a block from epoch 1's
// mined start, and uses the two mined start times exactly.
func TestStatsEpochClockBuiltInBoundaries(t *testing.T) {
	clock := newStatsEpochClock()
	week := testStatsEpochWeek
	for _, c := range []struct {
		name          string
		at            time.Time
		epoch         uint64
		start         uint64
		end           uint64
		startTime     time.Time
		endTime       time.Time
		hasPrev       bool
		prevStart     uint64
		prevStartTime time.Time
	}{
		{"before the deploy", testStatsEpoch0Start.Add(-24 * time.Hour), 0, testStatsEpoch0StartBlock, testStatsEpoch1StartBlock, testStatsEpoch0Start, testStatsEpoch1Start, false, 0, time.Time{}},
		{"epoch 0 opens", testStatsEpoch0Start, 0, testStatsEpoch0StartBlock, testStatsEpoch1StartBlock, testStatsEpoch0Start, testStatsEpoch1Start, false, 0, time.Time{}},
		{"epoch 0's last second", testStatsEpoch1Start.Add(-time.Second), 0, testStatsEpoch0StartBlock, testStatsEpoch1StartBlock, testStatsEpoch0Start, testStatsEpoch1Start, false, 0, time.Time{}},
		{"epoch 1 opens", testStatsEpoch1Start, 1, testStatsEpoch1StartBlock, testStatsEpoch2StartBlock, testStatsEpoch1Start, testStatsEpoch1Start.Add(week), true, testStatsEpoch0StartBlock, testStatsEpoch0Start},
		{"epoch 1's last second", testStatsEpoch1Start.Add(week - time.Second), 1, testStatsEpoch1StartBlock, testStatsEpoch2StartBlock, testStatsEpoch1Start, testStatsEpoch1Start.Add(week), true, testStatsEpoch0StartBlock, testStatsEpoch0Start},
		{"epoch 2 opens", testStatsEpoch1Start.Add(week), 2, testStatsEpoch2StartBlock, testStatsEpoch3StartBlock, testStatsEpoch1Start.Add(week), testStatsEpoch1Start.Add(2 * week), true, testStatsEpoch1StartBlock, testStatsEpoch1Start},
		{"epoch 3 opens", testStatsEpoch1Start.Add(2 * week), 3, testStatsEpoch3StartBlock, testStatsEpoch4StartBlock, testStatsEpoch1Start.Add(2 * week), testStatsEpoch1Start.Add(3 * week), true, testStatsEpoch2StartBlock, testStatsEpoch1Start.Add(week)},
	} {
		window := clock.window(c.at)
		if window.Epoch != c.epoch || window.StartBlock != c.start || window.EndBlock != c.end {
			t.Fatalf("%s: epoch %d [%d, %d), want %d [%d, %d)", c.name, window.Epoch, window.StartBlock, window.EndBlock, c.epoch, c.start, c.end)
		}
		if !window.Start.Equal(c.startTime) || !window.End.Equal(c.endTime) {
			t.Fatalf("%s: %s to %s, want %s to %s", c.name, window.Start, window.End, c.startTime, c.endTime)
		}
		if window.HasPrev != c.hasPrev || window.PrevStartBlock != c.prevStart || !window.PrevStart.Equal(c.prevStartTime) {
			t.Fatalf("%s: previous %t %d %s, want %t %d %s", c.name, window.HasPrev, window.PrevStartBlock, window.PrevStart, c.hasPrev, c.prevStart, c.prevStartTime)
		}
	}
}

func TestStatsProjectBlock(t *testing.T) {
	anchor := statsBlockAnchor{Block: 1_000, Time: time.Unix(1_000_000, 0)}
	for _, c := range []struct {
		offset time.Duration
		block  uint64
	}{
		{0, 1_000},
		{11 * time.Second, 1_000},
		{12 * time.Second, 1_001},
		{-time.Second, 999},
		{-12 * time.Second, 999},
		{-13 * time.Second, 998},
	} {
		if block := statsProjectBlock(anchor, anchor.Time.Add(c.offset), 12); block != c.block {
			t.Fatalf("projected block at %s = %d, want %d", c.offset, block, c.block)
		}
	}
	if block := statsProjectBlock(anchor, anchor.Time.Add(-time.Hour), 12); block != 700 {
		t.Fatalf("an hour back = %d, want 700", block)
	}
	if block := statsProjectBlock(statsBlockAnchor{Block: 2, Time: anchor.Time}, anchor.Time.Add(-time.Hour), 12); block != 0 {
		t.Fatalf("a projection before the chain's first block = %d, want 0", block)
	}
}

// testStatsEpochChain is a chain whose blocks are produced at a fixed
// cadence from epoch 1's mined start: the finalized head, the mined
// boundaries, and (optionally) the coordinator's schedule.
type testStatsEpochChain struct {
	blockDuration time.Duration
	head          uint64
	headErr       error

	schedule      statsEpochSchedule
	scheduleErr   error
	scheduleReads int
	timeReads     []uint64
}

func (self *testStatsEpochChain) blockTime(block uint64) time.Time {
	return testStatsEpoch1Start.Add(time.Duration(int64(block)-testStatsEpoch1StartBlock) * self.blockDuration)
}

func (self *testStatsEpochChain) FinalizedHead(context.Context) (uint64, [32]byte, time.Time, error) {
	if self.headErr != nil {
		return 0, [32]byte{}, time.Time{}, self.headErr
	}
	return self.head, [32]byte{}, self.blockTime(self.head), nil
}

func (self *testStatsEpochChain) BlockTime(_ context.Context, block uint64) (time.Time, error) {
	self.timeReads = append(self.timeReads, block)
	if self.head < block {
		return time.Time{}, errors.New("not mined")
	}
	return self.blockTime(block), nil
}

func (self *testStatsEpochChain) EpochSchedule(context.Context) (statsEpochSchedule, error) {
	self.scheduleReads += 1
	if self.scheduleErr != nil {
		return nil, self.scheduleErr
	}
	return append(statsEpochSchedule{}, self.schedule...), nil
}

// testStatsEpochHeadOnly is a chain client that cannot read the schedule.
type testStatsEpochHeadOnly struct {
	chain *testStatsEpochChain
}

func (self testStatsEpochHeadOnly) FinalizedHead(ctx context.Context) (uint64, [32]byte, time.Time, error) {
	return self.chain.FinalizedHead(ctx)
}

func (self testStatsEpochHeadOnly) BlockTime(ctx context.Context, block uint64) (time.Time, error) {
	return self.chain.BlockTime(ctx, block)
}

// Block drift: a chain slower than 12 s a block rolls later than the
// built-in projection. The clock follows the chain's head anchor, rolls at
// the boundary block, and takes the new epoch's start time from the mined
// header once it is final.
func TestStatsEpochClockFollowsTheChainHead(t *testing.T) {
	ctx := context.Background()
	chain := &testStatsEpochChain{
		blockDuration: 12_500 * time.Millisecond,
		schedule:      append(statsEpochSchedule{}, statsMainnetEpochSchedule...),
	}
	clock := newStatsEpochClock()

	// 628 blocks before epoch 2, which at 12.5 s a block is already past
	// the built-in projection of epoch 2's start
	chain.head = testStatsEpoch2StartBlock - 628
	now := chain.blockTime(chain.head).Add(5 * time.Second)
	if builtIn := newStatsEpochClock().window(now); builtIn.Epoch != 2 {
		t.Fatalf("the built-in projection is epoch %d at the drifted head, want 2", builtIn.Epoch)
	}
	if err := clock.refresh(ctx, chain, 12, now); err != nil {
		t.Fatal(err)
	}
	window := clock.window(now)
	if window.Epoch != 1 || window.StartBlock != testStatsEpoch1StartBlock || window.EndBlock != testStatsEpoch2StartBlock {
		t.Fatalf("anchored window = %+v, want epoch 1", window)
	}
	// the end is projected from the head anchor, the start is the mined one
	wantEnd := chain.blockTime(chain.head).Add(628 * 12 * time.Second)
	if !window.Start.Equal(testStatsEpoch1Start) || !window.End.Equal(wantEnd) || !window.PrevStart.Equal(testStatsEpoch0Start) {
		t.Fatalf("anchored times %s to %s (previous %s), want %s to %s", window.Start, window.End, window.PrevStart, testStatsEpoch1Start, wantEnd)
	}

	// between refreshes the clock projects from the anchor and rolls at the
	// boundary block
	if rolled := clock.window(wantEnd.Add(-time.Second)); rolled.Epoch != 1 {
		t.Fatalf("a second before the projected boundary is epoch %d", rolled.Epoch)
	}
	rolled := clock.window(wantEnd)
	if rolled.Epoch != 2 || rolled.StartBlock != testStatsEpoch2StartBlock || !rolled.Start.Equal(wantEnd) ||
		!rolled.HasPrev || rolled.PrevStartBlock != testStatsEpoch1StartBlock || !rolled.PrevStart.Equal(testStatsEpoch1Start) {
		t.Fatalf("projected rollover = %+v", rolled)
	}

	// the boundary is mined and final: its header time replaces the
	// projection, and epoch 1's start stays exact as the previous start
	chain.head = testStatsEpoch2StartBlock + 72
	now = chain.blockTime(chain.head).Add(5 * time.Second)
	if err := clock.refresh(ctx, chain, 12, now); err != nil {
		t.Fatal(err)
	}
	window = clock.window(now)
	if window.Epoch != 2 || !window.Start.Equal(chain.blockTime(testStatsEpoch2StartBlock)) || !window.PrevStart.Equal(testStatsEpoch1Start) {
		t.Fatalf("epoch 2 = %+v, want its mined start %s", window, chain.blockTime(testStatsEpoch2StartBlock))
	}
	// only the current and previous starts are kept
	if len(clock.blockTimes) != 2 {
		t.Fatalf("kept %d boundary times, want 2", len(clock.blockTimes))
	}
}

// The schedule is read from the coordinator on the first refresh and again
// in each new epoch, so a policy scheduled for a later epoch takes effect on
// time; a failed read keeps the last schedule and backs off.
func TestStatsEpochClockReadsTheSchedulePerEpoch(t *testing.T) {
	ctx := context.Background()
	chain := &testStatsEpochChain{
		blockDuration: 12 * time.Second,
		schedule: append(append(statsEpochSchedule{}, statsMainnetEpochSchedule...),
			statsEpochPolicy{EffectiveEpoch: 4, EffectiveBlock: testStatsEpoch4StartBlock, EpochBlocks: 25_200}),
	}
	clock := newStatsEpochClock()

	chain.head = testStatsEpoch3StartBlock + 10
	now := chain.blockTime(chain.head)
	if err := clock.refresh(ctx, chain, 12, now); err != nil {
		t.Fatal(err)
	}
	if err := clock.refresh(ctx, chain, 12, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if chain.scheduleReads != 1 {
		t.Fatalf("schedule reads in one epoch = %d, want 1", chain.scheduleReads)
	}
	chain.head = testStatsEpoch4StartBlock + 10
	now = chain.blockTime(chain.head)
	if err := clock.refresh(ctx, chain, 12, now); err != nil {
		t.Fatal(err)
	}
	if chain.scheduleReads != 2 {
		t.Fatalf("schedule reads after the rollover = %d, want 2", chain.scheduleReads)
	}
	if window := clock.window(now); window.Epoch != 4 || window.EndBlock != testStatsEpoch4StartBlock+25_200 {
		t.Fatalf("epoch 4 under its own policy = %+v", window)
	}

	// a failing read keeps the built-in schedule, and is not retried until
	// the backoff passes
	chain = &testStatsEpochChain{blockDuration: 12 * time.Second, scheduleErr: errors.New("rpc down")}
	clock = newStatsEpochClock()
	chain.head = testStatsEpoch1StartBlock + 10
	now = chain.blockTime(chain.head)
	if err := clock.refresh(ctx, chain, 12, now); err != nil {
		t.Fatal(err)
	}
	if err := clock.refresh(ctx, chain, 12, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if chain.scheduleReads != 1 || clock.window(now).Epoch != 1 {
		t.Fatalf("schedule reads %d, epoch %d", chain.scheduleReads, clock.window(now).Epoch)
	}
	if err := clock.refresh(ctx, chain, 12, now.Add(statsEpochScheduleRetry)); err != nil {
		t.Fatal(err)
	}
	if chain.scheduleReads != 2 {
		t.Fatalf("schedule reads after the backoff = %d, want 2", chain.scheduleReads)
	}

	// a client that cannot read the schedule keeps the built-in one
	headOnly := testStatsEpochHeadOnly{chain: &testStatsEpochChain{blockDuration: 12 * time.Second, head: testStatsEpoch2StartBlock + 1}}
	clock = newStatsEpochClock()
	now = headOnly.chain.blockTime(headOnly.chain.head)
	if err := clock.refresh(ctx, headOnly, 12, now); err != nil {
		t.Fatal(err)
	}
	if window := clock.window(now); window.Epoch != 2 || window.EndBlock != testStatsEpoch3StartBlock {
		t.Fatalf("head-only client window = %+v", window)
	}
}

// A failed head read is an error and the clock keeps projecting, across a
// rollover, from its last anchor.
func TestStatsEpochClockProjectsThroughAChainOutage(t *testing.T) {
	ctx := context.Background()
	chain := &testStatsEpochChain{blockDuration: 12 * time.Second, schedule: append(statsEpochSchedule{}, statsMainnetEpochSchedule...)}
	clock := newStatsEpochClock()
	chain.head = testStatsEpoch2StartBlock - 100
	now := chain.blockTime(chain.head)
	if err := clock.refresh(ctx, chain, 12, now); err != nil {
		t.Fatal(err)
	}
	chain.headErr = errors.New("gateway down")
	later := now.Add(101 * 12 * time.Second)
	if err := clock.refresh(ctx, chain, 12, later); err == nil {
		t.Fatal("a failed head read must be reported")
	}
	if window := clock.window(later); window.Epoch != 2 || !window.PrevStart.Equal(testStatsEpoch1Start) {
		t.Fatalf("projected through the outage = %+v", window)
	}
}

// testStatsGaugeAbsent is true when a gauge would have no value in the feed:
// never exported, or withdrawn to NaN.
func testStatsGaugeAbsent(gauge *statsGauge) bool {
	return !gauge.registered || math.IsNaN(testutil.ToFloat64(gauge.gauge))
}

// testStatsEpochGaugeValue reads an epoch gauge and checks that its
// deprecated block alias carries the same value.
func testStatsEpochGaugeValue(t *testing.T, gauge statsEpochGauge) float64 {
	t.Helper()
	value := testStatsGaugeValue(t, gauge.epoch)
	if alias := testStatsGaugeValue(t, gauge.block); alias != value && !(math.IsNaN(alias) && math.IsNaN(value)) {
		t.Fatalf("block alias = %f, epoch value = %f", alias, value)
	}
	return value
}

// testStatsEpochUsersSource keeps the users snapshots in memory and records
// which epochs' snapshots were read.
type testStatsEpochUsersSource struct {
	users     int64
	since     []time.Time
	snapshots map[uint64]int64
	reads     []uint64
}

func (self *testStatsEpochUsersSource) source() statsEpochSource {
	return statsEpochSource{
		countUsersSince: func(_ context.Context, startTime time.Time) int64 {
			self.since = append(self.since, startTime)
			return self.users
		},
		setUsersSnapshot: func(_ context.Context, epoch uint64, users int64) {
			self.snapshots[epoch] = users
		},
		getUsersSnapshot: func(_ context.Context, epoch uint64) (int64, bool) {
			self.reads = append(self.reads, epoch)
			users, ok := self.snapshots[epoch]
			return users, ok
		},
	}
}

// The previous epoch's users publish only from that epoch's own snapshot:
// nothing at a deploy mid-epoch, the finished epoch's final count from the
// first rollover after it, and nothing (rather than the epoch before's) when
// the finished epoch has no snapshot.
func TestStatsEpochUsersFirstRolloverAfterDeploy(t *testing.T) {
	ctx := context.Background()
	clock := newStatsEpochClock()
	store := &testStatsEpochUsersSource{snapshots: map[uint64]int64{}}

	// deploy three days into epoch 1
	window := clock.window(testStatsEpoch1Start.Add(72 * time.Hour))
	store.users = 100
	statsPublishEpochUsers(ctx, window, store.source())
	if value := testStatsEpochGaugeValue(t, statsEpochUsersGauge); value != 100 {
		t.Fatalf("epoch users = %f, want 100", value)
	}
	if !testStatsGaugeAbsent(statsPrevEpochUsersGauge.epoch) || !testStatsGaugeAbsent(statsPrevEpochUsersGauge.block) {
		t.Fatal("no previous epoch users may publish before the first rollover after deploy")
	}
	if !store.since[0].Equal(testStatsEpoch1Start) || store.snapshots[1] != 100 || store.reads[0] != 0 {
		t.Fatalf("counted since %s, snapshots %v, read %v", store.since[0], store.snapshots, store.reads)
	}

	// the running count freezes forward until the rollover
	store.users = 150
	statsPublishEpochUsers(ctx, clock.window(testStatsEpoch1Start.Add(testStatsEpochWeek-time.Minute)), store.source())

	// the first tick of epoch 2 publishes epoch 1's final count
	store.users = 5
	window = clock.window(testStatsEpoch1Start.Add(testStatsEpochWeek))
	statsPublishEpochUsers(ctx, window, store.source())
	if value := testStatsEpochGaugeValue(t, statsEpochUsersGauge); value != 5 {
		t.Fatalf("epoch 2 users = %f, want 5", value)
	}
	if value := testStatsEpochGaugeValue(t, statsPrevEpochUsersGauge); value != 150 {
		t.Fatalf("previous epoch users = %f, want epoch 1's final 150", value)
	}

	// epoch 3 with no snapshot of epoch 2 (the collector was down through
	// it) withdraws the previous value instead of keeping epoch 1's
	delete(store.snapshots, 2)
	statsPublishEpochUsers(ctx, clock.window(testStatsEpoch1Start.Add(2*testStatsEpochWeek)), store.source())
	if !testStatsGaugeAbsent(statsPrevEpochUsersGauge.epoch) || !testStatsGaugeAbsent(statsPrevEpochUsersGauge.block) {
		t.Fatal("a missing snapshot must withdraw the previous epoch users")
	}
	if last := store.reads[len(store.reads)-1]; last != 2 {
		t.Fatalf("read epoch %d's snapshot, want 2's", last)
	}
}

// The alpha accumulators sum the mirror over the epoch's blocks and the
// previous epoch's, and withdraw the previous values in epoch 0.
func TestStatsEpochAlphaWindows(t *testing.T) {
	ctx := context.Background()
	type blockRange struct{ min, max uint64 }
	ranges := map[string][]blockRange{}
	raoByRange := func(name string, scale int64) func(context.Context, model.StDeploymentKey, uint64, uint64) *big.Int {
		return func(_ context.Context, _ model.StDeploymentKey, minBlock uint64, maxBlock uint64) *big.Int {
			ranges[name] = append(ranges[name], blockRange{minBlock, maxBlock})
			// a distinct amount per range: scale alpha for the current epoch, half that for the previous
			if minBlock == testStatsEpoch1StartBlock {
				return big.NewInt(scale * 1_000_000_000)
			}
			return big.NewInt(scale * 500_000_000)
		}
	}
	source := statsEpochSource{
		depositsRao:  raoByRange("deposits", 2),
		emissionsRao: raoByRange("emissions", 4),
		claims: func(_ context.Context, _ model.StDeploymentKey, minBlock uint64, maxBlock uint64) (*big.Int, int64) {
			ranges["claims"] = append(ranges["claims"], blockRange{minBlock, maxBlock})
			return big.NewInt(6_000_000_000), 3
		},
	}
	clock := newStatsEpochClock()

	statsPublishEpochAlpha(ctx, "964:0xc18925925e2b7bb9059b7d696b8c92762ae86406", clock.window(testStatsEpoch1Start.Add(time.Hour)), source)
	want := []blockRange{{testStatsEpoch1StartBlock, testStatsEpoch2StartBlock}, {testStatsEpoch0StartBlock, testStatsEpoch1StartBlock}}
	for _, name := range []string{"deposits", "emissions", "claims"} {
		if len(ranges[name]) != 2 || ranges[name][0] != want[0] || ranges[name][1] != want[1] {
			t.Fatalf("%s ranges = %v, want %v", name, ranges[name], want)
		}
	}
	for gauge, value := range map[*statsEpochGauge]float64{
		&statsEpochDemandDepositsAlphaGauge:     2,
		&statsPrevEpochDemandDepositsAlphaGauge: 1,
		&statsEpochMinerEmissionsAlphaGauge:     4,
		&statsPrevEpochMinerEmissionsAlphaGauge: 2,
		&statsEpochMinerClaimsAlphaGauge:        6,
		&statsPrevEpochMinerClaimsAlphaGauge:    6,
		&statsEpochMinersClaimedGauge:           3,
		&statsPrevEpochMinersClaimedGauge:       3,
	} {
		if got := testStatsEpochGaugeValue(t, *gauge); got != value {
			t.Fatalf("%s = %f, want %f", gauge.epoch.gauge.Desc(), got, value)
		}
	}

	// epoch 0 has no finished predecessor
	statsPublishEpochAlpha(ctx, "964:0xc18925925e2b7bb9059b7d696b8c92762ae86406", clock.window(testStatsEpoch0Start.Add(time.Hour)), source)
	for _, gauge := range []statsEpochGauge{statsPrevEpochDemandDepositsAlphaGauge, statsPrevEpochMinerEmissionsAlphaGauge, statsPrevEpochMinerClaimsAlphaGauge, statsPrevEpochMinersClaimedGauge} {
		if !testStatsGaugeAbsent(gauge.epoch) || !testStatsGaugeAbsent(gauge.block) {
			t.Fatalf("%s must be withdrawn in epoch 0", gauge.epoch.gauge.Desc())
		}
	}
}

// The clock gauges publish the window, and the deprecated block names carry
// the same number, start and end.
func TestStatsPublishEpochClockWithBlockAliases(t *testing.T) {
	clock := newStatsEpochClock()
	statsPublishEpochClock(clock.window(testStatsEpoch1Start.Add(time.Hour)))
	if value := testStatsEpochGaugeValue(t, statsEpochNumberGauge); value != 1 {
		t.Fatalf("epoch = %f, want 1", value)
	}
	if value := testStatsEpochGaugeValue(t, statsEpochStartSecondsGauge); value != float64(testStatsEpoch1Start.Unix()) {
		t.Fatalf("epoch start = %f", value)
	}
	if value := testStatsEpochGaugeValue(t, statsEpochEndSecondsGauge); value != float64(testStatsEpoch1Start.Add(testStatsEpochWeek).Unix()) {
		t.Fatalf("epoch end = %f", value)
	}
	if testStatsGaugeValue(t, statsEpochStartBlockGauge) != testStatsEpoch1StartBlock || testStatsGaugeValue(t, statsEpochEndBlockGauge) != testStatsEpoch2StartBlock {
		t.Fatal("epoch blocks are wrong")
	}
	if value := testStatsGaugeValue(t, statsPrevEpochStartSecondsGauge); value != float64(testStatsEpoch0Start.Unix()) {
		t.Fatalf("previous epoch start = %f", value)
	}

	statsPublishEpochClock(clock.window(testStatsEpoch0Start.Add(time.Hour)))
	if value := testStatsEpochGaugeValue(t, statsEpochNumberGauge); value != 0 {
		t.Fatalf("epoch = %f, want 0", value)
	}
	if !testStatsGaugeAbsent(statsPrevEpochStartSecondsGauge) {
		t.Fatal("epoch 0 has no previous start")
	}
}

// EpochSchedule reads policyCount and every policyByIndex at one finalized
// block.
func TestCoreStClientEpochScheduleUsesOneFinalizedBlock(t *testing.T) {
	coordinator := stabi.NewSTCoordinator()
	policy := func(epoch uint64, block uint64, blocks uint64) stabi.STCoordinatorPolicySnapshot {
		return stabi.STCoordinatorPolicySnapshot{
			EffectiveEpoch: epoch, EffectiveBlock: block, EpochBlocks: blocks, RootCommitWindowBlocks: 1_200,
			FinalizeOffsetBlocks: 14_400, CloseGraceBlocks: 120,
			EpochDepositCapRao: big.NewInt(1), CampaignDepositCapRao: big.NewInt(2),
		}
	}
	results := map[string]json.RawMessage{
		hex.EncodeToString(coordinator.PackPolicyCount()):                stEpochRPCResult(t, "policyCount", big.NewInt(2)),
		hex.EncodeToString(coordinator.PackPolicyByIndex(big.NewInt(0))): stEpochRPCResult(t, "policyByIndex", policy(0, testStatsEpoch0StartBlock, 7_200)),
		hex.EncodeToString(coordinator.PackPolicyByIndex(big.NewInt(1))): stEpochRPCResult(t, "policyByIndex", policy(1, testStatsEpoch1StartBlock, 50_400)),
	}
	header := &types.Header{
		ParentHash: common.HexToHash("0x01"), UncleHash: types.EmptyUncleHash,
		Coinbase: common.HexToAddress("0x1000000000000000000000000000000000000001"), Root: common.HexToHash("0x02"),
		TxHash: types.EmptyTxsHash, ReceiptHash: types.EmptyReceiptsHash, Difficulty: big.NewInt(1),
		Number: big.NewInt(9_286_524), GasLimit: 30_000_000, Time: 1_792_040_904,
		Extra: []byte{}, MixDigest: common.HexToHash("0x03"), BaseFee: big.NewInt(1),
	}
	headerJSON, err := json.Marshal(header)
	if err != nil {
		t.Fatal(err)
	}
	chainIDJSON, _ := json.Marshal("0x3c4")

	var stateLock sync.Mutex
	finalizedHeadCalls := 0
	blockTags := []string{}
	endpoint := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		defer request.Body.Close()
		var rpcRequest stEpochRPCRequest
		if err := json.NewDecoder(request.Body).Decode(&rpcRequest); err != nil {
			http.Error(writer, err.Error(), http.StatusBadRequest)
			return
		}
		var result json.RawMessage
		switch rpcRequest.Method {
		case "eth_chainId":
			result = chainIDJSON
		case "eth_getBlockByNumber":
			stateLock.Lock()
			finalizedHeadCalls++
			stateLock.Unlock()
			result = headerJSON
		case "eth_call":
			var call map[string]string
			var blockTag string
			if len(rpcRequest.Params) != 2 || json.Unmarshal(rpcRequest.Params[0], &call) != nil || json.Unmarshal(rpcRequest.Params[1], &blockTag) != nil {
				http.Error(writer, "malformed eth_call", http.StatusBadRequest)
				return
			}
			input := call["input"]
			if input == "" {
				input = call["data"]
			}
			result = results[strings.TrimPrefix(input, "0x")]
			if len(result) == 0 {
				http.Error(writer, "unknown call", http.StatusBadRequest)
				return
			}
			stateLock.Lock()
			blockTags = append(blockTags, blockTag)
			stateLock.Unlock()
		default:
			http.Error(writer, "unexpected RPC method", http.StatusBadRequest)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(stEpochRPCResponse{JSONRPC: "2.0", ID: rpcRequest.ID, Result: result})
	}))
	defer endpoint.Close()

	client := &CoreStClient{
		cfg:         &StConfig{RpcUrls: []string{endpoint.URL}, ChainId: 964, ContractAddress: common.HexToAddress("0xC18925925E2B7bb9059b7d696b8c92762AE86406")},
		coordinator: coordinator, clients: map[string]*ethclient.Client{},
	}
	schedule, err := client.EpochSchedule(context.Background())
	client.stateLock.Lock()
	for _, rpcClient := range client.clients {
		rpcClient.Close()
	}
	client.stateLock.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if len(schedule) != 2 || schedule[0] != statsMainnetEpochSchedule[0] || schedule[1] != statsMainnetEpochSchedule[1] {
		t.Fatalf("schedule = %+v, want the mainnet schedule", schedule)
	}
	stateLock.Lock()
	defer stateLock.Unlock()
	if finalizedHeadCalls != 1 || len(blockTags) != 3 {
		t.Fatalf("finalized head reads %d, contract reads %d; want 1 and 3", finalizedHeadCalls, len(blockTags))
	}
	for _, blockTag := range blockTags {
		if blockTag != "0x8db37c" {
			t.Fatalf("contract read block tag = %q, want the finalized 0x8db37c", blockTag)
		}
	}
}
