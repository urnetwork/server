// Telemetry observes the existing readiness result without adding another
// resource read, catalog query, retry or financial decision.
package model

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestLegacyPayerReadinessMetricsKeepFiniteProbeOutcomes(t *testing.T) {
	counter := newLegacyPayerReadinessCounter()
	for _, test := range []struct {
		outcome string
		ready   bool
		err     error
	}{
		{outcome: "ready", ready: true},
		{outcome: "catalog_invalid"},
		{outcome: "deadline", err: context.DeadlineExceeded},
		{outcome: "canceled", err: context.Canceled},
		{outcome: "read_error", err: errors.New("synthetic private resource detail")},
	} {
		reads := 0
		observed := observeLegacySettlementPayerDueIndex(t.Context(), func(context.Context) (bool, error) {
			reads++
			return test.ready, test.err
		}, func() time.Time { return time.Unix(100, 0) })
		recordLegacyPayerReadiness(counter, "payer", observed)
		if observed.Outcome != test.outcome || reads != 1 || testutil.ToFloat64(counter.WithLabelValues("payer", test.outcome, "false")) != 1 {
			t.Fatal("readiness telemetry changed the existing probe or its outcome", test.outcome, reads, observed)
		}
	}
	cachedReads := 0
	cached := observeLegacySettlementPayerIndexWithCache(t.Context(), func(context.Context) bool { return true }, func(context.Context) (bool, error) {
		cachedReads++
		return false, context.DeadlineExceeded
	}, time.Now)
	recordLegacyPayerReadiness(counter, "dispatcher", cached)
	if cachedReads != 0 || testutil.ToFloat64(counter.WithLabelValues("dispatcher", "ready", "true")) != 1 {
		t.Fatal("cached readiness telemetry performed a fresh read")
	}
}

func TestLegacyPayerReadinessMetricsDoNotExposeUnexpectedLabels(t *testing.T) {
	counter := newLegacyPayerReadinessCounter()
	recordLegacyPayerReadiness(counter, "synthetic private caller", LegacySettlementPayerIndexReadiness{
		Outcome: "synthetic private error", Cached: true,
	})
	if testutil.ToFloat64(counter.WithLabelValues("unknown", "unknown", "false")) != 1 {
		t.Fatal("unexpected readiness fields were not bounded")
	}
	registry := prometheus.NewPedanticRegistry()
	registry.MustRegister(counter)
	families, err := registry.Gather()
	if err != nil || len(families) != 1 || families[0].GetName() != "urnetwork_legacy_payer_index_readiness_total" {
		t.Fatal("readiness metric exposition failed", err)
	}
	for _, metric := range families[0].Metric {
		if len(metric.Label) != 3 {
			t.Fatal("readiness retained fields beyond caller/outcome/cache")
		}
		for _, label := range metric.Label {
			switch label.GetValue() {
			case "dispatcher", "payer", "unknown", "ready", "catalog_invalid", "deadline", "canceled", "read_error", "true", "false":
			default:
				t.Fatal("readiness exposed a nonfinite label")
			}
		}
	}
}
