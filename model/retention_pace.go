package model

import (
	"context"
	"time"
)

// Paces a retention delete loop to a ceiling of rows per second, measured
// from the loop's start, so a backlog that comes due at once drains at a rate
// the primary can absorb instead of in one burst.
type retentionPace struct {
	rate  float64
	start time.Time
	count int
}

// A pace that starts now. A rate of zero or less never waits.
func newRetentionPace(rate float64) *retentionPace {
	return &retentionPace{rate: rate, start: time.Now()}
}

// Adds count deleted rows, then waits until the loop is back under its rate.
// False when ctx ends first or the wait would reach deadline: the caller then
// stops with work left, for its next run.
func (self *retentionPace) WaitAfter(ctx context.Context, count int, deadline time.Time) bool {
	self.count += count
	if !(0 < self.rate) {
		return true
	}
	due := self.start.Add(time.Duration(float64(self.count) / self.rate * float64(time.Second)))
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
