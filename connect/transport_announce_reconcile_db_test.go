// The root cause on the database. An announcement's single final disconnect
// fails while its handler stays alive: a trigger refuses the write, as
// PgBouncer refused it with query_wait_timeout during the 2026-10-13 livelock.
// The cleanup raises inside its HandleError and nothing retries. Handler
// expiry, the only other retirement, skips the row because the handler keeps
// heartbeating, so before the handler's reconcile the row stayed connected
// for the life of the process. These tests require the test DB env, like the
// rest of this package's announcement lifecycle tests.
package connect

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
)

// One device announced on a handler whose reconcile the test drives.
type reconcileAnnounceForTest struct {
	clientId        server.Id
	transportCancel context.CancelFunc
	registered      chan server.Id
	announce        *ConnectionAnnounce
}

// Creates the device and announces it at once through reconcile. Locations
// are not written and verification is off, so nothing depends on an mmdb or
// the vault's verify.yml.
func startReconcileAnnounceForTest(ctx context.Context, handlerId server.Id, reconcile *handlerConnectionReconcile) *reconcileAnnounceForTest {
	networkId := server.NewId()
	clientId := server.NewId()
	model.Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "", "")
	transportCtx, transportCancel := context.WithCancel(ctx)
	registered := make(chan server.Id, 1)
	settings := DefaultConnectionAnnounceSettings()
	settings.verifySettingsForTest = func() (*model.VerifySettings, bool) {
		return nil, false
	}
	settings.connectionRegisteredForTest = func(announce *ConnectionAnnounce) {
		registered <- *announce.ConnectionId()
	}
	announce := newConnectionAnnounceWithReconcile(
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
		handlerId,
		reconcile,
		0,
		V0TestConfig(),
		settings,
	)
	return &reconcileAnnounceForTest{
		clientId:        clientId,
		transportCancel: transportCancel,
		registered:      registered,
		announce:        announce,
	}
}

// The registered connection, failing unless registration comes first.
func (self *reconcileAnnounceForTest) awaitRegistered(t testing.TB, testCtx context.Context) server.Id {
	t.Helper()
	select {
	case connectionId := <-self.registered:
		return connectionId
	case <-self.announce.done:
		t.Fatal("the announcement ended before registering its connection")
	case <-testCtx.Done():
		t.Fatal("timed out waiting for the announcement to register")
	}
	panic("unreachable")
}

// Ends the transport and joins the announcement's final cleanup.
func (self *reconcileAnnounceForTest) end(t testing.TB, testCtx context.Context) {
	t.Helper()
	self.transportCancel()
	select {
	case <-self.announce.done:
	case <-testCtx.Done():
		t.Fatal("timed out waiting for the announcement to end")
	}
}

// Makes the database refuse every disconnect of clientId's rows until
// allowDisconnectsForTest. The refusal is an ordinary statement error, which
// the transaction does not retry, so the cleanup raises at once.
func refuseDisconnectsForTest(ctx context.Context, clientId server.Id) {
	server.Db(ctx, func(conn server.PgConn) {
		server.RaisePgResult(conn.Exec(ctx, fmt.Sprintf(`
			CREATE FUNCTION test_refuse_disconnect() RETURNS trigger LANGUAGE plpgsql AS $refuse$
			BEGIN
				RAISE EXCEPTION 'test: the database refused this disconnect';
			END
			$refuse$;
			CREATE TRIGGER test_refuse_disconnect BEFORE UPDATE ON network_client_connection
				FOR EACH ROW WHEN (OLD.connected AND NOT NEW.connected AND OLD.client_id = '%s'::uuid)
				EXECUTE FUNCTION test_refuse_disconnect();
		`, clientId)))
	})
}

// The database recovers.
func allowDisconnectsForTest(ctx context.Context) {
	server.Db(ctx, func(conn server.PgConn) {
		server.RaisePgResult(conn.Exec(ctx, `
			DROP TRIGGER IF EXISTS test_refuse_disconnect ON network_client_connection;
			DROP FUNCTION IF EXISTS test_refuse_disconnect();
		`))
	})
}

// A handler and the reconcile the test drives; its loop is not started.
func newReconcileOnLiveHandlerForTest(ctx context.Context) (server.Id, *handlerConnectionReconcile) {
	handlerId := model.CreateNetworkClientHandler(ctx)
	reconcile := newHandlerConnectionReconcileWithStore(
		ctx,
		handlerId,
		DefaultConnectionReconcileSettings(),
		model.GetConnectedNetworkClientHandlerConnectionIds,
		model.RetireNetworkClientHandlerConnection,
	)
	return handlerId, reconcile
}

