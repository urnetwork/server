// A client that hangs up before H1 admission ends the attempt as its own
// result. H1+ authorizes before its 101, under a context its request context
// cancels, so a departure cancels the authorization step in flight and the
// step reports an unavailable dependency. These tests end a real client at
// each step and pin the controls: a dependency failure while the client is
// still connected, the authorization deadline and the WebSocket carrier keep
// their results. Fixtures use documentation addresses and generated ids.
package connect

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/prometheus/client_golang/prometheus"
	clientconnect "github.com/urnetwork/connect"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// Sums every series of a family whose labels include all of want. A missing
// family or series reads as zero, so a delta can start before its first use.
func connectClientCanceledSum(t testing.TB, name string, want map[string]string) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	sum := 0.0
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.Metric {
			matched := 0
			for _, label := range metric.Label {
				if value, ok := want[label.GetName()]; ok && value == label.GetValue() {
					matched++
				}
			}
			if matched == len(want) {
				sum += metric.GetCounter().GetValue()
			}
		}
	}
	return sum
}

// The fixed outcomes one H1 attempt can publish, as process-wide counters.
type connectClientCanceledOutcome struct {
	clientCanceled float64
	authDependency float64
	authDeadline   float64
	// responses the router saw written with status 503 on the Connect route
	unavailable float64
}

// Reads the outcome counters of one carrier. Only the routed tests reach the
// router's 503 count; directly invoked handlers are checked by their recorder.
func readConnectClientCanceledOutcome(t testing.TB, carrier string) connectClientCanceledOutcome {
	t.Helper()
	result := func(result string) float64 {
		return connectClientCanceledSum(t, "urnetwork_connect_h1_handshake_results_total", map[string]string{"carrier": carrier, "result": result})
	}
	return connectClientCanceledOutcome{
		clientCanceled: result("client_canceled"),
		authDependency: result("auth_dependency"),
		authDeadline:   result("auth_deadline"),
		unavailable:    connectClientCanceledSum(t, "urnetwork_http_requests_total", map[string]string{"route": "GET ^/$", "status": "503"}),
	}
}

// The outcomes published after before was read.
func (self connectClientCanceledOutcome) since(before connectClientCanceledOutcome) connectClientCanceledOutcome {
	return connectClientCanceledOutcome{
		clientCanceled: self.clientCanceled - before.clientCanceled,
		authDependency: self.authDependency - before.authDependency,
		authDeadline:   self.authDeadline - before.authDeadline,
		unavailable:    self.unavailable - before.unavailable,
	}
}

// A real exchange, handler and PostgreSQL state for one network client. Peer
// discovery and announce work stay off, so only admission runs. The handler
// records H1+ selections into stats alone.
func connectClientCanceledEnvironment(ctx context.Context, t testing.TB, stats *clientconnect.H1PlusStats, mutate func(*ConnectHandlerSettings)) (*peerDiscoveryEnv, string) {
	t.Helper()
	env := testing_newPeerDiscoveryEnvWithAllSettings(ctx, t, func(settings *ExchangeSettings) {
		settings.EnableNetworkPeers = false
	}, func(settings *ConnectHandlerSettings) {
		settings.ConnectionAnnounceSettings.EnableNetworkPeers = false
		settings.H1PlusStats = stats
		if mutate != nil {
			mutate(settings)
		}
	})
	_, token := env.authClient(&model.AuthNetworkClientArgs{Description: "client canceled fixture", DeviceSpec: "synthetic"})
	return env, token
}

// One real H1+ client attempt through the front. Canceling ctx closes the
// client socket at once (DialFramedUpgrade), which is how a client, or the
// proxy in front of the service, hangs up. A completed upgrade reads as nil.
func startConnectClientCanceledDial(ctx context.Context, front *httptest.Server, token string) <-chan error {
	result := make(chan error, 1)
	go func() {
		address := "ws" + strings.TrimPrefix(front.URL, "http") + "/"
		conn, err := clientconnect.DialFramedUpgrade(ctx, address, authenticationDeadlineHeaders(token, server.NewId()), &websocket.Dialer{HandshakeTimeout: 10 * time.Second}, clientconnect.H1FramerProtocol)
		if conn != nil {
			conn.Close()
		}
		result <- err
	}()
	return result
}

// The dial's result, within the test's own deadline.
func waitConnectClientCanceledDial(ctx context.Context, t testing.TB, dialed <-chan error) error {
	t.Helper()
	select {
	case err := <-dialed:
		return err
	case <-ctx.Done():
		t.Fatal("H1+ client attempt did not finish")
		return nil
	}
}

