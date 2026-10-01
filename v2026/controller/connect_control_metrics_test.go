package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/urnetwork/connect/v2026/protocol"
)

// One blocked decoded frame has its own gauge and denominator; requests that
// never finish authentication cannot be mistaken for successful provide calls.
func TestControlFrameMetricsSeparateHttpHandlerResidence(t *testing.T) {
	metrics := newControlFrameMetrics(prometheus.NewRegistry())
	now := time.Unix(1_700_000_000, 0)
	metrics.now = func() time.Time { return now }
	ctx := context.WithValue(context.Background(), controlHttpIngressKey{}, true)
	err := observeControlFrame(ctx, &protocol.Provide{}, metrics, func() error {
		if got := testutil.ToFloat64(metrics.inflight.WithLabelValues("http", "provide")); got != 1 {
			t.Fatalf("provide inflight = %v", got)
		}
		now = now.Add(12 * time.Second)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := testutil.ToFloat64(metrics.completions.WithLabelValues("http", "provide", "handler_ok")); got != 1 {
		t.Fatalf("HTTP handler completion = %v", got)
	}
	if got := testutil.ToFloat64(metrics.completions.WithLabelValues("internal", "provide", "handler_ok")); got != 0 {
		t.Fatalf("HTTP work leaked into internal cohort = %v", got)
	}
	if got := testutil.ToFloat64(metrics.inflight.WithLabelValues("http", "provide")); got != 0 {
		t.Fatalf("provide retained after completion = %v", got)
	}
}

// Live panics remain per-frame errors; canceled-context panics still propagate
// unchanged. Both release the gauge and record exactly one completed handler.
func TestControlFrameMetricsPreservePanicAndCancellationOwnership(t *testing.T) {
	metrics := newControlFrameMetrics(prometheus.NewRegistry())
	sentinel := new(int)
	err := observeControlFrame(context.Background(), &protocol.CreateContract{}, metrics, func() error { panic(sentinel) })
	if err == nil || !strings.Contains(err.Error(), "panicked") {
		t.Fatalf("live panic not converted to frame error: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	func() {
		defer func() {
			if recovered := recover(); recovered != sentinel {
				t.Fatalf("cancellation panic changed: %v", recovered)
			}
		}()
		_ = observeControlFrame(ctx, &protocol.CreateContract{}, metrics, func() error {
			cancel()
			panic(sentinel)
		})
	}()
	for _, outcome := range []string{"panic", "canceled"} {
		if got := testutil.ToFloat64(metrics.completions.WithLabelValues("internal", "create_contract", outcome)); got != 1 {
			t.Fatalf("%s completion = %v", outcome, got)
		}
	}
	if got := testutil.ToFloat64(metrics.inflight.WithLabelValues("internal", "create_contract")); got != 0 {
		t.Fatalf("panic leaked inflight = %v", got)
	}
}

// Arbitrary message/error contents cannot create labels. Successful handler
// return is deliberately not named contract success or acknowledgement.
func TestControlFrameMetricsBoundedErrorsAndQuietCells(t *testing.T) {
	registry := prometheus.NewRegistry()
	metrics := newControlFrameMetrics(registry)
	sentinel := errors.New("synthetic-sensitive-detail")
	if err := observeControlFrame(context.Background(), struct{ Secret string }{"synthetic-sensitive-detail"}, metrics, func() error { return sentinel }); err != sentinel {
		t.Fatalf("handler error changed: %v", err)
	}
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	children := 0
	for _, family := range families {
		children += len(family.Metric)
		for _, metric := range family.Metric {
			for _, label := range metric.Label {
				if strings.Contains(label.GetValue(), "synthetic-sensitive-detail") {
					t.Fatalf("unbounded metric label: %v", metric)
				}
			}
		}
	}
	if children != 96 {
		t.Fatalf("bounded children = %d, want 96", children)
	}
	if got := testutil.ToFloat64(metrics.completions.WithLabelValues("internal", "other", "error")); got != 1 {
		t.Fatalf("unknown message error = %v", got)
	}
}
