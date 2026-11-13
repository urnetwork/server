package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
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

// A fully rejected returned tuple must survive beside a complete runtime.
func TestWorkerCapSignalRetainsFullyRejectedRuntimeCoverage(t *testing.T) {
	now := time.Date(2026, 9, 26, 22, 0, 0, 0, time.UTC)
	for _, rejected := range []struct {
		name  string
		value float64
		age   time.Duration
	}{
		{name: "stale", value: 1, age: 91 * time.Second},
		{name: "future", value: 1, age: -31 * time.Second},
		{name: "negative", value: -1},
		{name: "nan", value: math.NaN()},
		{name: "positive-infinity", value: math.Inf(1)},
		{name: "negative-infinity", value: math.Inf(-1)},
	} {
		for _, pressured := range []bool{false, true} {
			complete := workerCapFixture{host: "edge-a", block: "g1", instance: "complete", cpu: 1, maxprocs: 4, goroutines: 1000}
			if pressured {
				complete.cpu, complete.goroutines = 3.6, 10_000
			}
			settings := workerCapSyntheticSettings(t, now, complete,
				workerCapFixture{host: "edge-b", block: "g2", instance: "rejected-private", cpu: rejected.value, maxprocs: rejected.value, goroutines: rejected.value,
					cpuAge: rejected.age, maxprocsAge: rejected.age, goroutinesAge: rejected.age})
			env, err := newProbeEnv(settings.withDefaults())
			if err != nil {
				t.Fatal(err)
			}
			findings, err := (workerCapProbe{}).check(context.Background(), env)
			if err != nil {
				t.Fatalf("%s pressured=%t: %v", rejected.name, pressured, err)
			}
			visibility, capacity := 0, 0
			for _, finding := range findings {
				switch finding.class {
				case "worker-scheduler-unobservable":
					visibility++
					if finding.healthy || finding.observed != "paired_runtimes=1 incomplete_runtimes=1" || finding.sustain != 2 {
						t.Fatalf("%s pressured=%t: fully rejected runtime lost from visibility: %+v", rejected.name, pressured, finding)
					}
				case "worker-scheduler-capacity":
					capacity++
					if finding.target != "edge-a/g1" || finding.frame != "complete" || finding.healthy == pressured {
						t.Fatalf("%s pressured=%t: independent complete runtime changed: %+v", rejected.name, pressured, finding)
					}
				}
			}
			if visibility != 1 || capacity != 1 {
				t.Fatalf("%s pressured=%t: visibility=%d capacity=%d", rejected.name, pressured, visibility, capacity)
			}
			alerts, err := NewWorkerCapSignal().Run(context.Background(), settings)
			if err != nil {
				t.Fatal(err)
			}
			wantCapacity := 0
			if pressured {
				wantCapacity = 1
			}
			if requireAlertClassCount(alerts, "worker-scheduler-capacity") != wantCapacity || requireAlertClassCount(alerts, "worker-scheduler-unobservable") != 1 {
				t.Fatalf("%s pressured=%t: public findings changed", rejected.name, pressured)
			}
			if strings.Contains(requireAlertClass(t, alerts, "worker-scheduler-unobservable").Markdown(), "rejected-private") {
				t.Fatal("rejected runtime identity escaped aggregate visibility alert")
			}
		}
	}
}

// Each complete runtime remains quiet when either pressure condition is low.
func TestWorkerCapSignalCompleteRuntimeThresholdControls(t *testing.T) {
	now := time.Date(2026, 9, 26, 22, 0, 0, 0, time.UTC)
	for _, control := range []struct {
		cpu        float64
		goroutines float64
		age        time.Duration
		pressure   bool
	}{
		{cpu: 1, goroutines: 1000},
		{cpu: 3.6, goroutines: 9999},
		{cpu: 3.59, goroutines: 10_000},
		{cpu: 3.6, goroutines: 10_000, pressure: true},
		{cpu: 1, goroutines: 1000, age: 90 * time.Second},
		{cpu: 1, goroutines: 1000, age: -30 * time.Second},
	} {
		settings := workerCapSyntheticSettings(t, now,
			workerCapFixture{host: "edge-a", block: "g1", instance: "control", cpu: control.cpu, maxprocs: 4, goroutines: control.goroutines,
				cpuAge: control.age, maxprocsAge: control.age, goroutinesAge: control.age},
			workerCapFixture{host: "edge-b", block: "g2", instance: "quiet", cpu: 1, maxprocs: 4, goroutines: 1000})
		env, err := newProbeEnv(settings.withDefaults())
		if err != nil {
			t.Fatal(err)
		}
		findings, err := (workerCapProbe{}).check(context.Background(), env)
		if err != nil {
			t.Fatal(err)
		}
		visibility, capacity := 0, 0
		for _, finding := range findings {
			if finding.class == "worker-scheduler-unobservable" {
				visibility++
				if !finding.healthy {
					t.Fatalf("complete fresh control lost visibility: %+v", control)
				}
			}
			if finding.class == "worker-scheduler-capacity" && !finding.healthy {
				capacity++
				if !control.pressure || finding.target != "edge-a/g1" || finding.frame != "control" || finding.sustain != 2 {
					t.Fatalf("wrong pressure boundary: %+v", control)
				}
			}
		}
		wantCapacity := 0
		if control.pressure {
			wantCapacity = 1
		}
		if visibility != 1 || capacity != wantCapacity {
			t.Fatalf("complete control: %+v visibility=%d capacity=%d", control, visibility, capacity)
		}
	}
}

