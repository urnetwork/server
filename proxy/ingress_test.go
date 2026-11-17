package proxy

// The proxy's routers are exposed by DNAT with no nginx in front of them
// (PROXYDRAIN1.md 1.1), so each is its own ingress (router/ingress.go): what
// a client sends in X-UR-Forwarded-For never reaches a handler as its address.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/router"
)

// The ingress counter's value for one header, from the process registry.
func ingressOverwrittenCount(t testing.TB, header string) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() != "urnetwork_ingress_overwritten_headers_total" {
			continue
		}
		for _, metric := range family.GetMetric() {
			for _, label := range metric.GetLabel() {
				if label.GetName() == "header" && label.GetValue() == header {
					return metric.GetCounter().GetValue()
				}
			}
		}
	}
	t.Fatalf("urnetwork_ingress_overwritten_headers_total has no header=%q sample", header)
	return 0
}

// The status port echoes the client address its session resolved: the peer,
// whatever the client claims. Before, the router took the client's
// X-UR-Forwarded-For, so the answer echoed 203.0.113.7:443.
func TestStatusHandlerEchoesThePeerOverAForgedForwardedFor(t *testing.T) {
	t.Setenv("WARP_HOST", "host0")
	t.Setenv("WARP_SERVICE", "proxy")
	t.Setenv("WARP_BLOCK", "test")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	readiness := NewReadiness()
	readiness.SetInitialSyncDone()
	handler := NewStatusHandler(ctx, readiness)

	r := httptest.NewRequest(http.MethodGet, "/status", nil)
	r.RemoteAddr = "198.51.100.41:54001"
	r.Header.Set("X-UR-Forwarded-For", "203.0.113.7:443")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d %q", w.Code, w.Body.String())
	}
	result := &router.WarpStatusResult{}
	if err := json.Unmarshal(w.Body.Bytes(), result); err != nil {
		t.Fatal(err)
	}
	if result.ClientAddress != "198.51.100.41:54001" {
		t.Fatalf("status client address = %q, want the peer 198.51.100.41:54001", result.ClientAddress)
	}
}

// The tls control api passes every request through the ingress before its
// router, whichever route it names, a missing one included: a forged
// X-UR-Forwarded-For is counted once and replaced.
func TestApiAssemblyIsTheIngress(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	transportTls := server.NewTransportTls(map[string]bool{}, &server.TransportTlsSettings{EnableSelfSign: true, DefaultHostName: "fixture.example"})
	assembly := &apiServer{ctx: ctx, cancel: cancel, proxyDeviceManager: &ProxyDeviceManager{}, transportTls: transportTls, settings: DefaultProxySettings()}
	handler, _, _ := assembly.httpConfiguration()
	for _, path := range []string{"/warmup", deviceRpcPath, flowTracePath, "/missing"} {
		for _, forged := range []bool{true, false} {
			before := ingressOverwrittenCount(t, "x_ur_forwarded_for")
			r := httptest.NewRequest(http.MethodPost, path, nil)
			r.RemoteAddr = "198.51.100.42:54002"
			if forged {
				r.Header.Set("X-UR-Forwarded-For", "203.0.113.7:443")
			}
			handler.ServeHTTP(httptest.NewRecorder(), r)
			want := 0.0
			if forged {
				want = 1
			}
			if got := ingressOverwrittenCount(t, "x_ur_forwarded_for") - before; got != want {
				t.Errorf("POST %s, forged=%t: counted %v, want %v", path, forged, got, want)
			}
		}
	}
}
