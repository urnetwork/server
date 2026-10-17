package controller

// the settlement epoch clock of the public network stats
// (controller/stats_collector.go).
//
// The public stats window their accumulators by the SN25 settlement epoch,
// the span of chain blocks the coordinator counts as one epoch. The clock is
// the coordinator's own arithmetic over its policy schedule
// (STCoordinator.currentEpoch, epochStartBlock and epochEndBlock, over the
// policies policyCount and policyByIndex return): epoch e is the blocks
// [epochStartBlock(e), epochEndBlock(e)), and epochEndBlock(e) is the first
// block of e+1.
//
// On SN25 mainnet (coordinator proxy
// 0xC18925925E2B7bb9059b7d696b8c92762AE86406) epoch 0 was the 7,200-block
// bootstrap epoch from block 9,240,028, and the policy the governance Safe
// scheduled at block 9,240,407 (transaction
// 0x088b29ff3cb8f8e23088f381fe5fad1af62fc1876ad03d27afa70ee46b51d930) made
// every epoch from 1 on 50,400 blocks, about seven days, from block
// 9,247,228. That schedule is built in, so the clock is right before its
// first chain read and while the st subsystem is off. A chain read replaces
// it with the schedule the coordinator holds, and reads it again in each new
// epoch, because a policy for the new epoch may have been scheduled since.
//
// Times are block-derived. A mined boundary's time is its header timestamp.
// Any other block's time, and the block the chain is at now, are projected
// from a head anchor -- the last finalized head read, a block and its
// timestamp -- at the configured block cadence (12 s). Between reads, and
// through a chain outage, the clock keeps projecting from its last anchor.
//
// This clock is the stats collector's alone. The Sunday block clock in
// model/sn_model.go is unchanged: the points leaderboard, its payout-period
// rollup (account_payment_block) and the points readiness signal use it, and
// no settlement, payout or contract code reads this one.

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"time"

	"github.com/urnetwork/glog"
)

// statsEpochScheduleMaxPolicies bounds a schedule. The coordinator holds at
// most MAX_POLICY_VERSIONS (64) policies.
const statsEpochScheduleMaxPolicies = 64

// statsEpochPolicy is one policy of the coordinator's schedule, reduced to
// the fields the epoch arithmetic reads: from EffectiveEpoch on, epoch e
// starts at EffectiveBlock + (e - EffectiveEpoch) * EpochBlocks.
type statsEpochPolicy struct {
	EffectiveEpoch uint64
	EffectiveBlock uint64
	EpochBlocks    uint64
}

// statsEpochSchedule is the coordinator's policy schedule in index order.
type statsEpochSchedule []statsEpochPolicy

// statsMainnetEpochSchedule is the SN25 mainnet schedule as launched (see the
// file comment): policyByIndex(0) and policyByIndex(1).
var statsMainnetEpochSchedule = statsEpochSchedule{
	{EffectiveEpoch: 0, EffectiveBlock: 9_240_028, EpochBlocks: 7_200},
	{EffectiveEpoch: 1, EffectiveBlock: 9_247_228, EpochBlocks: 50_400},
}

// statsMainnetEpochBlockTimes are the header timestamps of the mainnet
// boundaries mined before the schedule was built in: epoch 0 started at
// 2026-10-08 18:08:12 UTC and epoch 1 at 2026-10-09 18:09:00 UTC.
var statsMainnetEpochBlockTimes = map[uint64]time.Time{
	9_240_028: time.Unix(1_791_482_892, 0).UTC(),
	9_247_228: time.Unix(1_791_569_340, 0).UTC(),
}

// statsMainnetEpochAnchor is the built-in head anchor: epoch 1's start block.
// From it to the head at 2026-10-15 05:08:48 UTC (block 9,286,526) the chain
// averaged 12.0003 s a block, so a projection from it drifts by about 15 s an
// epoch. The chain re-anchors the clock on every refresh.
var statsMainnetEpochAnchor = statsBlockAnchor{
	Block: 9_247_228,
	Time:  time.Unix(1_791_569_340, 0).UTC(),
}

