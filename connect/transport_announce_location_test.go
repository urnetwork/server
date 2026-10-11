// Connection registration commits the network_client_connection row before it
// locates the connection, and only the id it returns lets the announcement
// disconnect that row. These tests make the location write raise or fail, and
// the verify settings resolution raise, around that commit, and count the
// final disconnects. They require the test DB env, like the rest of this
// package's announcement lifecycle tests.
package connect

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
)

// A documentation address, which no mmdb record or override places.
const locationAnnounceClientAddress = "192.0.2.10:20000"

// One location write as the writer saw it.
type locationWriteForTest struct {
	connectionId server.Id
	clientIp     string
	// the row's state, read on the test context during the write
	connected bool
}

// Names the connection by its id rather than its bytes.
func (self locationWriteForTest) String() string {
	return fmt.Sprintf("{connection %s ip %s connected %t}", self.connectionId, self.clientIp, self.connected)
}

// One final disconnect from the announcement's cleanup.
type locationDisconnectForTest struct {
	connectionId server.Id
	err          error
}

// One device's announcement with a test location writer. Registration and
// every final disconnect are observed through the settings hooks, and
// verification is off, so no fixture depends on the vault's verify.yml.
type locationAnnounceFixture struct {
	// live for fixture queries after the transport stops
	ctx             context.Context
	transportCtx    context.Context
	transportCancel context.CancelFunc
	networkId       server.Id
	clientId        server.Id
	handlerId       server.Id
	settings        *ConnectionAnnounceSettings
	registered      chan struct{}
	disconnects     chan locationDisconnectForTest
	announce        *ConnectionAnnounce
}

// Creates the device and its live handler; start announces it.
func newLocationAnnounceFixture(ctx context.Context) *locationAnnounceFixture {
	networkId := server.NewId()
	clientId := server.NewId()
	model.Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "", "")
	transportCtx, transportCancel := context.WithCancel(ctx)
	fixture := &locationAnnounceFixture{
		ctx:             ctx,
		transportCtx:    transportCtx,
		transportCancel: transportCancel,
		networkId:       networkId,
		clientId:        clientId,
		// a live handler, so handler retirement never disconnects the row
		handlerId:   model.CreateNetworkClientHandler(ctx),
		settings:    DefaultConnectionAnnounceSettings(),
		registered:  make(chan struct{}),
		disconnects: make(chan locationDisconnectForTest, 4),
	}
	fixture.settings.verifySettingsForTest = func() (*model.VerifySettings, bool) {
		return nil, false
	}
	fixture.settings.connectionRegisteredForTest = func(*ConnectionAnnounce) {
		close(fixture.registered)
	}
	fixture.settings.connectionDisconnectedForTest = func(connectionId server.Id, err error) {
		fixture.disconnects <- locationDisconnectForTest{connectionId: connectionId, err: err}
	}
	return fixture
}

// Announces the device at once, with writer in place of SetConnectionLocation.
func (self *locationAnnounceFixture) start(writer func(context.Context, server.Id, string) error) {
	self.announce = NewConnectionAnnounce(
		controller.Testing_WithConnectionLocationWriter(self.transportCtx, writer),
		self.transportCancel,
		self.networkId,
		self.clientId,
		locationAnnounceClientAddress,
		self.handlerId,
		0,
		V0TestConfig(),
		self.settings,
	)
}

// Fails unless ready fires before the announcement ends on its own.
func (self *locationAnnounceFixture) awaitLive(t testing.TB, testCtx context.Context, ready <-chan struct{}, event string) {
	t.Helper()
	select {
	case <-ready:
	case <-self.announce.done:
		t.Fatalf(
			"the announcement ended before %s; client %s has %d connected rows left",
			event,
			self.clientId,
			connectedConnectionCountForTest(self.ctx, self.clientId),
		)
	case <-testCtx.Done():
		t.Fatalf("timed out waiting for %s", event)
	}
}

// Fails unless the announcement ends on its own.
func (self *locationAnnounceFixture) awaitEnd(t testing.TB, testCtx context.Context) {
	t.Helper()
	select {
	case <-self.announce.done:
	case <-testCtx.Done():
		t.Fatal("timed out waiting for the announcement to end")
	}
}

