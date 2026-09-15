package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

type serviceLoadFixture struct {
	host       string
	service    string
	block      string
	instance   string
	rss        float64
	heap       float64
	objects    float64
	goroutines float64
	start      float64
	cpu        float64
	allocation float64
	gc         float64
	age        time.Duration
	omit       string
}

func serviceLoadFixtureJSON(t testing.TB, now time.Time, hostCores map[string]float64, fixtures ...serviceLoadFixture) string {
	t.Helper()
	result := []any{}
	for host, cores := range hostCores {
		result = append(result, map[string]any{
			"metric": map[string]string{
				"env": "synthetic", "host": host, "job": "node", "monitor_metric": "host_cores",
			},
			"value": []any{float64(now.Unix()), fmt.Sprintf("%.9f", cores)},
		})
	}
	for _, fixture := range fixtures {
		metrics := map[string]float64{
			"process_resident_memory_bytes": fixture.rss,
			"go_memstats_heap_alloc_bytes":  fixture.heap,
			"go_memstats_heap_objects":      fixture.objects,
			"go_goroutines":                 fixture.goroutines,
			"process_start_time_seconds":    fixture.start,
			"cpu":                           fixture.cpu,
			"allocation":                    fixture.allocation,
			"gc":                            fixture.gc,
		}
		for metric, value := range metrics {
			if metric == fixture.omit {
				continue
			}
			result = append(result, map[string]any{
				"metric": map[string]string{
					"env":            "synthetic",
					"host":           fixture.host,
					"job":            fixture.service,
					"service":        fixture.service,
					"block":          fixture.block,
					"instance":       fixture.instance,
					"monitor_metric": metric,
				},
				"value": []any{float64(now.Add(-fixture.age).Unix()), fmt.Sprintf("%.9f", value)},
			})
		}
	}
	payload, err := json.Marshal(map[string]any{
		"status": "success",
		"data":   map[string]any{"resultType": "vector", "result": result},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(payload)
}

func serviceLoadSettings(t testing.TB, now time.Time, payload string) SignalSettings {
	t.Helper()
	source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		if host.Name != "compute-a.example.test" || !strings.Contains(command, "process_resident_memory_bytes") ||
			!strings.Contains(command, "go_goroutines") || !strings.Contains(command, "process_cpu_seconds_total") ||
			!strings.Contains(command, "node_cpu_seconds_total") {
			t.Fatalf("unexpected service-load command on %s: %s", host.Name, command)
		}
		return payload, nil
	}}
	return SignalSettings{
		Environment: "synthetic",
		LogServices: []string{"relay"},
		Source:      source,
		Now:         func() time.Time { return now },
		Hosts: []HostSettings{
			{Name: "compute-a.example.test", Roles: []string{"services"}},
			{Name: "compute-b.example.test", Roles: []string{"services"}},
		},
	}
}

