package model

import (
	"context"
	"time"
)

// Paces a retention delete loop to a ceiling of rows per second, so a backlog
// that comes due at once drains at a rate the primary can absorb instead of in
// one burst. Each batch is held to its own rate, read for it: a batch of n
// rows at r per second starts the next one n/r seconds after it began, at the
// soonest. A slow batch earns no credit for the next, so the loop never bursts
// to catch up.
type retentionPace struct {
	// when the batch now ending began
	batchStart time.Time
}

func newRetentionPace() *retentionPace {
	return &retentionPace{batchStart: time.Now()}
}

// After a batch of count rows, waits until the loop is back under rate rows
// per second. A rate of zero or less never waits. False when ctx ends first or
// the wait would reach deadline: the caller then stops with work left, for its
// next run.
func (self *retentionPace) WaitAfter(ctx context.Context, count int, rate float64, deadline time.Time) bool {
	defer func() {
		self.batchStart = time.Now()
	}()
	if !(0 < rate) {
		return true
	}
	due := self.batchStart.Add(time.Duration(float64(count) / rate * float64(time.Second)))
	wait := time.Until(due)
	if wait <= 0 {
		return true
	}
	if !due.Before(deadline) {
		return false
	}
	select {
	case <-ctx.Done():
		return false
	case <-time.After(wait):
		return true
	}
}