// The real Connect route records no H1+ selection and writes no 503 for a
// departed client; the departure is its own result, not a dependency outage.
func checkConnectClientCanceled(t testing.TB, got connectClientCanceledOutcome, stats *clientconnect.H1PlusStats) {
	t.Helper()
	want := connectClientCanceledOutcome{clientCanceled: 1}
	if got != want {
		t.Errorf("departed client outcome %+v, want %+v", got, want)
	}
	if selections := stats.Snapshot(); selections.Attempts != 0 || selections.Fallbacks != 0 {
		t.Errorf("departed client recorded an H1+ selection: %+v", selections)
	}
}

// The client hangs up while its state query waits on a real PostgreSQL lock.
// The departure cancels the query, which then reports an unavailable database.
func TestConnectH1PlusClientLeavingDuringStateQueryIsClientCanceled(t *testing.T) {
	environment := server.DefaultTestEnv()
	environment.RerunCount = 0
	environment.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		dialCtx, leave := context.WithCancel(ctx)
		defer leave()
		stats := &clientconnect.H1PlusStats{}
		env, token := connectClientCanceledEnvironment(ctx, t, stats, nil)
		defer env.Close()
		front, finished := connectH1LifecycleFront(t, env.handler)
		before := readConnectClientCanceledOutcome(t, "h1plus")
		release := holdResidentProfileTable(t, ctx)
		defer release()
		dialed := startConnectClientCanceledDial(dialCtx, front, token)
		waitForBlockedResidentProfiles(t, ctx, 1)
		leave()
		if err := waitConnectClientCanceledDial(ctx, t, dialed); err == nil {
			t.Fatal("departed client completed its upgrade")
		}
		waitConnectH1LifecycleFinished(t, finished)
		checkConnectClientCanceled(t, readConnectClientCanceledOutcome(t, "h1plus").since(before), stats)
	})
}

// The client hangs up after its state query. The hook returns only once the
// handler has observed the departure, so the lease starts under a canceled
// context; the lease reports every unanswered check as ErrAuthUnavailable,
// and only the cause tells this departure from an outage.
func TestConnectH1PlusClientLeavingBeforeLeaseIsClientCanceled(t *testing.T) {
	environment := server.DefaultTestEnv()
	environment.RerunCount = 0
	environment.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		dialCtx, leave := context.WithCancel(ctx)
		defer leave()
		stats := &clientconnect.H1PlusStats{}
		var leased atomic.Int32
		env, token := connectClientCanceledEnvironment(ctx, t, stats, func(settings *ConnectHandlerSettings) {
			settings.testingBeforeAuthorizationLease = func(handleCtx context.Context, _ *session.ByJwt) {
				leave()
				select {
				case <-handleCtx.Done():
				case <-ctx.Done():
				}
			}
			settings.testingAfterAuthorizationLease = func(*session.ByJwt, *session.AuthorizationLease) { leased.Add(1) }
		})
		defer env.Close()
		front, finished := connectH1LifecycleFront(t, env.handler)
		before := readConnectClientCanceledOutcome(t, "h1plus")
		if err := waitConnectClientCanceledDial(ctx, t, startConnectClientCanceledDial(dialCtx, front, token)); err == nil {
			t.Fatal("departed client completed its upgrade")
		}
		waitConnectH1LifecycleFinished(t, finished)
		if leased.Load() != 0 {
			t.Fatal("a departed client got an authorization lease")
		}
		checkConnectClientCanceled(t, readConnectClientCanceledOutcome(t, "h1plus").since(before), stats)
	})
}

// The client hangs up after its lease and before its 101. Nothing may be
// written to it, and no H1+ selection is recorded for an upgrade it left.
func TestConnectH1PlusClientLeavingBeforeUpgradeIsClientCanceled(t *testing.T) {
	environment := server.DefaultTestEnv()
	environment.RerunCount = 0
	environment.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		dialCtx, leave := context.WithCancel(ctx)
		defer leave()
		stats := &clientconnect.H1PlusStats{}
		env, token := connectClientCanceledEnvironment(ctx, t, stats, func(settings *ConnectHandlerSettings) {
			settings.testingAfterAuthorizationLease = func(_ *session.ByJwt, lease *session.AuthorizationLease) {
				leave()
				// the lease runs under the handler context
				select {
				case <-lease.Testing_Done():
				case <-ctx.Done():
				}
			}
		})
		defer env.Close()
		front, finished := connectH1LifecycleFront(t, env.handler)
		before := readConnectClientCanceledOutcome(t, "h1plus")
		upgraded := connectClientCanceledSum(t, "urnetwork_connect_h1_handshake_results_total", map[string]string{"carrier": "h1plus", "stage": "upgraded"})
		if err := waitConnectClientCanceledDial(ctx, t, startConnectClientCanceledDial(dialCtx, front, token)); err == nil {
			t.Fatal("departed client completed its upgrade")
		}
		waitConnectH1LifecycleFinished(t, finished)
		checkConnectClientCanceled(t, readConnectClientCanceledOutcome(t, "h1plus").since(before), stats)
		if got := connectClientCanceledSum(t, "urnetwork_connect_h1_handshake_results_total", map[string]string{"carrier": "h1plus", "stage": "upgraded"}) - upgraded; got != 0 {
			t.Errorf("departed client was upgraded: %v", got)
		}
	})
}

