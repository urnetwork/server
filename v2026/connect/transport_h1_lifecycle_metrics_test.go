// Real RFC WebSocket attempts prove HTTP completion, carrier upgrade and
// authenticated admission are three different observations.
package connect

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/router"
)

// Fixed metric filters never contain an identity or a caller-controlled value.
func connectH1LifecycleMetric(t testing.TB, name string, labels map[string]string) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.Metric {
			matched := 0
			for _, label := range metric.Label {
				if value, ok := labels[label.GetName()]; ok && value == label.GetValue() {
					matched++
				}
			}
			if matched == len(labels) {
				if metric.Counter != nil {
					return metric.Counter.GetValue()
				}
				if metric.Gauge != nil {
					return metric.Gauge.GetValue()
				}
				if metric.Histogram != nil {
					return float64(metric.Histogram.GetSampleCount())
				}
			}
		}
	}
	return 0
}

// The completion channel joins the real handler and all its deferred owners.
// The documentation source address keeps the rate-limit fixture independent
// of local infrastructure exclusions and other loopback test callers.
func connectH1LifecycleFront(t testing.TB, handler *ConnectHandler) (*httptest.Server, <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{}, 1)
	routes := router.NewRouter(ctx, []*router.Route{router.NewRoute(http.MethodGet, "/", handler.Connect)})
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.RemoteAddr = "198.51.100.207:45000"
		routes.ServeHTTP(w, r)
		finished <- struct{}{}
	}))
	t.Cleanup(func() { front.Close(); cancel() })
	return front, finished
}

func waitConnectH1LifecycleFinished(t testing.TB, finished <-chan struct{}) {
	t.Helper()
	select {
	case <-finished:
	case <-time.After(10 * time.Second):
		t.Fatal("real Connect handler did not join after its terminal event")
	}
}

func TestConnectH1LifecycleRateLimitEmpty200IsNotAdmission(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		handler := upgradeNilHandler()
		defer handler.Close()
		handler.settings.ConnectionRateLimitSettings.BurstConnectionCount = 0
		handler.settings.ConnectionRateLimitSettings.BurstConnectionDelay = 0
		front, finished := connectH1LifecycleFront(t, handler)
		rejected := map[string]string{"carrier": "websocket", "stage": "http", "result": "rate_limit"}
		accepted := map[string]string{"carrier": "websocket", "stage": "authenticated", "result": "accepted"}
		beforeRejected := connectH1LifecycleMetric(t, "urnetwork_connect_h1_handshake_results_total", rejected)
		beforeAccepted := connectH1LifecycleMetric(t, "urnetwork_connect_h1_handshake_results_total", accepted)
		conn, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(front.URL, "http"), nil)
		if conn != nil {
			conn.Close()
			t.Fatal("rate-limited request opened a WebSocket")
		}
		if err == nil || response == nil || response.StatusCode != http.StatusOK {
			t.Fatalf("legacy pre-upgrade rejection: status=%v error=%v", response, err)
		}
		body, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil || len(body) != 0 {
			t.Fatalf("legacy rate-limit return changed: bytes=%d error=%v", len(body), err)
		}
		waitConnectH1LifecycleFinished(t, finished)
		if got := connectH1LifecycleMetric(t, "urnetwork_connect_h1_handshake_results_total", rejected) - beforeRejected; got != 1 {
			t.Fatalf("rate-limit rejection counter=%v want=1", got)
		}
		if connectH1LifecycleMetric(t, "urnetwork_connect_h1_handshake_results_total", accepted) != beforeAccepted {
			t.Fatal("empty HTTP200 counted as authenticated admission")
		}
	})
}

func TestConnectH1LifecyclePost101AuthRefusalIsNotAdmission(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		handler := upgradeNilHandler()
		defer handler.Close()
		front, finished := connectH1LifecycleFront(t, handler)
		rejected := map[string]string{"carrier": "websocket", "stage": "upgraded", "result": "jwt_refused"}
		accepted := map[string]string{"carrier": "websocket", "stage": "authenticated", "result": "accepted"}
		beforeRejected := connectH1LifecycleMetric(t, "urnetwork_connect_h1_handshake_results_total", rejected)
		beforeAccepted := connectH1LifecycleMetric(t, "urnetwork_connect_h1_handshake_results_total", accepted)
		conn, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(front.URL, "http"), authenticationDeadlineHeaders("synthetic-invalid-token", server.NewId()))
		if err != nil || response == nil || response.StatusCode != http.StatusSwitchingProtocols {
			t.Fatalf("legacy WebSocket must upgrade before JWT validation: error=%v", err)
		}
		defer conn.Close()
		conn.SetReadDeadline(time.Now().Add(10 * time.Second))
		if _, _, err := conn.ReadMessage(); err == nil {
			t.Fatal("refused JWT left a usable WebSocket")
		}
		waitConnectH1LifecycleFinished(t, finished)
		if got := connectH1LifecycleMetric(t, "urnetwork_connect_h1_handshake_results_total", rejected) - beforeRejected; got != 1 {
			t.Fatalf("post101 auth refusal counter=%v want=1", got)
		}
		if connectH1LifecycleMetric(t, "urnetwork_connect_h1_handshake_results_total", accepted) != beforeAccepted {
			t.Fatal("HTTP101 with refused JWT counted as authenticated admission")
		}
	})
}

