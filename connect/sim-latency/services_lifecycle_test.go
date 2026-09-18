// Exercises simulator handler ownership against the production PostgreSQL
// eligibility rule, with explicit heartbeat and shutdown barriers.
package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/urnetwork/server"
	connectserver "github.com/urnetwork/server/connect"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// Builds the same registered handler as NewServices, without exchange TCP
// listeners; tests own their H1 listener continuously through httptest.
func newSimulationLifecycleHost(ctx context.Context, cancel context.CancelFunc) *Services {
	self := &Services{cancel: cancel}
	settings := newSimulationExchangeSettings(DefaultServicesConfig())
	exchange := connectserver.NewExchange(ctx, "synthetic-host", "connect", "sim", map[int]int{}, map[string]string{}, settings)
	self.exchanges = append(self.exchanges, exchange)
	self.newConnectHandler(ctx, exchange, &settings.ConnectHandlerSettings)
	return self
}

// A socket with an invented handler id is excluded. Registering the owner and
// refreshing its lease restores discovery without changing the model's gate.
func TestSimulationHandlersRestoreAndMaintainProviderEligibility(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		defer server.Config.PushSimpleResource("tls.yml", []byte("allowed_hosts: [sim.example]\n"))()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		entry := ProviderEntry{
			NetworkId: server.NewId().String(), UserId: server.NewId().String(),
			DeviceId: server.NewId().String(), ClientId: server.NewId().String(),
			LatencyMillis: 15, BandwidthBps: 20 * 1024 * 1024,
			UptimeSeconds: 3600, DowntimeSeconds: 0,
		}
		clientId := server.RequireParseId(entry.ClientId)
		locationId, err := provisionRegion(ctx, RegionConfig{
			Country: "Synthetic Handler Country", CountryCode: "zq",
			Region: "Synthetic Handler Region", City: "Synthetic Handler City",
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := provisionProviders(ctx, []ProviderEntry{entry}, locationId, "ZQ"); err != nil {
			t.Fatal(err)
		}
		model.SetProvide(ctx, clientId, map[model.ProvideMode][]byte{model.ProvideModePublic: make([]byte, 32)})
		connectionId, _, _, _, err := model.ConnectNetworkClient(ctx, clientId, "192.0.2.10:20000", server.NewId())
		if err != nil {
			t.Fatal(err)
		}
		if err := model.SetConnectionLocation(ctx, connectionId, locationId, &model.ConnectionLocationScores{}); err != nil {
			t.Fatal(err)
		}
		performances, err := matureProviderPerformances([]ProviderEntry{entry})
		if err != nil {
			t.Fatal(err)
		}
		reliabilities, err := matureProviderReliabilities([]ProviderEntry{entry})
		if err != nil {
			t.Fatal(err)
		}
		self := newSimulationLifecycleHost(ctx, cancel)
		defer self.Close()
		self.SetPrewarmed(performances)
		caller := session.Testing_CreateClientSession(ctx, jwt.NewByJwt(server.NewId(), server.NewId(), "synthetic-caller", false, false))
		assertReliabilityConnected := func(phase string, want bool) {
			t.Helper()
			self.RunPipelineOnce(ctx)
			server.Db(ctx, func(conn server.PgConn) {
				var connected bool
				server.Raise(conn.QueryRow(ctx, `
					SELECT EXISTS (
						SELECT 1 FROM network_client_location_reliability
						WHERE client_id = $1 AND connected = true
					)
				`, clientId).Scan(&connected))
				if connected != want {
					t.Fatalf("%s: location reliability connected = %t, want %t", phase, connected, want)
				}
			})
		}
		assertProviderCount := func(phase string, want int) {
			t.Helper()
			result, err := model.FindProviders2(&model.FindProviders2Args{
				Specs: []*model.ProviderSpec{{LocationId: &locationId}}, Count: 1, RankMode: model.RankModeQuality,
			}, caller)
			if err != nil || len(result.Providers) != want || want == 1 && result.Providers[0].ClientId != clientId {
				t.Fatalf("%s: providers = %+v, error = %v, want %d matching synthetic provider", phase, result, err, want)
			}
		}
		assertReliabilityConnected("unregistered", false)
		assertProviderCount("unregistered", 0)
		server.Db(ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(ctx, `UPDATE network_client_connection SET handler_id = $2 WHERE connection_id = $1`, connectionId, self.handlerIds[0]))
		}, server.OptReadWrite())
		model.UpdateClientLocationReliabilities(ctx, server.NowUtc().Add(-13*time.Hour), server.NowUtc())
		writeMatureReliabilityScores(ctx, server.NowUtc(), 13*time.Hour, reliabilities)
		assertReliabilityConnected("registered", true)
		assertProviderCount("registered", 1)
		server.Db(ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(ctx, `UPDATE network_client_handler SET heartbeat_time = $2 WHERE handler_id = $1`,
				self.handlerIds[0], server.NowUtc().Add(-3*model.NetworkClientHandlerHeartbeatTimeout)))
		}, server.OptReadWrite())
		// A vanished location target can retain its older Redis sample until
		// expiry. Handler freshness is enforced at this database boundary.
		assertReliabilityConnected("stale heartbeat", false)

		ticks := make(chan time.Time)
		refreshed := make(chan error, 1)
		self.wg.Add(1)
		go func() {
			defer self.wg.Done()
			self.runHandlerHeartbeats(ctx, ticks, func(ctx context.Context, handlerId server.Id) error {
				err := model.HeartbeatNetworkClientHandler(ctx, handlerId)
				refreshed <- err
				return err
			})
		}()
		ticks <- time.Time{}
		if err := <-refreshed; err != nil {
			t.Fatal(err)
		}
		assertReliabilityConnected("refreshed heartbeat", true)
		assertProviderCount("refreshed heartbeat", 1)

		if ports, err := self.handlers[0].ListenerReadyUdpPorts(); err != nil || len(ports) != 0 {
			t.Fatalf("H1-only host acquired packet listeners: %v, %v", ports, err)
		}
		httpServer := httptest.NewServer(http.HandlerFunc(self.handlers[0].Connect))
		defer httpServer.Close()
		conn, response, err := websocket.DefaultDialer.DialContext(ctx, "ws"+strings.TrimPrefix(httpServer.URL, "http"), nil)
		if err != nil {
			t.Fatalf("H1 upgrade failed after disabling UDP listeners: %v, response=%+v", err, response)
		}
		conn.Close()
	})
}