// Control: the same ErrAuthUnavailable from the lease, with the client still
// connected. The lease's state read waits on a real lock until its own check
// deadline, which is a dependency failure answered with a retryable 503.
func TestConnectH1PlusLeaseDependencyFailureWithClientConnectedIsAuthDependency(t *testing.T) {
	environment := server.DefaultTestEnv()
	environment.RerunCount = 0
	environment.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		arrived, proceed := make(chan struct{}), make(chan struct{})
		stats := &clientconnect.H1PlusStats{}
		env, token := connectClientCanceledEnvironment(ctx, t, stats, func(settings *ConnectHandlerSettings) {
			settings.testingBeforeAuthorizationLease = func(context.Context, *session.ByJwt) {
				close(arrived)
				select {
				case <-proceed:
				case <-ctx.Done():
				}
			}
		})
		defer env.Close()
		front, finished := connectH1LifecycleFront(t, env.handler)
		before := readConnectClientCanceledOutcome(t, "h1plus")
		dialed := startConnectClientCanceledDial(ctx, front, token)
		select {
		case <-arrived:
		case <-ctx.Done():
			t.Fatal("admission did not reach its lease")
		}
		// lock only after the state query, so the lease's own read waits
		release := holdResidentProfileTable(t, ctx)
		defer release()
		close(proceed)
		var rejection *clientconnect.HTTPUpgradeError
		if err := waitConnectClientCanceledDial(ctx, t, dialed); !errors.As(err, &rejection) || rejection.StatusCode != http.StatusServiceUnavailable || rejection.Terminal {
			t.Fatalf("connected client did not get a retryable 503: %v", err)
		}
		waitConnectH1LifecycleFinished(t, finished)
		release()
		want := connectClientCanceledOutcome{authDependency: 1, unavailable: 1}
		if got := readConnectClientCanceledOutcome(t, "h1plus").since(before); got != want {
			t.Errorf("connected client outcome %+v, want %+v", got, want)
		}
		if selections := stats.Snapshot(); selections.Attempts != 1 || selections.Fallbacks != 1 {
			t.Errorf("answered H1+ selection was not recorded once: %+v", selections)
		}
	})
}

// Control: WebSocket writes its 101 and takes the socket before authorizing,
// so its request context cannot observe the client leaving. The blocked
// state query ends at the authorization deadline, as before.
func TestConnectWebSocketClientLeavingAfterUpgradeKeepsAuthDependency(t *testing.T) {
	environment := server.DefaultTestEnv()
	environment.RerunCount = 0
	environment.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		env, token := connectClientCanceledEnvironment(ctx, t, &clientconnect.H1PlusStats{}, func(settings *ConnectHandlerSettings) {
			settings.ReadTimeout = 2 * time.Second
		})
		defer env.Close()
		front, finished := connectH1LifecycleFront(t, env.handler)
		upgradedDependency := map[string]string{"carrier": "websocket", "stage": "upgraded", "result": "auth_dependency"}
		dependency := connectClientCanceledSum(t, "urnetwork_connect_h1_handshake_results_total", upgradedDependency)
		before := readConnectClientCanceledOutcome(t, "websocket")
		release := holdResidentProfileTable(t, ctx)
		defer release()
		conn, response, err := (&websocket.Dialer{HandshakeTimeout: 10 * time.Second}).DialContext(ctx, "ws"+strings.TrimPrefix(front.URL, "http")+"/", authenticationDeadlineHeaders(token, server.NewId()))
		if err != nil || response == nil || response.StatusCode != http.StatusSwitchingProtocols {
			t.Fatalf("WebSocket must upgrade before authorizing: %v", err)
		}
		waitForBlockedResidentProfiles(t, ctx, 1)
		conn.Close()
		waitConnectH1LifecycleFinished(t, finished)
		got := readConnectClientCanceledOutcome(t, "websocket").since(before)
		if got.clientCanceled != 0 || got.authDependency != 1 {
			t.Errorf("WebSocket outcome %+v, want only the auth dependency result", got)
		}
		if got := connectClientCanceledSum(t, "urnetwork_connect_h1_handshake_results_total", upgradedDependency) - dependency; got != 1 {
			t.Errorf("WebSocket auth dependency results after its 101: %v, want 1", got)
		}
	})
}