func TestServiceLoadSignalSyntheticRunawayAndOverlap(t *testing.T) {
	now := time.Date(2026, 9, 15, 15, 10, 0, 0, time.UTC)
	const gib = float64(uint64(1) << 30)
	payload := serviceLoadFixtureJSON(t, now,
		map[string]float64{"compute-a.example.test": 72, "compute-b.example.test": 72},
		serviceLoadFixture{
			host: "compute-a.example.test", service: "relay", block: "g1", instance: "current-a",
			rss: 82 * gib, heap: 47 * gib, objects: 61_000_000, goroutines: 915_000,
			start: float64(now.Add(-10 * time.Hour).Unix()), cpu: 10.8, allocation: 1600 << 20, gc: 0.05,
		},
		serviceLoadFixture{
			host: "compute-a.example.test", service: "relay", block: "g1", instance: "replacement-a",
			rss: 2 * gib, heap: 1 * gib, objects: 2_000_000, goroutines: 30_000,
			start: float64(now.Add(-5 * time.Minute).Unix()), cpu: 0.4, allocation: 8 << 20, gc: 0.01,
		},
		serviceLoadFixture{
			host: "compute-b.example.test", service: "relay", block: "g2", instance: "current-b",
			rss: 3 * gib, heap: 2 * gib, objects: 3_000_000, goroutines: 40_000,
			start: float64(now.Add(-2 * time.Hour).Unix()), cpu: 0.6, allocation: 10 << 20, gc: 0.01,
		},
	)
	alerts, err := NewServiceLoadSignal().Run(context.Background(), serviceLoadSettings(t, now, payload))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "service-runtime-runaway")
	if alert.Target != "compute-a.example.test/relay/g1" || alert.Frame != "current-a" || alert.Severity != SeverityPage || alert.Sustain != 2 {
		t.Fatalf("wrong runaway identity: %+v", alert)
	}
	for _, want := range []string{
		"rss_bytes=88046829568",
		"goroutines=915000",
		"cpu_cores_5m=10.800",
		"alloc_bytes_per_s_5m=1677721600",
		"same_block_generations=2",
		"Hard-terminate only a proven-stuck old generation",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("runaway alert omitted %q: %s", want, alert.Markdown())
		}
	}
}

func TestServiceLoadQueryEnforcesSourceSampleFreshness(t *testing.T) {
	query := serviceLoadQuery("synthetic", []string{"relay"})
	if strings.Count(query, `timestamp(`) != 9 ||
		strings.Count(query, `>= time() - 90`) != 9 {
		t.Fatalf("service-load query does not enforce source-sample freshness for every input: %s", query)
	}
	for _, metric := range []string{
		"process_resident_memory_bytes",
		"go_memstats_heap_alloc_bytes",
		"go_memstats_heap_objects",
		"go_goroutines",
		"process_start_time_seconds",
		"process_cpu_seconds_total",
		"go_memstats_alloc_bytes_total",
		"go_gc_duration_seconds_count",
		"node_cpu_seconds_total",
	} {
		if !strings.Contains(query, `timestamp(`+metric+`{`) {
			t.Fatalf("service-load query omits source freshness for %s: %s", metric, query)
		}
	}
}

func TestServiceLoadSignalSyntheticHardCeilingWithoutRates(t *testing.T) {
	now := time.Date(2026, 9, 15, 15, 11, 0, 0, time.UTC)
	const gib = float64(uint64(1) << 30)
	payload := serviceLoadFixtureJSON(t, now,
		map[string]float64{"compute-a.example.test": 72},
		serviceLoadFixture{
			host: "compute-a.example.test", service: "relay", block: "g1", instance: "oversize",
			rss: 70 * gib, heap: 40 * gib, objects: 50_000_000, goroutines: 100_000,
			start: float64(now.Add(-30 * time.Minute).Unix()), omit: "cpu",
		},
	)
	alerts, err := NewServiceLoadSignal().Run(context.Background(), serviceLoadSettings(t, now, payload))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "service-runtime-runaway")
	if alert.Frame != "oversize" {
		t.Fatalf("hard RSS ceiling lost process identity: %+v", alert)
	}
}

func TestServiceLoadSignalSyntheticIncompleteRawFamily(t *testing.T) {
	now := time.Date(2026, 9, 15, 15, 12, 0, 0, time.UTC)
	const gib = float64(uint64(1) << 30)
	payload := serviceLoadFixtureJSON(t, now,
		map[string]float64{"compute-a.example.test": 16},
		serviceLoadFixture{
			host: "compute-a.example.test", service: "relay", block: "g1", instance: "partial",
			rss: 2 * gib, heap: 1 * gib, objects: 2_000_000, goroutines: 20_000,
			start: float64(now.Add(-time.Hour).Unix()), cpu: 0.2, allocation: 4 << 20, gc: 0.01,
			omit: "go_goroutines",
		},
	)
	alerts, err := NewServiceLoadSignal().Run(context.Background(), serviceLoadSettings(t, now, payload))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "service-runtime-metrics-incomplete")
	if alert.Target != "compute-a.example.test/relay/g1" || alert.Frame != "partial" {
		t.Fatalf("wrong incomplete process identity: %+v", alert)
	}
}