// Fails unless the ended announcement's cleanup disconnected exactly
// connectionId, exactly once, and left the client no connected row.
func (self *locationAnnounceFixture) requireDisconnectedOnce(t testing.TB, connectionId server.Id) {
	t.Helper()
	disconnects := drainForTest(self.disconnects)
	if len(disconnects) != 1 {
		t.Errorf("connection %s was disconnected %d times by its announcement's cleanup, want exactly once", connectionId, len(disconnects))
	}
	for _, disconnect := range disconnects {
		if disconnect.connectionId != connectionId || disconnect.err != nil {
			t.Errorf("cleanup disconnected %s (err = %v), want %s without error", disconnect.connectionId, disconnect.err, connectionId)
		}
	}
	if model.GetNetworkClientConnectionStatus(self.ctx, connectionId).Connected {
		t.Errorf("connection %s is still connected after its announcement ended; only retiring its live handler would disconnect it", connectionId)
	}
	if count := connectedConnectionCountForTest(self.ctx, self.clientId); count != 0 {
		t.Errorf("client %s has %d connected rows after its announcement ended, want 0", self.clientId, count)
	}
}

// Every value already sent on values, without waiting.
func drainForTest[T any](values chan T) []T {
	drained := []T{}
	for {
		select {
		case value := <-values:
			drained = append(drained, value)
		default:
			return drained
		}
	}
}

