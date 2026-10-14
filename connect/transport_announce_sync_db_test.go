// A live announcement's sync round on the database. Before the fix a raise
// from the round's read ended the announcement, whose run cancels the
// client's transport as it returns: on 2026-10-13 every database outage
// disconnected the providers whose sync fell in it, exactly while their
// disconnects could not be written. Each test holds the run goroutine in the
// stats hook of one round, injects the failure, and releases it, so the next
// round is the one that meets the failure. They use only seams that predate
// the fix, and require the test DB env like the rest of this package's
// announcement lifecycle tests.
package connect

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
)

// One providing device announced with a short sync interval. Each recorded
// stats round is sent on rounds, and the run goroutine then waits for
// release, so the test decides what the next round meets.
type syncAnnounceForTest struct {
	clientId        server.Id
	transportCtx    context.Context
	transportCancel context.CancelFunc
	registered      chan server.Id
	rounds          chan model.ClientReliabilityStats
	release         chan struct{}
	announce        *ConnectionAnnounce
}

// Creates a public provider and announces it at once. Locations are not
// written and verification is off.
func startSyncAnnounceForTest(ctx context.Context, appVersion string) *syncAnnounceForTest {
	networkId := server.NewId()
	clientId := server.NewId()
	model.Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "", "")
	model.SetProvide(ctx, clientId, map[model.ProvideMode][]byte{
		model.ProvideModePublic: make([]byte, 32),
	})
	transportCtx, transportCancel := context.WithCancel(ctx)
	fixture := &syncAnnounceForTest{
		clientId:        clientId,
		transportCtx:    transportCtx,
		transportCancel: transportCancel,
		registered:      make(chan server.Id, 1),
		rounds:          make(chan model.ClientReliabilityStats),
		release:         make(chan struct{}),
	}
	settings := DefaultConnectionAnnounceSettings()
	settings.SyncConnectionTimeout = 50 * time.Millisecond
	settings.LocationRetryTimeout = 0
	settings.verifySettingsForTest = func() (*model.VerifySettings, bool) {
		return nil, false
	}
	settings.connectionRegisteredForTest = func(announce *ConnectionAnnounce) {
		fixture.registered <- *announce.ConnectionId()
	}
	settings.reliabilityStatsRecordedForTest = func(_ server.Id, stats model.ClientReliabilityStats) {
		select {
		case fixture.rounds <- stats:
		case <-transportCtx.Done():
			return
		}
		select {
		case <-fixture.release:
		case <-transportCtx.Done():
		}
	}
	fixture.announce = NewConnectionAnnounceWithIpFamily(
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
		appVersion,
		server.NewId(),
		0,
		&TestConfig{},
		settings,
	)
	return fixture
}

// The registered connection.
func (self *syncAnnounceForTest) awaitRegistered(t testing.TB, testCtx context.Context) server.Id {
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

// The next recorded round, while the run goroutine waits for release.
func (self *syncAnnounceForTest) awaitRound(t testing.TB, testCtx context.Context) model.ClientReliabilityStats {
	t.Helper()
	select {
	case stats := <-self.rounds:
		return stats
	case <-self.announce.done:
		t.Fatal("the announcement ended before recording a sync round")
	case <-testCtx.Done():
		t.Fatal("timed out waiting for a sync round")
	}
	panic("unreachable")
}

// Ends the transport and joins the announcement, whatever round it holds.
func (self *syncAnnounceForTest) close() {
	self.transportCancel()
	self.announce.CloseAndWait()
}

// Makes the next read of provide_key_change fail, once. The table moves under
// a view whose function counts every read in a sequence, which a failed
// statement does not roll back, and raises on the first.
func failNextProvideKeyChangeReadForTest(ctx context.Context) {
	server.Db(ctx, func(conn server.PgConn) {
		server.RaisePgResult(conn.Exec(ctx, `
			ALTER TABLE provide_key_change RENAME TO test_provide_key_change;
			CREATE SEQUENCE test_provide_key_change_reads;
			CREATE FUNCTION test_provide_key_change_rows() RETURNS SETOF test_provide_key_change
			LANGUAGE plpgsql AS $rows$
			BEGIN
				IF nextval('test_provide_key_change_reads') = 1 THEN
					RAISE EXCEPTION 'test: the database refused this read';
				END IF;
				RETURN QUERY SELECT * FROM test_provide_key_change;
			END
			$rows$;
			CREATE VIEW provide_key_change AS SELECT * FROM test_provide_key_change_rows();
		`))
	})
}

// The reads of provide_key_change since failNextProvideKeyChangeReadForTest,
// the first of them failed.
func provideKeyChangeReadCountForTest(ctx context.Context) (count int64) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `SELECT CASE WHEN is_called THEN last_value ELSE 0 END FROM test_provide_key_change_reads`)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&count))
			}
		})
	})
	return
}

// A sync round whose read fails keeps the live transport and its row. The
// next round proceeds, and its stats still hold the traffic of the failed
// round.
func TestConnectionAnnounceKeepsTransportThroughFailedSyncRead(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		testCtx, testCancel := context.WithTimeout(ctx, 60*time.Second)
		defer testCancel()
		fixture := startSyncAnnounceForTest(ctx, "")
		defer fixture.close()
		connectionId := fixture.awaitRegistered(t, testCtx)

		fixture.awaitRound(t, testCtx)
		failNextProvideKeyChangeReadForTest(ctx)
		// traffic after the held round, for the round that fails to read
		fixture.announce.ReceiveMessage(1000)
		fixture.release <- struct{}{}

		var stats model.ClientReliabilityStats
		select {
		case stats = <-fixture.rounds:
		case <-fixture.announce.done:
			t.Fatalf(
				"a failed sync read tore down the live transport; client %s has %d connected rows left",
				fixture.clientId,
				connectedConnectionCountForTest(ctx, fixture.clientId),
			)
		case <-testCtx.Done():
			t.Fatal("timed out waiting for the round after the failed read")
		}
		if count := provideKeyChangeReadCountForTest(ctx); count < 2 {
			t.Fatalf("provide_key_change was read %d times, want the failed read and a later one", count)
		}
		if err := fixture.transportCtx.Err(); err != nil {
			t.Fatalf("the transport ended after a failed sync read: %v", err)
		}
		if !model.GetNetworkClientConnectionStatus(ctx, connectionId).Connected {
			t.Fatalf("connection %s was disconnected after a failed sync read", connectionId)
		}
		if stats.ReceiveMessageCount != 1 || stats.ReceiveByteCount != 1000 {
			t.Fatalf("the round after the failed read recorded %d messages of %d bytes, want the failed round's 1 of 1000", stats.ReceiveMessageCount, stats.ReceiveByteCount)
		}
		fixture.release <- struct{}{}
	})
}