func TestServiceLoadSignalSyntheticInvalidLabelsAreReduced(t *testing.T) {
	now := time.Date(2026, 9, 15, 15, 13, 0, 0, time.UTC)
	const gib = float64(uint64(1) << 30)
	payload := serviceLoadFixtureJSON(t, now,
		map[string]float64{"compute-a.example.test": 16},
		serviceLoadFixture{
			host: "compute-a.example.test", service: "relay", block: "g1", instance: "healthy",
			rss: 2 * gib, heap: 1 * gib, objects: 2_000_000, goroutines: 20_000,
			start: float64(now.Add(-time.Hour).Unix()), cpu: 0.2, allocation: 4 << 20, gc: 0.01,
		},
		serviceLoadFixture{
			host: "compute-a.example.test", service: "relay", block: "invalid/block", instance: "private-value",
			rss: 80 * gib, heap: 50 * gib, objects: 90_000_000, goroutines: 900_000,
			start: float64(now.Add(-time.Hour).Unix()), cpu: 9, allocation: 2 << 30, gc: 0.1,
		},
	)
	alerts, err := NewServiceLoadSignal().Run(context.Background(), serviceLoadSettings(t, now, payload))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "service-runtime-metrics-invalid")
	if !strings.Contains(alert.Observed, "invalid_series=8") {
		t.Fatalf("invalid series were not reduced to a count: %+v", alert)
	}
	requireAlertOmits(t, alert, "invalid/block", "private-value")
}

func TestServiceLoadSignalSyntheticStaleRunawayIsIgnored(t *testing.T) {
	now := time.Date(2026, 9, 15, 15, 14, 0, 0, time.UTC)
	const gib = float64(uint64(1) << 30)
	payload := serviceLoadFixtureJSON(t, now,
		map[string]float64{"compute-a.example.test": 16},
		serviceLoadFixture{
			host: "compute-a.example.test", service: "relay", block: "g1", instance: "healthy",
			rss: 2 * gib, heap: 1 * gib, objects: 2_000_000, goroutines: 20_000,
			start: float64(now.Add(-time.Hour).Unix()), cpu: 0.2, allocation: 4 << 20, gc: 0.01,
		},
		serviceLoadFixture{
			host: "compute-a.example.test", service: "relay", block: "g2", instance: "stale",
			rss: 80 * gib, heap: 50 * gib, objects: 90_000_000, goroutines: 900_000,
			start: float64(now.Add(-time.Hour).Unix()), cpu: 9, allocation: 2 << 30, gc: 0.1,
			age: 2 * time.Minute,
		},
	)
	alerts, err := NewServiceLoadSignal().Run(context.Background(), serviceLoadSettings(t, now, payload))
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 0 {
		t.Fatalf("stale runaway contaminated current state: %+v", alerts)
	}
}

func TestServiceLoadSignalSyntheticHealthy(t *testing.T) {
	now := time.Date(2026, 9, 15, 15, 15, 0, 0, time.UTC)
	const gib = float64(uint64(1) << 30)
	payload := serviceLoadFixtureJSON(t, now,
		map[string]float64{"compute-a.example.test": 16},
		serviceLoadFixture{
			host: "compute-a.example.test", service: "relay", block: "g1", instance: "healthy",
			rss: 2 * gib, heap: 1 * gib, objects: 2_000_000, goroutines: 20_000,
			start: float64(now.Add(-time.Hour).Unix()), cpu: 0.2, allocation: 4 << 20, gc: 0.01,
		},
	)
	alerts, err := NewServiceLoadSignal().Run(context.Background(), serviceLoadSettings(t, now, payload))
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 0 {
		t.Fatalf("healthy service fixture alerted: %+v", alerts)
	}
}
