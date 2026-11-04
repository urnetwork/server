package alt

// The alt fronts' ingress, hardened: the limits are the ingress of whatever
// they wrap, so no front can take a client's own forwarding headers, and the
// public api front does not answer /status.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/server/api"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/session"
)

// NewLimitedHandler is the ingress of whatever it wraps. A front mounted on
// the limits alone still hands every handler the QUIC peer as the client and
// none of the forwarding headers the client sent. Before, the ingress was a
// wrapper of its own around the limits, and a front built on the limits
// without it served the address the client named.
func TestLimitedHandlerIsTheIngressOfWhatItWraps(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	observation := &testIngressObservation{}
	handler := NewLimitedHandler(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			observation.servedRequestCount += 1
			observation.urForwardedFor = r.Header.Values("X-UR-Forwarded-For")
			observation.forwardedFor = r.Header.Values("X-Forwarded-For")
			observation.forwardedSourcePort = r.Header.Values("X-Forwarded-Source-Port")
			observation.requestBuffering = r.Header.Values(router.RequestBufferingHeader)
			clientSession, err := session.NewClientSessionFromRequest(r)
			observation.sessionErr = err
			if err == nil {
				observation.clientAddress = clientSession.ClientAddress
			}
			w.WriteHeader(http.StatusOK)
		}),
		NewLimits(ctx, testLimitsSettings(t)),
	)

	r := httptest.NewRequest(http.MethodPost, "/network/extender-activate", nil)
	r.RemoteAddr = "198.51.100.21:52001"
	r.Header.Set("X-UR-Forwarded-For", "203.0.113.7:443")
	r.Header.Set("X-Forwarded-For", "203.0.113.8")
	r.Header.Set("X-Forwarded-Source-Port", "41001")
	r.Header.Set(router.RequestBufferingHeader, router.RequestBufferingOff)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if observation.servedRequestCount != 1 {
		t.Fatalf("the limits served %d requests (status %d)", observation.servedRequestCount, w.Code)
	}
	if observation.sessionErr != nil {
		t.Fatal(observation.sessionErr)
	}
	if observation.clientAddress != "198.51.100.21:52001" {
		t.Fatalf("the quic peer 198.51.100.21:52001 reached the route as client %q", observation.clientAddress)
	}
	if !slices.Equal(observation.urForwardedFor, []string{"198.51.100.21:52001"}) {
		t.Fatalf("X-UR-Forwarded-For reached the route as %q", observation.urForwardedFor)
	}
	for name, values := range map[string][]string{
		"X-Forwarded-For":             observation.forwardedFor,
		"X-Forwarded-Source-Port":     observation.forwardedSourcePort,
		router.RequestBufferingHeader: observation.requestBuffering,
	} {
		if len(values) != 0 {
			t.Fatalf("%s reached the route as %q", name, values)
		}
	}
}

// The public api front answers /status, whatever the method, exactly as the
// router answers a path it has no route for. Over the public udp sockets the
// status would name the proxy host, the alt service, its block and its
// version to anyone who asks. Alt's deploy poll reads the status listener,
// which still answers (TestStatusHandlerResolvesThePeer), and the front
// still serves the rest of the api.
func TestApiFrontAnswersStatusAsAPathWithNoRoute(t *testing.T) {
	t.Setenv("WARP_HOST", "host0")
	t.Setenv("WARP_SERVICE", "alt")
	t.Setenv("WARP_BLOCK", "test")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	front := newApiFrontHandler(router.NewRouter(ctx, api.Routes()), NewLimits(ctx, testLimitsSettings(t)))

	// one peer per request, so the per-address limits never decide an answer
	peer := 0
	serve := func(method string, path string) (int, string) {
		peer += 1
		r := httptest.NewRequest(method, path, nil)
		r.RemoteAddr = fmt.Sprintf("198.51.100.%d:53000", 30+peer)
		w := httptest.NewRecorder()
		front.ServeHTTP(w, r)
		return w.Code, w.Body.String()
	}
	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodPost} {
		statusCode, statusBody := serve(method, "/status")
		noRouteCode, noRouteBody := serve(method, "/no-such-route")
		if noRouteCode != http.StatusNotFound {
			t.Fatalf("%s of a path with no route = %d %q", method, noRouteCode, noRouteBody)
		}
		if statusCode != noRouteCode || statusBody != noRouteBody {
			t.Fatalf("%s /status = %d %q over the public front, want %d %q as for a path with no route",
				method, statusCode, statusBody, noRouteCode, noRouteBody)
		}
	}
	if code, body := serve(http.MethodGet, "/robots.txt"); code != http.StatusOK || body != router.RobotsTxtBody {
		t.Fatalf("GET /robots.txt = %d %q over the public front", code, body)
	}
}

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

// Each front applies the ingress exactly once: a forged X-UR-Forwarded-For is
// counted once, the value the ingress set itself is not counted again behind
// it, and a request without the header counts nothing.
func TestAltFrontsCountAForgedForwardedForOnce(t *testing.T) {
	t.Setenv("WARP_HOST", "host0")
	t.Setenv("WARP_SERVICE", "alt")
	t.Setenv("WARP_BLOCK", "test")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	apiFront, _ := testApiFront(t)
	statusListener := newStatusHandler(ctx, &Alt{
		listenerStates: map[altListenerKey]bool{
			{transport: ListenerTransportH3, address: "127.0.0.1:443"}: true,
		},
	})
	for _, front := range []struct {
		name    string
		handler http.Handler
		path    string
	}{
		{name: "api front", handler: apiFront, path: "/hello"},
		{name: "status listener", handler: statusListener, path: "/status"},
	} {
		for _, forged := range []bool{true, false} {
			before := ingressOverwrittenCount(t, "x_ur_forwarded_for")
			r := httptest.NewRequest(http.MethodGet, front.path, nil)
			r.RemoteAddr = "198.51.100.22:52002"
			if forged {
				r.Header.Set("X-UR-Forwarded-For", "203.0.113.7:443")
			}
			w := httptest.NewRecorder()
			front.handler.ServeHTTP(w, r)
			if w.Code != http.StatusOK {
				t.Fatalf("%s: %d %q", front.name, w.Code, w.Body.String())
			}
			want := 0.0
			if forged {
				want = 1
			}
			if got := ingressOverwrittenCount(t, "x_ur_forwarded_for") - before; got != want {
				t.Fatalf("%s, forged=%t: counted %v, want %v", front.name, forged, got, want)
			}
		}
	}
}