// validate holds a schedule to what the coordinator's schedulePolicy
// guarantees: at least one policy, positive epoch lengths, effective epochs
// that strictly increase, and every later policy taking effect exactly at its
// epoch's start under the policies before it.
func (self statsEpochSchedule) validate() error {
	if len(self) == 0 {
		return errors.New("the epoch schedule has no policy")
	}
	if statsEpochScheduleMaxPolicies < len(self) {
		return fmt.Errorf("the epoch schedule has %d policies, more than %d", len(self), statsEpochScheduleMaxPolicies)
	}
	for index, policy := range self {
		if policy.EpochBlocks == 0 {
			return fmt.Errorf("epoch policy %d has no epoch length", index)
		}
		if index == 0 {
			continue
		}
		if policy.EffectiveEpoch <= self[index-1].EffectiveEpoch {
			return fmt.Errorf("epoch policy %d does not follow policy %d", index, index-1)
		}
		if start, ok := self[:index].startBlock(policy.EffectiveEpoch); !ok || start != policy.EffectiveBlock {
			return fmt.Errorf("epoch policy %d takes effect at block %d, not at epoch %d's start", index, policy.EffectiveBlock, policy.EffectiveEpoch)
		}
	}
	return nil
}

// epochAt is STCoordinator.currentEpoch at block: ok is false before the
// first policy takes effect, where the coordinator reverts.
func (self statsEpochSchedule) epochAt(block uint64) (uint64, bool) {
	for index := len(self); index != 0; index -= 1 {
		policy := self[index-1]
		if policy.EffectiveBlock <= block {
			return policy.EffectiveEpoch + (block-policy.EffectiveBlock)/policy.EpochBlocks, true
		}
	}
	return 0, false
}

// policyAt is STCoordinator.policyAt: the last policy in effect by epoch.
func (self statsEpochSchedule) policyAt(epoch uint64) (statsEpochPolicy, bool) {
	for index := len(self); index != 0; index -= 1 {
		if self[index-1].EffectiveEpoch <= epoch {
			return self[index-1], true
		}
	}
	return statsEpochPolicy{}, false
}

// startBlock is STCoordinator.epochStartBlock.
func (self statsEpochSchedule) startBlock(epoch uint64) (uint64, bool) {
	policy, ok := self.policyAt(epoch)
	if !ok {
		return 0, false
	}
	return policy.EffectiveBlock + (epoch-policy.EffectiveEpoch)*policy.EpochBlocks, true
}

// endBlock is STCoordinator.epochEndBlock: the first block of the next epoch.
func (self statsEpochSchedule) endBlock(epoch uint64) (uint64, bool) {
	policy, ok := self.policyAt(epoch)
	if !ok {
		return 0, false
	}
	start, _ := self.startBlock(epoch)
	return start + policy.EpochBlocks, true
}

// statsBlockAnchor is a block and its header timestamp.
type statsBlockAnchor struct {
	Block uint64
	Time  time.Time
}

// statsProjectBlock is the block the chain is projected to be at, at `at`:
// the last block whose projected time is not after it, one block every
// blockSeconds from the anchor in either direction.
func statsProjectBlock(anchor statsBlockAnchor, at time.Time, blockSeconds int64) uint64 {
	if blockSeconds <= 0 {
		blockSeconds = stDefaultBlockSeconds
	}
	step := time.Duration(blockSeconds) * time.Second
	if !at.Before(anchor.Time) {
		return anchor.Block + uint64(at.Sub(anchor.Time)/step)
	}
	back := uint64((anchor.Time.Sub(at) + step - 1) / step)
	if anchor.Block < back {
		return 0
	}
	return anchor.Block - back
}

// statsEpochWindow is the settlement epoch the stats window by, with the
// finished epoch before it.
type statsEpochWindow struct {
	Epoch      uint64
	StartBlock uint64
	// EndBlock is the coordinator's epochEndBlock: the next epoch's first
	// block, so the epoch is the blocks [StartBlock, EndBlock)
	EndBlock uint64
	Start    time.Time
	End      time.Time
	// HasPrev is false in the schedule's first epoch, which has no finished
	// predecessor. The previous epoch is the blocks [PrevStartBlock,
	// StartBlock), from PrevStart to Start.
	HasPrev        bool
	PrevStartBlock uint64
	PrevStart      time.Time
}

