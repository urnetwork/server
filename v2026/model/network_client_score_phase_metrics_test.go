package model

import (
	"bytes"
	"encoding/gob"
	"errors"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/urnetwork/server/v2026"
)

func gatherUpdateClientScoresPhaseMetric(
	t *testing.T,
	registry *prometheus.Registry,
	name string,
	phase string,
) float64 {
	t.Helper()
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.Metric {
			if len(metric.Label) == 1 && metric.Label[0].GetName() == "phase" && metric.Label[0].GetValue() == phase {
				if metric.Gauge != nil {
					return metric.Gauge.GetValue()
				}
				if metric.Counter != nil {
					return metric.Counter.GetValue()
				}
			}
		}
	}
	t.Fatalf("metric %s phase %s not found", name, phase)
	return 0
}

func registerUpdateClientScoresPhaseMetrics(t *testing.T, metrics *updateClientScoresPhaseMetricSet) *prometheus.Registry {
	t.Helper()
	registry := prometheus.NewRegistry()
	for _, collector := range metrics.collectors() {
		if err := registry.Register(collector); err != nil {
			t.Fatal(err)
		}
	}
	return registry
}

func TestUpdateClientScoresPhaseMetricsHaveFixedCardinality(t *testing.T) {
	metrics := newUpdateClientScoresPhaseMetricSet(func() time.Time { return time.Unix(100, 0) })
	registry := registerUpdateClientScoresPhaseMetrics(t, metrics)
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(families), 5; got != want {
		t.Fatalf("metric families = %d, want %d", got, want)
	}
	wantPhases := append([]string(nil), updateClientScoresPhaseNames[:]...)
	sort.Strings(wantPhases)
	for _, family := range families {
		phases := make([]string, 0, len(family.Metric))
		for _, metric := range family.Metric {
			if len(metric.Label) != 1 || metric.Label[0].GetName() != "phase" {
				t.Fatalf("%s has non-phase labels: %+v", family.GetName(), metric.Label)
			}
			phases = append(phases, metric.Label[0].GetValue())
		}
		sort.Strings(phases)
		if fmt.Sprint(phases) != fmt.Sprint(wantPhases) {
			t.Fatalf("%s phases = %v, want fixed set %v", family.GetName(), phases, wantPhases)
		}
	}
}

func TestUpdateClientScoresPhaseSpanRecordsAndFinishesOnce(t *testing.T) {
	times := []time.Time{time.Unix(100, 0), time.Unix(102, 250_000_000)}
	metrics := newUpdateClientScoresPhaseMetricSet(func() time.Time {
		value := times[0]
		times = times[1:]
		return value
	})
	registry := registerUpdateClientScoresPhaseMetrics(t, metrics)
	span := metrics.start(updateClientScoresPhaseGobEncode)
	phase := updateClientScoresPhaseNames[updateClientScoresPhaseGobEncode]
	if active := gatherUpdateClientScoresPhaseMetric(t, registry, "urnetwork_update_client_scores_phase_active", phase); active != 1 {
		t.Fatalf("gob phase active after start = %v, want 1", active)
	}
	metrics.addWork(updateClientScoresPhaseGobEncode, 3, 2048)
	span.finish()
	span.finish()

	for name, want := range map[string]float64{
		"urnetwork_update_client_scores_phase_active":                 0,
		"urnetwork_update_client_scores_phase_duration_seconds_total": 2.25,
		"urnetwork_update_client_scores_phase_exits_total":            1,
		"urnetwork_update_client_scores_phase_work_items_total":       3,
		"urnetwork_update_client_scores_phase_work_bytes_total":       2048,
	} {
		if got := gatherUpdateClientScoresPhaseMetric(t, registry, name, phase); got != want {
			t.Fatalf("%s = %v, want %v", name, got, want)
		}
	}
}

