package sidecarhost

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/sidecar/sidecartest"
)

// A synthetic repo id, in the path and never in a label.
const routeTestRepoId = "01J9ZD8XH5T0W3Y2V6M4K7Q1BC"

func newRouteTestMetrics(t *testing.T) (*Metrics, *prometheus.Registry, *sidecartest.FakeClock) {
	t.Helper()
	registry := prometheus.NewRegistry()
	return newMetrics(registry), registry, sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
}

func routeRequests(metrics *Metrics, route string, code string) float64 {
	return testutil.ToFloat64(metrics.declared["urnetwork_test_http_requests_total"].counter.WithLabelValues(route, code))
}

// Every label value in the registry.
func routeLabelValues(t *testing.T, registry *prometheus.Registry) []string {
	t.Helper()
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	values := []string{}
	for _, family := range families {
		for _, metric := range family.GetMetric() {
			for _, label := range metric.GetLabel() {
				values = append(values, label.GetValue())
			}
		}
	}
	return values
}

// A request served by a ServeMux pattern counts under that pattern, its time under it too, and its path
// reaches no label.
func TestRouteMetricsCountByPattern(t *testing.T) {
	metrics, registry, clock := newRouteTestMetrics(t)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /evs/{repo}/head", func(w http.ResponseWriter, r *http.Request) {
		clock.Advance(2 * time.Second)
		w.Write([]byte("head"))
	})
	mux.HandleFunc("PUT /evs/{repo}/seg/{segment}", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
	})
	handler := newRouteMetrics(metrics, "test", clock).wrap(mux)

	for _, request := range []*http.Request{
		httptest.NewRequest("GET", "/evs/"+routeTestRepoId+"/head", nil),
		httptest.NewRequest("PUT", "/evs/"+routeTestRepoId+"/seg/s1", nil),
		httptest.NewRequest("GET", "/"+routeTestRepoId+"/elsewhere", nil),
	} {
		handler.ServeHTTP(httptest.NewRecorder(), request)
	}
	if count := routeRequests(metrics, "GET /evs/{repo}/head", "200"); count != 1 {
		t.Fatalf("head %v", count)
	}
	if count := routeRequests(metrics, "PUT /evs/{repo}/seg/{segment}", "201"); count != 1 {
		t.Fatalf("segment %v", count)
	}
	if count := routeRequests(metrics, unmatchedRoute, "404"); count != 1 {
		t.Fatalf("unmatched %v", count)
	}
	histogram := metrics.declared["urnetwork_test_http_request_seconds"].histogram
	if count := testutil.CollectAndCount(histogram); count != 3 {
		t.Fatalf("timed routes %d", count)
	}
	sum := 0.0
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() != "urnetwork_test_http_request_seconds" {
			continue
		}
		for _, metric := range family.GetMetric() {
			if metric.GetLabel()[0].GetValue() == "GET /evs/{repo}/head" {
				sum = metric.GetHistogram().GetSampleSum()
			}
		}
	}
	if sum != 2 {
		t.Fatalf("head took %v s on the clock", sum)
	}
	for _, value := range routeLabelValues(t, registry) {
		if strings.Contains(value, routeTestRepoId) || strings.Contains(value, "elsewhere") || strings.Contains(value, "s1") {
			t.Fatalf("a label holds the path: %q", value)
		}
	}
}

// An engine that routes on anything but a ServeMux pattern counts as unmatched, never by its path.
func TestRouteMetricsCountOtherRoutingAsUnmatched(t *testing.T) {
	metrics, registry, clock := newRouteTestMetrics(t)
	handler := newRouteMetrics(metrics, "test", clock).wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/socket/"+routeTestRepoId, nil))
	if count := routeRequests(metrics, unmatchedRoute, "204"); count != 1 {
		t.Fatalf("unmatched %v", count)
	}
	for _, value := range routeLabelValues(t, registry) {
		if strings.Contains(value, "/socket") || strings.Contains(value, routeTestRepoId) {
			t.Fatalf("a label holds the path: %q", value)
		}
	}
}

// A handler that panics before it answers counts as a 500, and the panic goes on to the router, which
// answers it.
func TestRouteMetricsCountAPanicAs500(t *testing.T) {
	metrics, _, clock := newRouteTestMetrics(t)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /evs/{repo}/head", func(w http.ResponseWriter, r *http.Request) {
		panic("an engine bug")
	})
	handler := newRouteMetrics(metrics, "test", clock).wrap(mux)
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("the panic stopped at the metrics")
			}
		}()
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/evs/r/head", nil))
	}()
	if count := routeRequests(metrics, "GET /evs/{repo}/head", "500"); count != 1 {
		t.Fatalf("panic %v", count)
	}
}

// Through Run's router the engine's mux gets the request the metrics read, so a block counts by its
// patterns end to end.
func TestRouteMetricsThroughRun(t *testing.T) {
	fixture := newRunFixture(t)
	fixture.engine.ready.Store(true)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	options := fixture.httpOptions()
	done := fixture.start(ctx, options)
	if err := fixture.awaitLatch(); err != nil {
		t.Fatal(err)
	}
	if recorder := fixture.request("GET", "/evs/"+routeTestRepoId+"/head"); recorder.Code != http.StatusOK {
		t.Fatalf("answered %d", recorder.Code)
	}
	fixture.request("GET", "/nothing/"+routeTestRepoId)
	metrics := options.Metrics.(*Metrics)
	if count := routeRequests(metrics, "GET /evs/{repo}/head", "200"); count != 1 {
		t.Fatalf("head %v", count)
	}
	if count := routeRequests(metrics, unmatchedRoute, "404"); count != 1 {
		t.Fatalf("unmatched %v", count)
	}
	for _, value := range routeLabelValues(t, fixture.registry) {
		if strings.Contains(value, routeTestRepoId) {
			t.Fatalf("a label holds the path: %q", value)
		}
	}
	cancel()
	awaitRunEnd(t, done)
}