// statsEpochScheduleRetry spaces the schedule reads after a failed one, so a
// coordinator read that keeps failing costs the RPC gateway a few calls an
// hour rather than a few a minute.
const statsEpochScheduleRetry = 10 * time.Minute

// statsEpochClock is the collector's clock. It is used only from the collector
// goroutine (or a test's own instance).
type statsEpochClock struct {
	schedule statsEpochSchedule
	// whether the schedule was read from the coordinator, and the epoch the
	// finalized head was in at that read
	scheduleRead      bool
	scheduleReadEpoch uint64
	// no schedule read before this, after a failed one
	scheduleRetryAt time.Time
	anchor          statsBlockAnchor
	blockSeconds    int64
	// header timestamps of mined boundaries: the current and previous
	// epochs' start blocks
	blockTimes map[uint64]time.Time
}

// newStatsEpochClock returns a clock on the built-in mainnet schedule and
// anchor.
func newStatsEpochClock() *statsEpochClock {
	blockTimes := map[uint64]time.Time{}
	for block, blockTime := range statsMainnetEpochBlockTimes {
		blockTimes[block] = blockTime
	}
	return &statsEpochClock{
		schedule:     slices.Clone(statsMainnetEpochSchedule),
		anchor:       statsMainnetEpochAnchor,
		blockSeconds: stDefaultBlockSeconds,
		blockTimes:   blockTimes,
	}
}

// blockTime is a boundary's header timestamp once read, else its time
// projected from the anchor.
func (self *statsEpochClock) blockTime(block uint64) time.Time {
	if blockTime, ok := self.blockTimes[block]; ok {
		return blockTime
	}
	return stEstimateBlockTime(self.anchor.Block, self.anchor.Time, block, self.blockSeconds)
}

// window is the epoch the chain is projected to be in at now.
func (self *statsEpochClock) window(now time.Time) statsEpochWindow {
	if len(self.schedule) == 0 {
		return statsEpochWindow{}
	}
	epoch, ok := self.schedule.epochAt(statsProjectBlock(self.anchor, now, self.blockSeconds))
	if !ok {
		// before the schedule's first block: its first epoch, not yet open
		epoch = self.schedule[0].EffectiveEpoch
	}
	start, _ := self.schedule.startBlock(epoch)
	end, _ := self.schedule.endBlock(epoch)
	window := statsEpochWindow{
		Epoch:      epoch,
		StartBlock: start,
		EndBlock:   end,
		Start:      self.blockTime(start),
		End:        self.blockTime(end),
	}
	if 0 < epoch {
		if prevStart, ok := self.schedule.startBlock(epoch - 1); ok {
			window.HasPrev = true
			window.PrevStartBlock = prevStart
			window.PrevStart = self.blockTime(prevStart)
		}
	}
	return window
}

// statsEpochChain is what the clock reads from the st client: the finalized
// head, its anchor, and the header timestamps of mined boundaries. A client
// that is also a statsEpochScheduleReader keeps the schedule current.
type statsEpochChain interface {
	FinalizedHead(ctx context.Context) (number uint64, hash [32]byte, blockTime time.Time, err error)
	BlockTime(ctx context.Context, block uint64) (time.Time, error)
}

// statsEpochScheduleReader reads the coordinator's policy schedule.
type statsEpochScheduleReader interface {
	EpochSchedule(ctx context.Context) (statsEpochSchedule, error)
}