// Close must join an in-flight heartbeat before removing its row, withdraw
// only owned connections, and retain all connection history and foreign hosts.
func TestSimulationHandlersCloseJoinsHeartbeatAndPreservesForeignRows(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		defer server.Config.PushSimpleResource("tls.yml", []byte("allowed_hosts: [sim.example]\n"))()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		self := newSimulationLifecycleHost(ctx, cancel)
		defer self.Close()
		foreignHandlerId := model.CreateNetworkClientHandler(ctx)
		entry := ProviderEntry{
			NetworkId: server.NewId().String(), UserId: server.NewId().String(),
			DeviceId: server.NewId().String(), ClientId: server.NewId().String(),
		}
		if err := provisionIdentityBatch(ctx, []ProviderEntry{entry}); err != nil {
			t.Fatal(err)
		}
		for _, handlerId := range []server.Id{self.handlerIds[0], foreignHandlerId} {
			if _, _, _, _, err := model.ConnectNetworkClient(ctx, server.RequireParseId(entry.ClientId), "192.0.2.20:20000", handlerId); err != nil {
				t.Fatal(err)
			}
		}
		ticks := make(chan time.Time, 1)
		ticks <- time.Time{}
		entered := make(chan struct{})
		release := make(chan struct{})
		var releaseOnce sync.Once
		unblock := func() { releaseOnce.Do(func() { close(release) }) }
		defer unblock()
		self.wg.Add(1)
		go func() {
			defer self.wg.Done()
			self.runHandlerHeartbeats(ctx, ticks, func(ctx context.Context, _ server.Id) error {
				close(entered)
				<-ctx.Done()
				<-release
				return ctx.Err()
			})
		}()
		<-entered
		joining := make(chan struct{})
		closed := make(chan error, 1)
		go func() {
			closed <- self.closeWithWorkerJoin(func() {
				close(joining)
				self.wg.Wait()
			})
		}()
		// The join call cannot return while the heartbeat is held. Cleanup
		// before this call would be observable, independent of scheduling.
		<-joining
		server.Db(context.Background(), func(conn server.PgConn) {
			var exists bool
			server.Raise(conn.QueryRow(context.Background(), `SELECT EXISTS (SELECT 1 FROM network_client_handler WHERE handler_id = $1)`, self.handlerIds[0]).Scan(&exists))
			if !exists {
				t.Fatal("registration removed before its heartbeat joined")
			}
		})
		unblock()
		if err := <-closed; err != nil {
			t.Fatal(err)
		}
		if err := self.Close(); err != nil {
			t.Fatalf("repeated close: %v", err)
		}
		server.Db(context.Background(), func(conn server.PgConn) {
			var owned, foreign, history, connected int
			server.Raise(conn.QueryRow(context.Background(), `
				SELECT (SELECT count(*) FROM network_client_handler WHERE handler_id = $1),
				       (SELECT count(*) FROM network_client_handler WHERE handler_id = $2),
				       count(*), count(*) FILTER (WHERE connected)
				FROM network_client_connection WHERE handler_id IN ($1, $2)
			`, self.handlerIds[0], foreignHandlerId).Scan(&owned, &foreign, &history, &connected))
			if owned != 0 || foreign != 1 || history != 2 || connected != 1 {
				t.Fatalf("closed handler ownership = %d/%d/%d/%d, want 0/1/2/1", owned, foreign, history, connected)
			}
			var foreignConnected bool
			server.Raise(conn.QueryRow(context.Background(), `SELECT connected FROM network_client_connection WHERE handler_id = $1`, foreignHandlerId).Scan(&foreignConnected))
			if !foreignConnected {
				t.Fatal("close withdrew a foreign connection")
			}
		})
		response := httptest.NewRecorder()
		self.handlers[0].Connect(response, httptest.NewRequest(http.MethodGet, "http://sim.example/", nil))
		if response.Code != http.StatusServiceUnavailable {
			t.Fatalf("closed H1 handler accepted new work: %d", response.Code)
		}
	})
}

