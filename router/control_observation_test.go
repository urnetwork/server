package router

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/urnetwork/server/session"
)

// A delayed authenticated route and a slow frame handler must be distinguishable
// without retaining a URL, client identity, or credentials in any label.
func TestControlHttpObservationSeparatesPhaseResidence(t *testing.T) {
	registry := prometheus.NewRegistry()
	metrics := newControlHttpMetrics(registry)
	now := time.Unix(1_700_000_000, 0)
	metrics.now = func() time.Time { return now }
	req, finish := observeConnectControl(httptest.NewRequest(http.MethodPost, "/synthetic", nil), metrics)
	func() {
		defer finish()
		now = now.Add(time.Second)
		advanceControlHttpPhase(req, controlHttpAuthenticate)
		if got := testutil.ToFloat64(metrics.inflight.WithLabelValues("authenticate")); got != 1 {
			t.Fatalf("authentication inflight = %v", got)
		}
		now = now.Add(12 * time.Second)
		advanceControlHttpPhase(req, controlHttpController)
		now = now.Add(2 * time.Second)
		advanceControlHttpPhase(req, controlHttpResponse)
		now = now.Add(time.Second)
	}()
	for index, phase := range controlHttpPhases {
		if got := testutil.ToFloat64(metrics.inflight.WithLabelValues(phase)); got != 0 {
			t.Fatalf("%s inflight retained = %v", phase, got)
		}
		if got := testutil.ToFloat64(metrics.completions.WithLabelValues(phase, "ok")); got != 1 {
			t.Fatalf("%s completion = %v", phase, got)
		}
		families, err := registry.Gather()
		if err != nil {
			t.Fatal(err)
		}
		for _, family := range families {
			if family.GetName() != "urnetwork_connect_control_http_phase_seconds" {
				continue
			}
			for _, metric := range family.Metric {
				if metric.Label[0].GetValue() == phase && metric.Summary.GetSampleSum() != []float64{1, 12, 2, 1}[index] {
					t.Fatalf("%s residence = %v", phase, metric.Summary.GetSampleSum())
				}
			}
		}
	}
}

// Cancellation before frame dispatch must remain visible, and panic ownership
// must be identical to the uninstrumented handler's unwind.
func TestControlHttpObservationCancellationAndPanicRelease(t *testing.T) {
	metrics := newControlHttpMetrics(prometheus.NewRegistry())
	ctx, cancel := context.WithCancel(context.Background())
	req, finish := observeConnectControl(httptest.NewRequest(http.MethodPost, "/synthetic", nil).WithContext(ctx), metrics)
	sentinel := new(int)
	func() {
		defer func() {
			if recovered := recover(); recovered != sentinel {
				t.Fatalf("cancellation changed panic: %v", recovered)
			}
		}()
		defer finish()
		advanceControlHttpPhase(req, controlHttpAuthenticate)
		cancel()
		panic(sentinel)
	}()
	if got := testutil.ToFloat64(metrics.completions.WithLabelValues("authenticate", "canceled")); got != 1 {
		t.Fatalf("pre-controller cancellation = %v", got)
	}
	_, finish = observeConnectControl(httptest.NewRequest(http.MethodPost, "/synthetic", nil), metrics)
	func() {
		defer func() {
			if recovered := recover(); recovered != sentinel {
				t.Fatalf("live panic changed: %v", recovered)
			}
		}()
		defer finish()
		panic(sentinel)
	}()
	if got := testutil.ToFloat64(metrics.completions.WithLabelValues("prepare", "panic")); got != 1 {
		t.Fatalf("prepare panic = %v", got)
	}
	for _, phase := range controlHttpPhases {
		if got := testutil.ToFloat64(metrics.inflight.WithLabelValues(phase)); got != 0 {
			t.Fatalf("%s leaked inflight = %v", phase, got)
		}
	}
}

// Exercise the real shared wrapper with invalid input and absent credentials;
// neither rejection should reach a model or change the existing HTTP contract.
func TestControlHttpObservationRealWrapperRejections(t *testing.T) {
	metrics := newControlHttpMetrics(prometheus.NewRegistry())
	for _, fixture := range []struct {
		body  string
		phase string
		code  int
	}{
		{"{", "prepare", http.StatusBadRequest},
		{"{}", "authenticate", http.StatusUnauthorized},
	} {
		req, finish := observeConnectControl(httptest.NewRequest(http.MethodPost, "/synthetic", strings.NewReader(fixture.body)), metrics)
		writer := httptest.NewRecorder()
		func() {
			defer finish()
			WrapWithInputRequireClient(func(map[string]any, *session.ClientSession) (bool, error) {
				t.Fatal("rejected request reached controller")
				return false, nil
			}, writer, req)
		}()
		if writer.Code != fixture.code {
			t.Fatalf("status = %d, want %d", writer.Code, fixture.code)
		}
		if got := testutil.ToFloat64(metrics.completions.WithLabelValues(fixture.phase, "rejected")); got != 1 {
			t.Fatalf("%s rejection = %v", fixture.phase, got)
		}
	}
}

// Ordinary routes do not opt in. All cells exist even on a quiet process.
func TestControlHttpObservationQuietAndUnobservedRoutes(t *testing.T) {
	registry := prometheus.NewRegistry()
	metrics := newControlHttpMetrics(registry)
	req := httptest.NewRequest(http.MethodPost, "/synthetic", nil)
	advanceControlHttpPhase(req, controlHttpAuthenticate)
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	children := 0
	for _, family := range families {
		children += len(family.Metric)
		for _, metric := range family.Metric {
			if len(metric.Label) > 2 || metric.Counter.GetValue() != 0 || metric.Gauge.GetValue() != 0 || metric.Summary.GetSampleCount() != 0 {
				t.Fatalf("unexpected quiet metric: %v", metric)
			}
		}
	}
	if children != 24 || testutil.ToFloat64(metrics.inflight.WithLabelValues("authenticate")) != 0 {
		t.Fatalf("bounded quiet children = %d", children)
	}
}
