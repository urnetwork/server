// A live announcement's workers on the database and Redis. A worker runs in
// HandleError(worker, self.cancel), which does not call that
// context.CancelFunc, so before the fix a raise from the measurement
// publisher's database write ended the publisher for the rest of the
// connection, and a raise from a verify egress lease refresh ended the
// refresh. The first lease write runs in run(), whose return cancels the
// transport. These tests make a write raise and require the worker and the
// transport to keep going while the write is retried. They use only seams
// that predate the fix, and require the test DB env like the rest of this
// package's announcement lifecycle tests.
package connect

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
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

// A verify egress lease write that raises keeps the transport. The first
// write runs in run(), so before the fix it ended the announcement before it
// registered; the refreshes that follow go through the same retry. Once Redis
// accepts the lease again, a retried write restores it while the transport
// and row stay.
func TestConnectionAnnounceKeepsTransportThroughFailedVerifyLeaseWrite(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		testCtx, testCancel := context.WithTimeout(ctx, 60*time.Second)
		defer testCancel()
		networkId := server.NewId()
		clientId := server.NewId()
		model.Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "", "")

		// the client's reverse lease index holds the wrong kind of value, so
		// every lease write raises WRONGTYPE until the key is removed
		reverseKey := fmt.Sprintf("{vce4_%s}", clientId)
		server.Redis(ctx, func(r server.RedisClient) {
			server.Raise(r.Set(ctx, reverseKey, "not a lease index", time.Minute).Err())
		})
		removeReverseKey := func() {
			server.Redis(ctx, func(r server.RedisClient) {
				server.Raise(r.Del(ctx, reverseKey).Err())
			})
		}
		defer removeReverseKey()

		verifySettings := model.DefaultVerifySettings()
		verifySettings.EgressRefreshInterval = 50 * time.Millisecond
		settings := DefaultConnectionAnnounceSettings()
		settings.SyncConnectionTimeout = 50 * time.Millisecond
		settings.LocationRetryTimeout = 0
		settings.verifySettingsForTest = func() (*model.VerifySettings, bool) {
			return verifySettings, true
		}
		registered := make(chan server.Id, 1)
		settings.connectionRegisteredForTest = func(announce *ConnectionAnnounce) {
			registered <- *announce.ConnectionId()
		}
		transportCtx, transportCancel := context.WithCancel(ctx)
		announce := NewConnectionAnnounceWithIpFamily(
			controller.Testing_WithConnectionLocationWriter(
				transportCtx,
				func(context.Context, server.Id, string) error {
					return nil
				},
			),
			transportCancel,
			networkId,
			clientId,
			locationAnnounceClientAddress,
			0,
			"",
			server.NewId(),
			0,
			V0TestConfig(),
			settings,
		)
		defer announce.CloseAndWait()

		var connectionId server.Id
		select {
		case connectionId = <-registered:
		case <-announce.done:
			t.Fatalf(
				"a failed verify egress lease write tore down the live transport; client %s has %d connected rows left",
				clientId,
				connectedConnectionCountForTest(ctx, clientId),
			)
		case <-testCtx.Done():
			t.Fatal("timed out waiting for the announcement to register")
		}

		// Redis accepts the lease again; a retried write recreates the index
		removeReverseKey()
		for {
			keyType := ""
			server.Redis(ctx, func(r server.RedisClient) {
				keyType = r.Type(ctx, reverseKey).Val()
			})
			if keyType == "hash" {
				break
			}
			select {
			case <-announce.done:
				t.Fatal("the announcement ended while its lease write was retried")
			case <-testCtx.Done():
				t.Fatalf("no retried lease write restored the lease index (type %q)", keyType)
			case <-time.After(10 * time.Millisecond):
			}
		}
		if err := transportCtx.Err(); err != nil {
			t.Fatalf("the transport ended after a failed lease write: %v", err)
		}
		if !model.GetNetworkClientConnectionStatus(ctx, connectionId).Connected {
			t.Fatalf("connection %s was disconnected after a failed lease write", connectionId)
		}
	})
}
