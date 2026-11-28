// The provider intent check's observation waits on the process's timer sweep
// (CPUOPT7.md §11): each observation comes ObserveInterval after the last pass
// ended, or at most one sweep tick after that, never before, and the timer
// leaves the sweep when the connection ends.
package connect

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// A scripted store that reports the time of each observation.
type observeTimingProviderIntentStore struct {
	*testingProviderIntentStore
	observeTimes chan time.Time
}

func (self *observeTimingProviderIntentStore) observe(ctx context.Context, networkId server.Id, clientId server.Id, presenceTimeout time.Duration) *model.ProviderIntentState {
	self.observeTimes <- time.Now()
	return self.testingProviderIntentStore.observe(ctx, networkId, clientId, presenceTimeout)
}

func TestProviderIntentCheckObservesWithinASweepTickOfEachInterval(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sweep := newBubbleTimerSweep()
		defer sweep.close()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		store := &observeTimingProviderIntentStore{
			testingProviderIntentStore: &testingProviderIntentStore{
				observeState: &model.ProviderIntentState{
					Status: model.ProviderIntentStatusQualified,
				},
				nowValue: time.Now(),
			},
			observeTimes: make(chan time.Time, 8),
		}
		settings := DefaultProviderIntentCheckSettings()
		// not a multiple of the tick, so every deadline falls between ticks
		settings.ObserveInterval = 2*time.Minute + 37*time.Millisecond + 11*time.Microsecond
		check := newProviderIntentCheckWithStore(ctx, server.NewId(), server.NewId(), connectTransportH1, settings, store, sweep.sweep)

		time.Sleep(53*time.Millisecond + 29*time.Microsecond)
		start := time.Now()
		done := make(chan bool, 1)
		go func() {
			done <- check.Run()
		}()
		synctest.Wait()
		if armed := sweep.ownerArmedCount(); armed != 1 {
			t.Fatalf("%d check timers on the sweep, want 1", armed)
		}

		deadline := start.Add(settings.ObserveInterval)
		for observation := range 3 {
			time.Sleep(time.Until(sweep.fireTime(deadline)) - time.Nanosecond)
			synctest.Wait()
			select {
			case at := <-store.observeTimes:
				t.Fatalf("observation %d came %s before its tick", observation, sweep.fireTime(deadline).Sub(at))
			default:
			}
			time.Sleep(time.Nanosecond)
			synctest.Wait()
			select {
			case at := <-store.observeTimes:
				requireSweepFire(t, sweep, "the observation", deadline, at)
				// the next interval counts from the end of this pass
				deadline = at.Add(settings.ObserveInterval)
			default:
				t.Fatalf("no observation %d", observation)
			}
		}

		cancel()
		if !<-done {
			t.Fatal("a check whose connection ended reported a kick")
		}
		if armed := sweep.ownerArmedCount(); armed != 0 {
			t.Fatalf("%d check timers on the sweep after the check ended", armed)
		}
	})
}
