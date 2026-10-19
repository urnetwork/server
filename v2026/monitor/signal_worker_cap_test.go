package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

type workerCapFixture struct {
	host, block, instance              string
	cpu, maxprocs, goroutines          float64
	cpuAge, maxprocsAge, goroutinesAge time.Duration
	omitMaxprocs                       bool
}

func workerCapFixtureJSON(t *testing.T, now time.Time, workers ...workerCapFixture) string {
	t.Helper()
	result := []any{}
	for _, worker := range workers {
		for _, metric := range []struct {
			name  string
			value float64
			age   time.Duration
			omit  bool
		}{
			{"cpu", worker.cpu, worker.cpuAge, false},
			{"maxprocs", worker.maxprocs, worker.maxprocsAge, worker.omitMaxprocs},
			{"goroutines", worker.goroutines, worker.goroutinesAge, false},
		} {
			if metric.omit {
				continue
			}
			result = append(result, map[string]any{
				"metric": map[string]string{
					"monitor_metric": metric.name,
					"host":           worker.host, "block": worker.block, "instance": worker.instance,
				},
				"value": []any{float64(now.Add(-metric.age).Unix()), fmt.Sprintf("%.6f", metric.value)},
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

func workerCapSyntheticSettings(t *testing.T, now time.Time, workers ...workerCapFixture) SignalSettings {
	t.Helper()
	payload := workerCapFixtureJSON(t, now, workers...)
	source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		if host.Name != "metrics-1" || !strings.Contains(command, "go_sched_gomaxprocs_threads") ||
			!strings.Contains(command, "process_cpu_seconds_total") ||
			!strings.Contains(command, "go_goroutines") ||
			!strings.Contains(command, "timestamp%28") {
			t.Fatalf("worker-cap query did not require exact source-fresh metrics: %s %s", host.Name, command)
		}
		return payload, nil
	}}
	return workerMemorySyntheticSettings(source, now)
}

func TestWorkerCapSignalDetectsProcessLimitDespiteHostHeadroom(t *testing.T) {
	now := time.Date(2026, 9, 26, 22, 0, 0, 0, time.UTC)
	settings := workerCapSyntheticSettings(t, now,
		workerCapFixture{host: "edge-a", block: "g1", instance: "busy", cpu: 3.71, maxprocs: 4, goroutines: 105_000},
		workerCapFixture{host: "edge-b", block: "g2", instance: "spare", cpu: 2.5, maxprocs: 4, goroutines: 105_000},
	)
	alerts, err := NewWorkerCapSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "worker-scheduler-capacity")
	if alert.Target != "edge-a/g1" || alert.Frame != "busy" || alert.Sustain != 2 {
		t.Fatalf("wrong scheduler-capacity identity: %+v", alert)
	}
	if requireAlertClassCount(alerts, "worker-scheduler-capacity") != 1 ||
		requireAlertClassCount(alerts, "worker-scheduler-unobservable") != 0 {
		t.Fatalf("healthy sibling or complete metrics misclassified: %+v", alerts)
	}
	markdown := alert.Markdown()
	for _, want := range []string{"cpu_cores_5m=3.710", "gomaxprocs=4", "goroutines=105000", "gVisor", "§2.19"} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("scheduler finding omitted %q", want)
		}
	}
}

func TestWorkerCapSignalMissingOrStaleMetricIsNotHealthy(t *testing.T) {
	now := time.Date(2026, 9, 26, 22, 0, 0, 0, time.UTC)
	settings := workerCapSyntheticSettings(t, now,
		workerCapFixture{host: "edge-a", block: "g1", instance: "missing", cpu: 3.9, goroutines: 105_000, omitMaxprocs: true},
		workerCapFixture{host: "edge-b", block: "g2", instance: "stale", cpu: 3.9, maxprocs: 4, goroutines: 105_000, maxprocsAge: 2 * time.Minute},
	)
	alerts, err := NewWorkerCapSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	visibility := requireAlertClass(t, alerts, "worker-scheduler-unobservable")
	if !strings.Contains(visibility.Markdown(), "paired_runtimes=0 incomplete_runtimes=2") ||
		requireAlertClassCount(alerts, "worker-scheduler-capacity") != 0 {
		t.Fatalf("missing or stale maxprocs was treated as capacity evidence: %+v", alerts)
	}
}

func TestWorkerCapSignalRejectsUnsafeIdentity(t *testing.T) {
	now := time.Date(2026, 9, 26, 22, 0, 0, 0, time.UTC)
	settings := workerCapSyntheticSettings(t, now,
		workerCapFixture{host: "edge-a", block: "g1", instance: "bad/identity", cpu: 3.9, maxprocs: 4, goroutines: 105_000},
	)
	alerts, err := NewWorkerCapSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	if requireAlertClassCount(alerts, "worker-scheduler-capacity") != 0 ||
		!strings.Contains(requireAlertClass(t, alerts, "worker-scheduler-unobservable").Markdown(), "paired_runtimes=0") {
		t.Fatalf("unsafe runtime identity entered the finding: %+v", alerts)
	}
}

func TestWorkerCapSignalRejectsDuplicateRuntimeSeries(t *testing.T) {
	now := time.Date(2026, 9, 26, 22, 0, 0, 0, time.UTC)
	runtime := workerCapFixture{
		host: "edge-a", block: "g1", instance: "duplicate",
		cpu: 3.9, maxprocs: 4, goroutines: 105_000,
	}
	settings := workerCapSyntheticSettings(t, now, runtime, runtime)
	alerts, err := NewWorkerCapSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	if requireAlertClassCount(alerts, "worker-scheduler-capacity") != 0 ||
		!strings.Contains(requireAlertClass(t, alerts, "worker-scheduler-unobservable").Markdown(), "incomplete_runtimes=1") {
		t.Fatalf("ambiguous same-instance series were accepted: %+v", alerts)
	}
}