// A healthy foreign /status must not satisfy readiness after our API bind
// fails. The earlier host's listener and registration must both be released.
func TestSimulationServicesFailedStartupReleasesOwnedHandlers(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		defer server.Config.PushSimpleResource("tls.yml", []byte("allowed_hosts: [sim.example]\n"))()
		ctx := context.Background()
		foreignHandlerId := model.CreateNetworkClientHandler(ctx)
		occupied := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		defer occupied.Close()
		reservations := make([]net.Listener, 0, 2)
		for range 2 {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			reservations = append(reservations, listener)
		}
		config := DefaultServicesConfig()
		config.HostCount = 1
		config.ApiPort = occupied.Listener.Addr().(*net.TCPAddr).Port
		config.WsPortBase = reservations[0].Addr().(*net.TCPAddr).Port
		config.ExchangePortBase = reservations[1].Addr().(*net.TCPAddr).Port
		for _, listener := range reservations {
			listener.Close()
		}
		services, err := NewServices(ctx, config)
		if services != nil || err == nil || !strings.Contains(err.Error(), fmt.Sprintf(":%d", config.ApiPort)) {
			if services != nil {
				services.Close()
			}
			t.Fatalf("occupied healthy API was accepted: services=%v error=%v", services, err)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var owned, foreign int
			server.Raise(conn.QueryRow(ctx, `
				SELECT count(*) FILTER (WHERE handler_id <> $1), count(*) FILTER (WHERE handler_id = $1)
				FROM network_client_handler
			`, foreignHandlerId).Scan(&owned, &foreign))
			if owned != 0 || foreign != 1 {
				t.Fatalf("failed startup retained owned or removed foreign handlers: %d/%d", owned, foreign)
			}
		})
		listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", config.WsPortBase))
		if err != nil {
			t.Fatalf("failed startup retained its H1 listener: %v", err)
		}
		listener.Close()
	})
}