// A transport ends while the database refuses its final disconnect, and the
// handler stays alive. After the database recovers, the handler's heartbeat
// and handler expiry leave the row connected, as they did before the
// reconcile. The reconcile retires it on its second sighting, and never
// touches the handler's live connection.
func TestConnectionReconcileRetiresRowAfterFailedFinalDisconnect(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		testCtx, testCancel := context.WithTimeout(ctx, 60*time.Second)
		defer testCancel()
		handlerId, reconcile := newReconcileOnLiveHandlerForTest(ctx)
		connected := func(connectionId server.Id) bool {
			return model.GetNetworkClientConnectionStatus(ctx, connectionId).Connected
		}

		failed := startReconcileAnnounceForTest(ctx, handlerId, reconcile)
		live := startReconcileAnnounceForTest(ctx, handlerId, reconcile)
		defer live.announce.CloseAndWait()
		failedConnectionId := failed.awaitRegistered(t, testCtx)
		liveConnectionId := live.awaitRegistered(t, testCtx)

		refuseDisconnectsForTest(ctx, failed.clientId)
		defer allowDisconnectsForTest(ctx)
		failed.end(t, testCtx)
		if !connected(failedConnectionId) {
			t.Fatal("fixture: the refused final disconnect must leave the row connected")
		}

		allowDisconnectsForTest(ctx)
		if err := model.HeartbeatNetworkClientHandler(ctx, handlerId); err != nil {
			t.Fatalf("heartbeat of the live handler: %v", err)
		}
		model.CloseExpiredNetworkClientHandlers(ctx, server.NowUtc().Add(-2*model.NetworkClientHandlerHeartbeatTimeout))
		if !connected(failedConnectionId) {
			t.Fatal("fixture: handler expiry must not reach a row of a live handler")
		}

		reconcile.reconcile()
		reconcile.reconcile()
		if connected(failedConnectionId) {
			t.Fatalf("connection %s is still connected after its final disconnect failed and the database recovered; while its handler lives nothing else disconnects it", failedConnectionId)
		}
		if count := connectedConnectionCountForTest(ctx, failed.clientId); count != 0 {
			t.Fatalf("client %s has %d connected rows, want 0", failed.clientId, count)
		}
		if !connected(liveConnectionId) {
			t.Fatalf("the reconcile retired live connection %s", liveConnectionId)
		}

		live.end(t, testCtx)
		if connected(liveConnectionId) {
			t.Fatalf("live connection %s is still connected after its own final disconnect", liveConnectionId)
		}
		if backlog := reconcile.reconcile(); backlog {
			t.Fatal("a backlog remains after every connection ended")
		}
	})
}

// While the database still refuses, the confirmed retire fails only its row:
// the pass completes without raising and keeps the row as a backlog, and a
// pass after the recovery retires it.
func TestConnectionReconcileRetriesRetireTheDatabaseRefused(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		testCtx, testCancel := context.WithTimeout(ctx, 60*time.Second)
		defer testCancel()
		handlerId, reconcile := newReconcileOnLiveHandlerForTest(ctx)

		failed := startReconcileAnnounceForTest(ctx, handlerId, reconcile)
		failedConnectionId := failed.awaitRegistered(t, testCtx)
		refuseDisconnectsForTest(ctx, failed.clientId)
		defer allowDisconnectsForTest(ctx)
		failed.end(t, testCtx)

		reconcile.reconcile()
		backlog := false
		if r := server.HandleError(func() { backlog = reconcile.reconcile() }); r != nil {
			t.Fatalf("a retire the database refused raised out of its pass: %v", r)
		}
		if !backlog {
			t.Fatal("the pass did not keep the row of its refused retire as a backlog")
		}
		if !model.GetNetworkClientConnectionStatus(ctx, failedConnectionId).Connected {
			t.Fatal("fixture: the refused retire must leave the row connected")
		}

		allowDisconnectsForTest(ctx)
		if backlog := reconcile.reconcile(); backlog {
			t.Fatal("a backlog remains after the retry")
		}
		if model.GetNetworkClientConnectionStatus(ctx, failedConnectionId).Connected {
			t.Fatalf("connection %s is still connected after the database recovered from refusing its retire", failedConnectionId)
		}
	})
}

// A registration whose row committed but whose announcement raised before it
// could hold the row leaves no cleanup. The reconcile retires that orphan once
// the registration has ended, and not while it is in flight.
func TestConnectionReconcileRetiresOrphanOfRaisedRegistration(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		handlerId, reconcile := newReconcileOnLiveHandlerForTest(ctx)
		networkId := server.NewId()
		clientId := server.NewId()
		model.Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "", "")

		endConnect := reconcile.beginConnect()
		connectionId, _, _, _, err := model.ConnectNetworkClientWithIpFamily(ctx, clientId, locationAnnounceClientAddress, handlerId, 0, networkId)
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
		reconcile.reconcile()
		reconcile.reconcile()
		if !model.GetNetworkClientConnectionStatus(ctx, connectionId).Connected {
			t.Fatal("the reconcile retired the row of a registration still in flight")
		}
		// the announcement raised after the commit
		endConnect(nil)
		reconcile.reconcile()
		if model.GetNetworkClientConnectionStatus(ctx, connectionId).Connected {
			t.Fatalf("orphan connection %s of a raised registration is still connected", connectionId)
		}
	})
}
