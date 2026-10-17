// Cold readers share one store observation, including failed and panicking
// observations; explicit barriers keep their completion order deterministic.
package model

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// A held first read cannot admit another result that it would later overwrite.
// Cancellation remains local to the triggering request, and every completion
// releases admission for the next cache window.
func TestArindbServingEpochColdReadIsCoalesced(t *testing.T) {
	for _, c := range []struct {
		outcome string
		epoch   int64
		panics  bool
	}{
		{outcome: ArindbDecisionReadOk, epoch: testCanaryOldEpoch},
		{outcome: ArindbDecisionReadAbsent},
		{outcome: ArindbDecisionReadInvalid},
		{outcome: ArindbDecisionReadError},
		{outcome: ArindbDecisionReadError, panics: true},
	} {
		cache := &arindbServingEpochCache{}
		clock := &testArindbClock{now: testCanaryClock}
		started, release := make(chan struct{}), make(chan struct{})
		type result struct {
			epoch     int64
			recovered any
		}
		completed := make(chan result, 1)
		var reads atomic.Int32
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		go func() {
			got := result{}
			defer func() {
				got.recovered = recover()
				completed <- got
			}()
			got.epoch = cache.get(ctx, clock.read, func(readCtx context.Context) (int64, string) {
				reads.Add(1)
				close(started)
				<-release
				if readCtx.Err() == context.Canceled {
					panic("triggering cancellation reached shared read")
				}
				if c.panics {
					panic("synthetic cold read panic")
				}
				return c.epoch, c.outcome
			})
		}()
		<-started
		for range 10 {
			if got := cache.get(t.Context(), clock.read, func(context.Context) (int64, string) {
				reads.Add(1)
				return testCanaryLatestEpoch, ArindbDecisionReadOk
			}); got != 0 {
				t.Errorf("%s panic=%t: concurrent cold reader got %d, want unknown", c.outcome, c.panics, got)
			}
		}
		close(release)
		got := <-completed
		if reads.Load() != 1 || (got.recovered != nil) != c.panics || got.epoch != c.epoch {
			t.Errorf("%s panic=%t: %d reads, result %+v", c.outcome, c.panics, reads.Load(), got)
		}
		if current := cache.get(t.Context(), clock.read, nil); current != c.epoch {
			t.Errorf("%s panic=%t: cached epoch %d, want %d", c.outcome, c.panics, current, c.epoch)
		}
		clock.now = clock.now.Add(arindbServingEpochCacheTtl + time.Nanosecond)
		if current := cache.get(t.Context(), clock.read, func(context.Context) (int64, string) {
			return testCanaryLatestEpoch, ArindbDecisionReadOk
		}); current != testCanaryLatestEpoch {
			t.Errorf("%s panic=%t: next window stayed at %d", c.outcome, c.panics, current)
		}
	}
}