func TestClientScoreTargetTelemetryCleansUpOnErrorAndPanic(t *testing.T) {
	for _, test := range []struct {
		name      string
		encode    clientScoreTargetEncode
		emit      func(clientScoreRedisSet) error
		wantPanic bool
	}{
		{
			name: "error",
			encode: func(map[server.Id]*ClientScore) clientScoreExportPayload {
				return unfacetedExportPayloadForTest(
					[]byte("count"),
					[]byte("filter"),
					nil,
					func(int) []byte { return nil },
				)
			},
			emit: func(clientScoreRedisSet) error { return errors.New("synthetic emit failure") },
		},
		{
			name: "panic",
			encode: func(map[server.Id]*ClientScore) clientScoreExportPayload {
				panic("synthetic encode panic")
			},
			emit:      func(clientScoreRedisSet) error { return nil },
			wantPanic: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			metrics := newUpdateClientScoresPhaseMetricSet(func() time.Time { return time.Unix(100, 0) })
			registry := registerUpdateClientScoresPhaseMetrics(t, metrics)
			panicked := false
			func() {
				defer func() { panicked = recover() != nil }()
				_ = emitClientScoreTargetFanoutWithMetrics(
					nil,
					nil,
					nil,
					clientScoreTargetKeys{
						counts: func(server.Id) string { return "count" },
						filter: func(server.Id) string { return "filter" },
						sample: func(server.Id, int) string { return "sample" },
						alias:  func(server.Id) string { return "alias" },
						facetCounts: func(server.Id, ipFamilyFacet) string {
							return "facet-count"
						},
						facetSample: func(server.Id, ipFamilyFacet, int) string {
							return "facet-sample"
						},
					},
					test.encode,
					false,
					false,
					test.emit,
					metrics,
				)
			}()
			if panicked != test.wantPanic {
				t.Fatalf("panicked = %t, want %t", panicked, test.wantPanic)
			}
			for _, phase := range []updateClientScoresPhase{
				updateClientScoresPhaseTargetExport,
				updateClientScoresPhaseTargetMap,
			} {
				name := updateClientScoresPhaseNames[phase]
				if active := gatherUpdateClientScoresPhaseMetric(t, registry, "urnetwork_update_client_scores_phase_active", name); active != 0 {
					t.Fatalf("phase %s remained active after %s: %v", name, test.name, active)
				}
				if exits := gatherUpdateClientScoresPhaseMetric(t, registry, "urnetwork_update_client_scores_phase_exits_total", name); exits != 1 {
					t.Fatalf("phase %s exits after %s = %v, want 1", name, test.name, exits)
				}
			}
		})
	}
}

func TestClientScoreGobTelemetryPreservesBytesAndIsConcurrentSafe(t *testing.T) {
	value := []string{"alpha", "beta", "gamma"}
	var expected bytes.Buffer
	if err := gob.NewEncoder(&expected).Encode(value); err != nil {
		t.Fatal(err)
	}
	metrics := newUpdateClientScoresPhaseMetricSet(func() time.Time { return time.Unix(100, 0) })
	registry := registerUpdateClientScoresPhaseMetrics(t, metrics)

	const workers = 48
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if got := encodeClientScoreGobValue(metrics, value); !bytes.Equal(got, expected.Bytes()) {
				t.Errorf("instrumented gob bytes changed")
			}
		}()
	}
	wg.Wait()

	phase := updateClientScoresPhaseNames[updateClientScoresPhaseGobEncode]
	if active := gatherUpdateClientScoresPhaseMetric(t, registry, "urnetwork_update_client_scores_phase_active", phase); active != 0 {
		t.Fatalf("gob phase active = %v after all workers exited", active)
	}
	if exits := gatherUpdateClientScoresPhaseMetric(t, registry, "urnetwork_update_client_scores_phase_exits_total", phase); exits != workers {
		t.Fatalf("gob phase exits = %v, want %d", exits, workers)
	}
	if workBytes := gatherUpdateClientScoresPhaseMetric(t, registry, "urnetwork_update_client_scores_phase_work_bytes_total", phase); workBytes != float64(workers*expected.Len()) {
		t.Fatalf("gob work bytes = %v, want %d", workBytes, workers*expected.Len())
	}
}
