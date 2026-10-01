package server

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

type pgPoolMetricsFixtureSource struct {
	snapshot pgPoolMetricSnapshot
	ready    bool
}

func (self pgPoolMetricsFixtureSource) metricSnapshot() (pgPoolMetricSnapshot, bool) {
	return self.snapshot, self.ready
}

func TestPgPoolMetricsRemainAbsentUntilPoolIsUsed(t *testing.T) {
	collector := newPgPoolMetricsCollector(map[string]pgPoolMetricsSource{
		"default": pgPoolMetricsFixtureSource{},
	})
	registry := prometheus.NewPedanticRegistry()
	registry.MustRegister(collector)
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	if len(families) != 0 {
		t.Fatalf("unused PostgreSQL pool emitted %d metric families, want none", len(families))
	}
}

func TestPgPoolMetricsPublishFiniteCompleteSnapshot(t *testing.T) {
	collector := newPgPoolMetricsCollector(map[string]pgPoolMetricsSource{
		"default": pgPoolMetricsFixtureSource{
			ready: true,
			snapshot: pgPoolMetricSnapshot{
				acquiredConnections: 2,
				idleConnections:     3,
				totalConnections:    5,
				maximumConnections:  8,
				acquires:            13,
				emptyAcquires:       1,
				canceledAcquires:    2,
				acquireDuration:     3 * time.Second,
				newConnections:      7,
				lifetimeDestroyed:   4,
				idleDestroyed:       6,
			},
		},
	})
	registry := prometheus.NewPedanticRegistry()
	registry.MustRegister(collector)
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	metricCount := 0
	for _, family := range families {
		metricCount += len(family.Metric)
		for _, metric := range family.Metric {
			for _, label := range metric.Label {
				if label.GetName() != "pool" && label.GetName() != "state" && label.GetName() != "outcome" && label.GetName() != "reason" {
					t.Fatalf("unexpected PostgreSQL pool metric label %q", label.GetName())
				}
			}
		}
	}
	if metricCount != 11 {
		t.Fatalf("PostgreSQL pool metrics = %d, want 11 complete samples", metricCount)
	}
	assertPgPoolMetricValue(t, families, "urnetwork_pg_pool_connections", "state", "maximum", 8)
	assertPgPoolMetricValue(t, families, "urnetwork_pg_pool_acquires_total", "outcome", "canceled", 2)
}

// assertPgPoolMetricValue finds one finite-label pool series.
func assertPgPoolMetricValue(t *testing.T, families []*dto.MetricFamily, familyName string, labelName string, labelValue string, want float64) {
	t.Helper()
	for _, family := range families {
		if family.GetName() != familyName {
			continue
		}
		for _, metric := range family.Metric {
			matched := false
			for _, label := range metric.Label {
				matched = matched || label.GetName() == labelName && label.GetValue() == labelValue
			}
			if matched {
				got := metric.GetGauge().GetValue()
				if metric.Counter != nil {
					got = metric.GetCounter().GetValue()
				}
				if got != want {
					t.Fatalf("%s{%s=%q} = %v, want %v", familyName, labelName, labelValue, got, want)
				}
				return
			}
		}
	}
	t.Fatalf("%s{%s=%q} is absent", familyName, labelName, labelValue)
}