// refresh re-anchors the clock on the finalized head, reads the schedule on
// the first refresh and in each new epoch, and reads the header timestamps of
// the current and previous epochs' start blocks once they are final. Only a
// failed head read is an error, and the clock then keeps projecting from its
// last anchor; a failed schedule or timestamp read keeps what the clock had
// and is retried on the next refresh.
func (self *statsEpochClock) refresh(ctx context.Context, chain statsEpochChain, blockSeconds int64, now time.Time) error {
	if 0 < blockSeconds {
		self.blockSeconds = blockSeconds
	}
	head, _, headTime, err := chain.FinalizedHead(ctx)
	if err != nil {
		return fmt.Errorf("finalized head: %w", err)
	}
	if headTime.IsZero() {
		return fmt.Errorf("finalized head %d has no timestamp", head)
	}
	self.anchor = statsBlockAnchor{Block: head, Time: headTime}

	if reader, ok := chain.(statsEpochScheduleReader); ok && !now.Before(self.scheduleRetryAt) {
		headEpoch, inSchedule := self.schedule.epochAt(head)
		if !self.scheduleRead || !inSchedule || headEpoch != self.scheduleReadEpoch {
			schedule, err := reader.EpochSchedule(ctx)
			if err == nil {
				err = schedule.validate()
			}
			if err == nil {
				headEpoch, inSchedule = schedule.epochAt(head)
				if !inSchedule {
					err = fmt.Errorf("the finalized head %d precedes the epoch schedule", head)
				}
			}
			if err != nil {
				glog.Infof("[stats]epoch schedule read error, keeping the last schedule (%s)\n", err)
				self.scheduleRetryAt = now.Add(statsEpochScheduleRetry)
			} else {
				self.schedule = schedule
				self.scheduleRead = true
				self.scheduleReadEpoch = headEpoch
			}
		}
	}

	window := self.window(now)
	keep := []uint64{window.StartBlock}
	if window.HasPrev {
		keep = append(keep, window.PrevStartBlock)
	}
	for _, block := range keep {
		if _, ok := self.blockTimes[block]; ok || head < block {
			continue
		}
		blockTime, err := chain.BlockTime(ctx, block)
		if err != nil || blockTime.IsZero() {
			glog.Infof("[stats]epoch boundary %d timestamp read error, projecting it (%v)\n", block, err)
			continue
		}
		self.blockTimes[block] = blockTime
	}
	for block := range self.blockTimes {
		if !slices.Contains(keep, block) {
			delete(self.blockTimes, block)
		}
	}
	return nil
}

// EpochSchedule reads the coordinator's policy schedule at one finalized
// block: policyCount() and, for each policy, the effective epoch, effective
// block and epoch length that currentEpoch(), epochStartBlock(e) and
// epochEndBlock(e) compute from. Only the stats epoch clock reads it.
func (self *CoreStClient) EpochSchedule(ctx context.Context) (statsEpochSchedule, error) {
	if self == nil || self.cfg == nil || self.coordinator == nil {
		return nil, errors.New("st: the epoch schedule needs the coordinator")
	}
	ctx, stopRead, err := beginStRpcRead(ctx, self.readHooks)
	if err != nil {
		return nil, err
	}
	defer stopRead()
	head, err := self.finalizedBlock(ctx)
	if err != nil {
		return nil, fmt.Errorf("finalized head: %w", err)
	}
	count, err := stViewAtBlock(self, ctx, self.cfg.ContractAddress, head.Number, self.coordinator.PackPolicyCount(), self.coordinator.UnpackPolicyCount)
	if err != nil {
		return nil, fmt.Errorf("policyCount(): %w", err)
	}
	if !count.IsUint64() || count.Uint64() == 0 || statsEpochScheduleMaxPolicies < count.Uint64() {
		return nil, fmt.Errorf("policyCount() = %s is out of range", count)
	}
	schedule := make(statsEpochSchedule, 0, count.Uint64())
	for index := uint64(0); index < count.Uint64(); index += 1 {
		policy, err := stViewAtBlock(self, ctx, self.cfg.ContractAddress, head.Number, self.coordinator.PackPolicyByIndex(new(big.Int).SetUint64(index)), self.coordinator.UnpackPolicyByIndex)
		if err != nil {
			return nil, fmt.Errorf("policyByIndex(%d): %w", index, err)
		}
		schedule = append(schedule, statsEpochPolicy{
			EffectiveEpoch: policy.EffectiveEpoch,
			EffectiveBlock: policy.EffectiveBlock,
			EpochBlocks:    policy.EpochBlocks,
		})
	}
	return schedule, schedule.validate()
}
