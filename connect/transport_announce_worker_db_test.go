// A live announcement's measurement publisher on the database. A worker runs
// in HandleError(worker, self.cancel), which does not call that
// context.CancelFunc, so before the fix a raise from the publisher's database
// write ended the publisher for the rest of the connection. This test makes a
// write raise and requires the publisher and the transport to keep going
// while the write is retried. It uses only seams that predate the fix, and
// requires the test DB env like the rest of this package's announcement
// lifecycle tests.
package connect

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// A measurement write that raises must not end the measurement publisher.
// Before the fix the raise ended it while the transport stayed, so the
// connection's measurements stopped for its life. Now the snapshot stays
// pending, the retry commits it, and the publisher and the transport stay.
func TestConnectionAnnounceMeasurementPublisherSurvivesFailedWrite(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		testCtx, testCancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer testCancel()
		// the transport's context: only the test or a teardown cancels it
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		connectionId := server.NewId()
		settings := DefaultConnectionAnnounceSettings()
		// the retry wait after one failed write
		settings.SyncConnectionTimeout = 50 * time.Millisecond
		announce := &ConnectionAnnounce{
			ctx: ctx, cancel: cancel, connectionId: &connectionId,
			settings: settings, testConfig: V0TestConfig(),
			measurementWake:     make(chan struct{}, 1),
			measurementSnapshot: connectionMeasurements{connectionId: connectionId},
		}
		var writes atomic.Int32
		announce.beforeMeasurementWriteForTest = func() {
			if writes.Add(1) == 1 {
				panic(errors.New("test: the database refused this write"))
			}
		}
		published := make(chan connectionMeasurements, 4)
		announce.measurementPublishedForTest = func(measurements connectionMeasurements) {
			published <- measurements
		}
		// closes when the production publisher returns or raises
		publisherEnded := make(chan struct{})
		announce.startWorker(func() {
			defer close(publisherEnded)
			announce.runMeasurementPublisher()
		})
		defer func() {
			cancel()
			announce.closeWorkersAndWait()
			announce.releaseArinShadowOwner()
		}()

		announce.updateMeasurements(func() connectionMeasurements {
			return connectionMeasurements{speed: true, bytesPerSecond: 100}
		})
		select {
		case measurements := <-published:
			if count := writes.Load(); count != 2 {
				t.Fatalf("the snapshot was published after %d write attempts, want the failed one and its retry", count)
			}
			if measurements.speedSampleCount != 1 || measurements.bytesPerSecond != 100 {
				t.Fatalf("the retry published %+v, want the pending snapshot", measurements)
			}
		case <-publisherEnded:
			t.Fatalf(
				"a failed measurement write ended the measurement publisher after %d write attempts; the connection's measurements would stop for its life",
				writes.Load(),
			)
		case <-ctx.Done():
			t.Fatal("a failed measurement write ended the live transport")
		case <-testCtx.Done():
			t.Fatal("timed out waiting for the measurement write's retry")
		}
		select {
		case <-publisherEnded:
			t.Fatal("the measurement publisher ended after its retried write")
		default:
		}
		if err := ctx.Err(); err != nil {
			t.Fatalf("the transport ended after a failed measurement write: %v", err)
		}
		rows := readMeasurementRowsForTest(testCtx, connectionId)
		if rows.bytesPerSecond != 100 || rows.speedSampleCount != 1 {
			t.Fatalf("the retried write committed %+v, want the snapshot", rows)
		}
	})
}
