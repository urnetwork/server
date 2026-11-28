// The announcement's measurement publisher waits on one reused timer. A
// time.After per loop created a timer each time around; the allocation profile
// counts them.
package connect

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/server/v2026"
)

// The measurement publisher waits for each passive sample on one timer,
// re-armed after it fires. A time.After per sample created a timer every
// PassiveSpeedWindowDuration for each connection. The timer is a sweep timer
// (LBALTOPT2.md §8.1), so the publisher creates no runtime timer.
func TestConnectionAnnounceMeasurementPublisherReusesItsSampleTimer(t *testing.T) {
	const windowCount = 100
	var created int64
	sweepCreated := countSweepTimersCreated(t, "github.com/urnetwork/server/v2026/connect.(*ConnectionAnnounce).runMeasurementPublisher", func() {
		created = countTimersCreated(t, "github.com/urnetwork/server/v2026/connect.(*ConnectionAnnounce).runMeasurementPublisher", func() {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				connectionId := server.NewId()
				start := time.Now()
				announce := &ConnectionAnnounce{
					ctx: ctx, cancel: cancel, connectionId: &connectionId,
					settings: DefaultConnectionAnnounceSettings(), testConfig: V0TestConfig(),
					passiveWindowStartTime: start,
					measurementWake:        make(chan struct{}, 1),
					measurementSnapshot:    connectionMeasurements{connectionId: connectionId},
				}
				announce.startWorker(announce.runMeasurementPublisher)
				window := announce.settings.PassiveSpeedWindowDuration
				time.Sleep(windowCount * window)
				synctest.Wait()
				sampled := func() time.Time {
					announce.stateLock.Lock()
					defer announce.stateLock.Unlock()
					return announce.passiveWindowStartTime
				}()
				cancel()
				announce.closeWorkersAndWait()
				// in the bubble every re-arm falls on a tick boundary of the
				// sweep, so no window runs long; timer_sweep_owner_test.go
				// tests the bound for a window armed between ticks
				if sampled.Sub(start) != windowCount*window {
					t.Errorf("the last passive sample was %s in, want %s", sampled.Sub(start), windowCount*window)
				}
			})
		})
	})
	if created != 0 {
		t.Fatalf("%d passive windows created %d runtime timers, want none", windowCount, created)
	}
	if sweepCreated != 1 {
		t.Fatalf("%d passive windows created %d sample timers, want 1", windowCount, sweepCreated)
	}
}
