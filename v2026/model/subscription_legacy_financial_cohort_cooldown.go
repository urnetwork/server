// A known cohort deadline temporarily selects ordinary settlement on that shard.
// This process-local hint changes optimization only, never financial authority.
package model

import (
	"context"
	"errors"
	"sync/atomic"
	"time"
)

const legacyFinancialCohortCooldownTime = time.Minute

// Deadlines use elapsed monotonic process time, so wall-clock adjustments cannot
// extend this bounded loss of optimization. No row, task or network owner waits.
type legacyFinancialCohortCooldown struct {
	now   func() time.Duration
	until [LegacySettlementShardCount]atomic.Int64
}

func newLegacyFinancialCohortCooldown() *legacyFinancialCohortCooldown {
	started := time.Now()
	return &legacyFinancialCohortCooldown{now: func() time.Duration { return time.Since(started) }}
}

var legacyFinancialCohortProcessCooldown = newLegacyFinancialCohortCooldown()

// Tests may own the advisory clock/state without replacing any page, transaction,
// SQL, rollback or financial decision. Ordinary callers share the fixed array.
type legacyFinancialCohortCooldownKey struct{}

func legacyFinancialCohortCooldownFor(ctx context.Context) *legacyFinancialCohortCooldown {
	if cooldown, ok := ctx.Value(legacyFinancialCohortCooldownKey{}).(*legacyFinancialCohortCooldown); ok {
		return cooldown
	}
	return legacyFinancialCohortProcessCooldown
}

func (self *legacyFinancialCohortCooldown) ready(shard int) bool {
	return self.until[shard].Load() <= int64(self.now())
}

// Concurrent notices may restore probing slightly earlier than the latest
// notice; they never extend a deadline beyond one minute from a real notice.
func (self *legacyFinancialCohortCooldown) deferProbe(shard int) {
	self.until[shard].Store(int64(self.now() + legacyFinancialCohortCooldownTime))
}

// The caller separately proves no commit was attempted and cleanup has joined.
// A stopped child alongside an unrelated body error is not deadline evidence.
func legacyFinancialCohortDeadlineFallback(parent, child context.Context, err error) bool {
	return parent.Err() == nil && errors.Is(child.Err(), context.DeadlineExceeded) && isSettlementPageCancellation(err)
}
