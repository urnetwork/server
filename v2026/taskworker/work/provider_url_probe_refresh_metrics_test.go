package work

import (
	"context"
	"errors"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/urnetwork/server/v2026/model"
)

func TestUrlProbeFleetRefreshObservationResultsAndRetainedGenerations(t *testing.T) {
	for _, test := range []struct {
		name, result string
		panicRead    bool
	}{
		{"success", "success", false}, {"deadline", "context_deadline", false},
		{"deadline_panic", "context_deadline", true}, {"canceled", "context_canceled", false},
		{"canceled_panic", "context_canceled", true}, {"read_error", "read_error", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			registry := prometheus.NewPedanticRegistry()
			metrics := newProviderUrlProbeFleetRefreshCollectors(registry)
			collector := newProviderUrlProbeFleetCollector()
			collector.refreshMetrics = metrics
			previous := &providerUrlProbeFleetSnapshot{fleet: model.ProviderUrlProbeFleet{Eligible: 1}, observedAt: time.Unix(1, 0)}
			collector.snapshot.Store(previous)
			ctx := t.Context()
			if strings.HasPrefix(test.name, "deadline") {
				var cancel context.CancelFunc
				ctx, cancel = context.WithDeadline(ctx, time.Now().Add(-time.Second))
				defer cancel()
			} else if strings.HasPrefix(test.name, "canceled") {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			calls := 0
			observedAt := time.Unix(2, 0)
			var ownedContext context.Context
			err := collector.refresh(ctx, observedAt, func(snapshotCtx context.Context, comparisonAt time.Time) model.ProviderUrlProbeFleet {
				ownedContext = snapshotCtx
				calls++
				deadline, present := snapshotCtx.Deadline()
				if !present || time.Until(deadline) > 10*time.Second || comparisonAt != observedAt {
					t.Fatal("refresh changed deadline or comparison clock")
				}
				if test.panicRead {
					panic(errors.New("private-provider.example/raw-query-secret"))
				}
				return model.ProviderUrlProbeFleet{Eligible: 99}
			})
			if calls != 1 || (test.result == "success") != (err == nil) {
				t.Fatalf("refresh semantics changed: calls=%d error=%v", calls, err)
			}
			if ownedContext.Err() == nil {
				t.Fatal("refresh leaked its owned context")
			}
			current := collector.snapshot.Load()
			if test.result == "success" {
				if current == previous || current.fleet.Eligible != 99 || current.observedAt != observedAt {
					t.Fatal("successful refresh did not publish one complete generation")
				}
			} else if current != previous {
				t.Fatal("failed refresh advanced census counts or freshness")
			}
			for _, result := range urlProbeFleetRefreshResultLabels {
				count := testutil.ToFloat64(metrics.results.WithLabelValues(result))
				duration := testutil.ToFloat64(metrics.seconds.WithLabelValues(result))
				want := 0.0
				if result == test.result {
					want = 1
				}
				if count != want || duration < 0 || math.IsNaN(duration) || math.IsInf(duration, 0) || (want == 1 && duration <= 0) || (want == 0 && duration != 0) {
					t.Fatalf("result%s count=%v duration=%v want count%v", result, count, duration, want)
				}
			}
			if count, err := testutil.GatherAndCount(registry); err != nil || count != 9 {
				t.Fatalf("refresh series escaped fixed domain: %d %v", count, err)
			}
			families, err := registry.Gather()
			if err != nil {
				t.Fatal(err)
			}
			for _, family := range families {
				for _, metric := range family.Metric {
					if strings.Contains(metric.String(), "private-provider") || strings.Contains(metric.String(), "raw-query-secret") {
						t.Fatal("read error escaped into telemetry")
					}
				}
			}
		})
	}
}

func TestUrlProbeFleetRefreshObservationIdleDomainDoesNotInventCensus(t *testing.T) {
	registry := prometheus.NewPedanticRegistry()
	metrics := newProviderUrlProbeFleetRefreshCollectors(registry)
	collector := newProviderUrlProbeFleetCollector()
	collector.refreshMetrics = metrics
	registry.MustRegister(collector)
	if count, err := testutil.GatherAndCount(registry); err != nil || count != 9 {
		t.Fatalf("idle producer invented census or omitted capability: count%d err%v", count, err)
	}
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		for _, metric := range family.Metric {
			if family.GetName() == "urnetwork_url_probe_fleet_refresh_observation_enabled" {
				if len(metric.Label) != 0 || metric.Gauge.GetValue() != 1 {
					t.Fatal("capability gained labels or wrong value")
				}
				continue
			}
			if family.GetName() != "urnetwork_url_probe_fleet_refresh_total" && family.GetName() != "urnetwork_url_probe_fleet_refresh_seconds_total" {
				t.Fatal("idle observations invented a fleet generation")
			}
			if len(metric.Label) != 1 || metric.Label[0].GetName() != "result" || metric.Counter.GetValue() != 0 {
				t.Fatal("invalid idle refresh label/value")
			}
			result := metric.Label[0].GetValue()
			if result != "success" && result != "context_deadline" && result != "context_canceled" && result != "read_error" {
				t.Fatal("unbounded result label")
			}
		}
	}
}

func TestUrlProbeFleetRefreshObservationGuardSkipsConcurrentOwners(t *testing.T) {
	// A guard already claimed by an actual refresh must exclude all overlapping
	// owners. No read seam or timer is changed to produce this existing state.
	urlProbeFleetRefresh.Lock()
	previous := urlProbeFleetRefresh.last
	guardAt := time.Now()
	urlProbeFleetRefresh.last = guardAt
	urlProbeFleetRefresh.Unlock()
	t.Cleanup(func() {
		urlProbeFleetRefresh.Lock()
		urlProbeFleetRefresh.last = previous
		urlProbeFleetRefresh.Unlock()
	})
	before := make([]float64, len(urlProbeFleetRefreshResultLabels))
	for index, result := range urlProbeFleetRefreshResultLabels {
		before[index] = testutil.ToFloat64(urlProbeFleetRefreshMetrics.results.WithLabelValues(result))
	}
	snapshot := urlProbeFleetMetrics.snapshot.Load()
	var owners sync.WaitGroup
	for range 16 {
		owners.Add(1)
		go func() {
			defer owners.Done()
			refreshProviderUrlProbeFleetMetrics(t.Context())
		}()
	}
	owners.Wait()
	urlProbeFleetRefresh.Lock()
	last := urlProbeFleetRefresh.last
	urlProbeFleetRefresh.Unlock()
	if last != guardAt || urlProbeFleetMetrics.snapshot.Load() != snapshot {
		t.Fatal("overlapping guard skips advanced producer state")
	}
	for index, result := range urlProbeFleetRefreshResultLabels {
		if testutil.ToFloat64(urlProbeFleetRefreshMetrics.results.WithLabelValues(result)) != before[index] {
			t.Fatal("guard skip was counted as an actual refresh")
		}
	}
}