// The client's connection rows still marked connected.
func connectedConnectionCountForTest(ctx context.Context, clientId server.Id) (count int) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT COUNT(*) FROM network_client_connection WHERE client_id = $1 AND connected`,
			clientId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&count))
			}
		})
	})
	return
}

// Records the writer's raise, if any, and propagates it unchanged.
func recordRaiseForTest(raises chan any) {
	if r := recover(); r != nil {
		raises <- r
		panic(r)
	}
}

// The transport is canceled while its first location write waits for a pool
// connection, as when the primary saturated on 2026-10-10, so the write
// raises the pool's context-done error after the connection row committed.
// The announcement must still disconnect that row, exactly once.
func TestConnectionAnnounceDisconnectsAfterCanceledLocationWrite(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		testCtx, testCancel := context.WithTimeout(ctx, 30*time.Second)
		defer testCancel()
		fixture := newLocationAnnounceFixture(ctx)
		writes := make(chan locationWriteForTest, 4)
		raises := make(chan any, 4)
		fixture.start(func(transportCtx context.Context, connectionId server.Id, clientIp string) error {
			defer recordRaiseForTest(raises)
			writes <- locationWriteForTest{
				connectionId: connectionId,
				clientIp:     clientIp,
				connected:    model.GetNetworkClientConnectionStatus(ctx, connectionId).Connected,
			}
			// the client goes away while the write waits for a pool
			// connection, and the mmdb path's writes then raise
			fixture.transportCancel()
			location := &model.Location{
				LocationType: model.LocationTypeCountry,
				Country:      "Japan",
				CountryCode:  "jp",
			}
			model.CreateLocation(transportCtx, location)
			return model.SetConnectionLocation(transportCtx, connectionId, location.LocationId, &model.ConnectionLocationScores{})
		})
		defer fixture.announce.CloseAndWait()
		fixture.awaitEnd(t, testCtx)

		observedWrites := drainForTest(writes)
		if len(observedWrites) != 1 || !observedWrites[0].connected {
			t.Fatalf("location writes = %v, want one over a committed, connected row", observedWrites)
		}
		observedRaises := drainForTest(raises)
		if len(observedRaises) != 1 || !server.IsDoneError(observedRaises[0]) {
			t.Fatalf("location write raises = %v, want one context-done error", observedRaises)
		}
		fixture.requireDisconnectedOnce(t, observedWrites[0].connectionId)
	})
}

// A location write that raises inside its transaction on a live transport,
// here for a country the location table cannot name, fails that attempt and
// not the connection: the announcement registers the committed row, the
// retry locates it, and close disconnects it exactly once.
func TestConnectionAnnounceRetriesLocationAfterWriteRaise(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		testCtx, testCancel := context.WithTimeout(ctx, 30*time.Second)
		defer testCancel()
		fixture := newLocationAnnounceFixture(ctx)
		// the retry is awaited below, never slept for
		fixture.settings.LocationRetryTimeout = time.Millisecond
		writes := make(chan locationWriteForTest, 4)
		raises := make(chan any, 4)
		retried := make(chan struct{})
		// attempts are sequential: the retry goroutine starts after the first
		// attempt returns
		attemptCount := 0
		fixture.start(func(transportCtx context.Context, connectionId server.Id, clientIp string) error {
			attemptCount += 1
			writes <- locationWriteForTest{
				connectionId: connectionId,
				clientIp:     clientIp,
				connected:    model.GetNetworkClientConnectionStatus(ctx, connectionId).Connected,
			}
			if 1 < attemptCount {
				if attemptCount == 2 {
					close(retried)
				}
				return nil
			}
			defer recordRaiseForTest(raises)
			model.CreateLocation(transportCtx, &model.Location{
				LocationType: model.LocationTypeCountry,
				CountryCode:  "zz",
			})
			return errors.New("an unnamed country code did not raise")
		})
		defer fixture.announce.CloseAndWait()

		fixture.awaitLive(t, testCtx, fixture.registered, "registering its connection after the location write raised")
		observedRaises := drainForTest(raises)
		if len(observedRaises) != 1 || server.IsDoneError(observedRaises[0]) {
			t.Fatalf("first location write raises = %v, want one raise on the live transport", observedRaises)
		}
		fixture.awaitLive(t, testCtx, retried, "retrying the location write")
		observedWrites := drainForTest(writes)
		if len(observedWrites) != 2 {
			t.Fatalf("location writes = %v, want the raised attempt and one retry", observedWrites)
		}
		first, retry := observedWrites[0], observedWrites[1]
		if retry.connectionId != first.connectionId || retry.clientIp != first.clientIp || !first.connected || !retry.connected {
			t.Fatalf("retry %v does not continue attempt %v on the connected row", retry, first)
		}
		if count := len(fixture.disconnects); count != 0 {
			t.Fatalf("the live connection was disconnected %d times while its location was retried", count)
		}

		fixture.announce.CloseAndWait()
		fixture.requireDisconnectedOnce(t, first.connectionId)
	})
}

// The control: a location write that succeeds registers the connection and
// stores its location, and close disconnects it exactly once.
func TestConnectionAnnounceDisconnectsOnceOnClose(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		testCtx, testCancel := context.WithTimeout(ctx, 30*time.Second)
		defer testCancel()
		fixture := newLocationAnnounceFixture(ctx)
		location := &model.Location{
			LocationType: model.LocationTypeCountry,
			Country:      "Japan",
			CountryCode:  "jp",
		}
		model.CreateLocation(ctx, location)
		writes := make(chan locationWriteForTest, 4)
		fixture.start(func(transportCtx context.Context, connectionId server.Id, clientIp string) error {
			writes <- locationWriteForTest{
				connectionId: connectionId,
				clientIp:     clientIp,
				connected:    model.GetNetworkClientConnectionStatus(ctx, connectionId).Connected,
			}
			return model.SetConnectionLocation(transportCtx, connectionId, location.LocationId, &model.ConnectionLocationScores{})
		})
		defer fixture.announce.CloseAndWait()

		fixture.awaitLive(t, testCtx, fixture.registered, "registering its connection")
		observedWrites := drainForTest(writes)
		if len(observedWrites) != 1 || !observedWrites[0].connected {
			t.Fatalf("location writes = %v, want one over a committed, connected row", observedWrites)
		}
		connectionId := observedWrites[0].connectionId
		var countryLocationId *server.Id
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(
				ctx,
				`SELECT country_location_id FROM network_client_location WHERE connection_id = $1`,
				connectionId,
			)
			server.WithPgResult(result, err, func() {
				if result.Next() {
					server.Raise(result.Scan(&countryLocationId))
				}
			})
		})
		if countryLocationId == nil {
			t.Fatalf("connection %s stored no location, want country location %s", connectionId, location.CountryLocationId)
		}
		if *countryLocationId != location.CountryLocationId {
			t.Fatalf("connection %s stored country location %s, want %s", connectionId, *countryLocationId, location.CountryLocationId)
		}
		if count := len(fixture.disconnects); count != 0 {
			t.Fatalf("the live connection was disconnected %d times before close", count)
		}
		if !model.GetNetworkClientConnectionStatus(ctx, connectionId).Connected {
			t.Fatalf("registered connection %s is not connected before close", connectionId)
		}

		fixture.announce.CloseAndWait()
		fixture.requireDisconnectedOnce(t, connectionId)
	})
}

// The control for the raise: a location write that returns an error, here the
// mmdb lookup that cannot place a documentation address, keeps the retry as it
// was. The connection stays registered and connected while the retry owns its
// location, and close disconnects it exactly once.
func TestConnectionAnnounceRetriesLocationError(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		testCtx, testCancel := context.WithTimeout(ctx, 30*time.Second)
		defer testCancel()
		fixture := newLocationAnnounceFixture(ctx)
		// the retry is awaited below, never slept for
		fixture.settings.LocationRetryTimeout = time.Millisecond
		writes := make(chan locationWriteForTest, 4)
		firstErrs := make(chan error, 1)
		retried := make(chan struct{})
		// attempts are sequential: the retry goroutine starts after the first
		// attempt returns
		attemptCount := 0
		fixture.start(func(transportCtx context.Context, connectionId server.Id, clientIp string) error {
			attemptCount += 1
			writes <- locationWriteForTest{
				connectionId: connectionId,
				clientIp:     clientIp,
				connected:    model.GetNetworkClientConnectionStatus(ctx, connectionId).Connected,
			}
			if 1 < attemptCount {
				if attemptCount == 2 {
					close(retried)
				}
				return nil
			}
			err := controller.SetConnectionLocation(transportCtx, connectionId, clientIp)
			firstErrs <- err
			return err
		})
		defer fixture.announce.CloseAndWait()

		fixture.awaitLive(t, testCtx, fixture.registered, "registering its connection")
		if observedErrs := drainForTest(firstErrs); len(observedErrs) != 1 || observedErrs[0] == nil {
			t.Fatalf("first location write errors = %v, want the lookup error for a documentation address", observedErrs)
		}
		fixture.awaitLive(t, testCtx, retried, "retrying the location write")
		observedWrites := drainForTest(writes)
		if len(observedWrites) != 2 {
			t.Fatalf("location writes = %v, want the failed attempt and one retry", observedWrites)
		}
		first, retry := observedWrites[0], observedWrites[1]
		if retry.connectionId != first.connectionId || retry.clientIp != first.clientIp || !first.connected || !retry.connected {
			t.Fatalf("retry %v does not continue attempt %v on the connected row", retry, first)
		}
		if count := len(fixture.disconnects); count != 0 {
			t.Fatalf("the live connection was disconnected %d times while its location was retried", count)
		}

		fixture.announce.CloseAndWait()
		fixture.requireDisconnectedOnce(t, first.connectionId)
	})
}

// Verify settings resolve before the connection row commits. A deployment
// whose verify.yml cannot be read raises in every announcement, and that raise
// must not leave a committed row that nothing will disconnect.
func TestConnectionAnnounceVerifySettingsRaiseLeavesNoConnectedRow(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		testCtx, testCancel := context.WithTimeout(ctx, 30*time.Second)
		defer testCancel()
		fixture := newLocationAnnounceFixture(ctx)
		fixture.settings.verifySettingsForTest = func() (*model.VerifySettings, bool) {
			panic(errors.New("synthetic verify.yml failure"))
		}
		writes := make(chan locationWriteForTest, 4)
		fixture.start(func(transportCtx context.Context, connectionId server.Id, clientIp string) error {
			writes <- locationWriteForTest{
				connectionId: connectionId,
				clientIp:     clientIp,
				connected:    model.GetNetworkClientConnectionStatus(ctx, connectionId).Connected,
			}
			return nil
		})
		defer fixture.announce.CloseAndWait()
		fixture.awaitEnd(t, testCtx)

		if count := connectedConnectionCountForTest(ctx, fixture.clientId); count != 0 {
			t.Errorf("client %s has %d connected rows after its verify settings raised, want 0", fixture.clientId, count)
		}
		if observedWrites := drainForTest(writes); len(observedWrites) != 0 {
			t.Errorf("connection rows %v committed before the verify settings resolved", observedWrites)
		}
	})
}