func TestConnectH1LifecycleAuthenticatedSocketAndPeerClose(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		env := testing_newPeerDiscoveryEnvWithAllSettings(ctx, t, func(settings *ExchangeSettings) {
			settings.EnableNetworkPeers = false
		}, func(settings *ConnectHandlerSettings) {
			settings.ConnectionAnnounceSettings.EnableNetworkPeers = false
			settings.ConnectionTestConfig = &TestConfig{}
			settings.MinPingTimeout = time.Hour
			settings.MaxPingTimeout = time.Hour
		})
		defer env.Close()
		_, token := env.authClient(&model.AuthNetworkClientArgs{Description: "H1 metric fixture", DeviceSpec: "synthetic"})
		front, finished := connectH1LifecycleFront(t, env.handler)
		accepted := map[string]string{"carrier": "websocket", "stage": "authenticated", "result": "accepted"}
		carrier := map[string]string{"carrier": "websocket"}
		closed := map[string]string{"carrier": "websocket", "reason": "peer_close"}
		beforeAccepted := connectH1LifecycleMetric(t, "urnetwork_connect_h1_handshake_results_total", accepted)
		beforeActive := connectH1LifecycleMetric(t, "urnetwork_connect_h1_sessions_active", carrier)
		beforeClosed := connectH1LifecycleMetric(t, "urnetwork_connect_h1_sessions_closed_total", closed)
		beforeDuration := connectH1LifecycleMetric(t, "urnetwork_connect_h1_session_duration_seconds", carrier)
		conn, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(front.URL, "http"), authenticationDeadlineHeaders(token, server.NewId()))
		if err != nil || response == nil || response.StatusCode != http.StatusSwitchingProtocols {
			t.Fatalf("valid WebSocket upgrade: error=%v", err)
		}
		var servingOnce sync.Once
		serving := make(chan struct{})
		readDone := make(chan struct{})
		conn.SetPongHandler(func(string) error {
			servingOnce.Do(func() { close(serving) })
			return nil
		})
		conn.SetReadDeadline(time.Now().Add(10 * time.Second))
		go func() {
			defer close(readDone)
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
			}
		}()
		defer func() {
			conn.Close()
			<-readDone
		}()
		// Only the authenticated serving reader can answer this control ping.
		// Disable periodic/measurement writes so they cannot race the intended
		// peer-close event with Gorilla's close-sent write error.
		if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(10*time.Second)); err != nil {
			t.Fatal(err)
		}
		select {
		case <-serving:
		case <-readDone:
			t.Fatal("socket closed before its authenticated serving pong")
		case <-ctx.Done():
			t.Fatal("authenticated serving pong did not arrive")
		}
		if got := connectH1LifecycleMetric(t, "urnetwork_connect_h1_handshake_results_total", accepted) - beforeAccepted; got != 1 {
			t.Fatalf("authenticated admission counter=%v want=1", got)
		}
		if got := connectH1LifecycleMetric(t, "urnetwork_connect_h1_sessions_active", carrier) - beforeActive; got != 1 {
			t.Fatalf("live authenticated gauge=%v want=1", got)
		}
		if err := conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(10*time.Second)); err != nil {
			t.Fatal(err)
		}
		waitConnectH1LifecycleFinished(t, finished)
		if got := connectH1LifecycleMetric(t, "urnetwork_connect_h1_sessions_active", carrier); got != beforeActive {
			t.Fatalf("closed socket retained active ownership: %v want=%v", got, beforeActive)
		}
		if got := connectH1LifecycleMetric(t, "urnetwork_connect_h1_sessions_closed_total", closed) - beforeClosed; got != 1 {
			t.Fatalf("peer close counter=%v want=1", got)
		}
		if got := connectH1LifecycleMetric(t, "urnetwork_connect_h1_session_duration_seconds", carrier) - beforeDuration; got != 1 {
			t.Fatalf("authenticated lifetime samples=%v want=1", got)
		}
	})
}