// A synthetic H1+ upgrade from a documentation address, invoked directly on
// a handler without an exchange: these attempts end in the rate limit.
func connectClientCanceledRequest(ctx context.Context, remoteAddress string) *http.Request {
	r := httptest.NewRequestWithContext(ctx, http.MethodGet, "http://connect.example/", nil)
	r.RemoteAddr = remoteAddress
	r.Header.Set("Connection", "Upgrade")
	r.Header.Set("Upgrade", clientconnect.H1FramerProtocol)
	return r
}

// A handler whose every connection waits out an hour of burst delay, so only
// its context can end the rate limit's wait.
func connectClientCanceledRateLimitedHandler(readTimeout time.Duration) *ConnectHandler {
	handler := upgradeNilHandler()
	handler.settings.H1PlusStats = &clientconnect.H1PlusStats{}
	handler.settings.ConnectionRateLimitSettings.BurstConnectionCount = 0
	handler.settings.ConnectionRateLimitSettings.BurstConnectionDelay = time.Hour
	handler.settings.ReadTimeout = readTimeout
	return handler
}

// The client hangs up while its connection waits in the rate limit's burst
// delay. The test waits for the attempt's own total counter, so the departure
// lands in the delay: before the counter write, the Redis wrapper panics on
// its done context instead, which the router recovers as a canceled request.
func TestConnectH1ClientLeavingDuringRateLimitIsClientCanceled(t *testing.T) {
	environment := server.DefaultTestEnv()
	environment.RerunCount = 0
	environment.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		const remoteAddress = "198.51.100.211:45011"
		handler := connectClientCanceledRateLimitedHandler(time.Hour)
		defer handler.Close()
		client, err := server.NewRateLimitClient(remoteAddress)
		if err != nil || client.Excluded() {
			t.Fatalf("documentation address is not a counted caller: %v", err)
		}
		totalKey := fmt.Sprintf("{connect_%s}total_%s", client.IpHashHex(), handler.handlerId)
		before := readConnectClientCanceledOutcome(t, "h1plus")
		requestCtx, leave := context.WithCancel(ctx)
		defer leave()
		w := httptest.NewRecorder()
		finished := make(chan any, 1)
		go func() {
			finished <- upgradeNilInvoke(handler, w, connectClientCanceledRequest(requestCtx, remoteAddress))
		}()
		if err := TestingWaitForConnectCondition(ctx, 10*time.Second, time.Millisecond, func(ctx context.Context) (bool, string) {
			count := int64(0)
			server.Redis(ctx, func(r server.RedisClient) {
				count, _ = r.Get(ctx, totalKey).Int64()
			})
			return count == 1, "waiting for the attempt to enter its burst delay"
		}); err != nil {
			t.Fatal(err)
		}
		leave()
		select {
		case recovered := <-finished:
			if recovered != nil {
				t.Fatalf("departed rate-limited attempt panicked: %v", recovered)
			}
		case <-ctx.Done():
			t.Fatal("rate limit did not end with its request")
		}
		if w.Code == http.StatusServiceUnavailable || w.Body.Len() != 0 {
			t.Errorf("departed client was answered: status=%d bytes=%d", w.Code, w.Body.Len())
		}
		want := connectClientCanceledOutcome{clientCanceled: 1}
		if got := readConnectClientCanceledOutcome(t, "h1plus").since(before); got != want {
			t.Errorf("departed rate-limited outcome %+v, want %+v", got, want)
		}
	})
}

// Control: the authorization deadline itself, which ends the same burst
// delay with the client still connected, stays auth_deadline with a 503.
func TestConnectH1AuthorizationDeadlineDuringRateLimitIsAuthDeadline(t *testing.T) {
	environment := server.DefaultTestEnv()
	environment.RerunCount = 0
	environment.Run(t, func(t testing.TB) {
		handler := connectClientCanceledRateLimitedHandler(time.Second)
		defer handler.Close()
		before := readConnectClientCanceledOutcome(t, "h1plus")
		w := httptest.NewRecorder()
		if recovered := upgradeNilInvoke(handler, w, connectClientCanceledRequest(t.Context(), "198.51.100.212:45012")); recovered != nil {
			t.Fatalf("expired rate-limited attempt panicked: %v", recovered)
		}
		if w.Code != http.StatusServiceUnavailable {
			t.Errorf("expired attempt status=%d, want a retryable 503", w.Code)
		}
		want := connectClientCanceledOutcome{authDeadline: 1}
		if got := readConnectClientCanceledOutcome(t, "h1plus").since(before); got != want {
			t.Errorf("expired rate-limited outcome %+v, want %+v", got, want)
		}
	})
}