// Zero parallelism and duplicate tuples remain unknown beside a valid runtime.
func TestWorkerCapSignalAmbiguityPreservesCompleteSibling(t *testing.T) {
	now := time.Date(2026, 9, 26, 22, 0, 0, 0, time.UTC)
	for _, duplicate := range []bool{false, true} {
		workers := []workerCapFixture{
			{host: "edge-a", block: "g1", instance: "complete", cpu: 3.6, maxprocs: 4, goroutines: 10_000},
			{host: "edge-b", block: "g2", instance: "ambiguous", cpu: 4, goroutines: 10_000},
		}
		if duplicate {
			workers[1].maxprocs = 4
			workers = append(workers, workers[1])
		}
		alerts, err := NewWorkerCapSignal().Run(context.Background(), workerCapSyntheticSettings(t, now, workers...))
		if err != nil {
			t.Fatal(err)
		}
		capacity := requireAlertClass(t, alerts, "worker-scheduler-capacity")
		visibility := requireAlertClass(t, alerts, "worker-scheduler-unobservable")
		if requireAlertClassCount(alerts, "worker-scheduler-capacity") != 1 || capacity.Frame != "complete" ||
			!strings.Contains(visibility.Markdown(), "paired_runtimes=1 incomplete_runtimes=1") {
			t.Fatalf("duplicate=%t: ambiguity changed independent sibling", duplicate)
		}
	}
}

// Rejection reasons do not need to agree to preserve returned coverage.
func TestWorkerCapSignalRetainsMixedRejectedRuntimeCoverage(t *testing.T) {
	now := time.Date(2026, 9, 26, 22, 0, 0, 0, time.UTC)
	settings := workerCapSyntheticSettings(t, now,
		workerCapFixture{host: "edge-a", block: "g1", instance: "complete", cpu: 1, maxprocs: 4, goroutines: 1000},
		workerCapFixture{host: "edge-b", block: "g2", instance: "rejected", cpu: 1, cpuAge: 91 * time.Second,
			maxprocs: 1, maxprocsAge: -31 * time.Second, goroutines: math.NaN()})
	alerts, err := NewWorkerCapSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	if requireAlertClassCount(alerts, "worker-scheduler-capacity") != 0 ||
		!strings.Contains(requireAlertClass(t, alerts, "worker-scheduler-unobservable").Markdown(), "paired_runtimes=1 incomplete_runtimes=1") {
		t.Fatal("mixed rejected tuple disappeared beside complete runtime")
	}
}

// Unparseable scalars remain a fixed whole-probe error without source text.
func TestWorkerCapSignalMalformedScalarRemainsSourceError(t *testing.T) {
	now := time.Date(2026, 9, 26, 22, 0, 0, 0, time.UTC)
	payload := workerCapFixtureJSON(t, now,
		workerCapFixture{host: "edge-a", block: "g1", instance: "complete", cpu: 1, maxprocs: 4, goroutines: 1000})
	payload = strings.Replace(payload, `"1.000000"`, `"private-invalid-scalar"`, 1)
	source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) { return payload, nil }}
	alerts, err := NewWorkerCapSignal().Run(context.Background(), workerMemorySyntheticSettings(source, now))
	if err == nil || err.Error() != "worker cap: invalid metric value" || len(alerts) != 0 {
		t.Fatal("malformed scalar changed source-error or privacy contract")
	}
}
