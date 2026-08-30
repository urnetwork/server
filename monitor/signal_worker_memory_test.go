package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

type workerMetricFixture struct {
	host        string
	block       string
	instance    string
	heap        float64
	objects     float64
	rss         float64
	allocTotal  float64
	gcCycles    float64
	startTime   float64
	age         time.Duration
	cpuRate     float64
	allocRate   float64
	gcRate      float64
	gcPauseRate float64
}

func workerMetricsFixtureJSON(t *testing.T, now time.Time, workers ...workerMetricFixture) string {
	t.Helper()
	result := []any{}
	for _, worker := range workers {
		metrics := map[string]float64{
			"go_memstats_heap_alloc_bytes":  worker.heap,
			"go_memstats_heap_objects":      worker.objects,
			"go_memstats_alloc_bytes_total": worker.allocTotal,
			"go_gc_duration_seconds_count":  worker.gcCycles,
			"process_resident_memory_bytes": worker.rss,
			"process_start_time_seconds":    worker.startTime,
		}
		for name, value := range metrics {
			result = append(result, map[string]any{
				"metric": map[string]string{
					"__name__": name,
					"env":      "synthetic",
					"job":      "taskworker",
					"host":     worker.host,
					"block":    worker.block,
					"instance": worker.instance,
				},
				"value": []any{float64(now.Add(-worker.age).Unix()), fmt.Sprintf("%.0f", value)},
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

func workerRatesFixtureJSON(t *testing.T, now time.Time, workers ...workerMetricFixture) string {
	t.Helper()
	result := []any{}
	for _, worker := range workers {
		rates := map[string]float64{
			"cpu":      worker.cpuRate,
			"alloc":    worker.allocRate,
			"gc":       worker.gcRate,
			"gc_pause": worker.gcPauseRate,
		}
		for name, value := range rates {
			result = append(result, map[string]any{
				"metric": map[string]string{
					"monitor_rate": name,
					"env":          "synthetic",
					"job":          "taskworker",
					"host":         worker.host,
					"block":        worker.block,
					"instance":     worker.instance,
				},
				"value": []any{float64(now.Add(-worker.age).Unix()), fmt.Sprintf("%.9f", value)},
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

func workerMemorySyntheticSettings(source SignalSource, now time.Time) SignalSettings {
	settings := syntheticSettings(source)
	settings.Now = func() time.Time { return now }
	settings.Hosts = append(settings.Hosts, HostSettings{Name: "metrics-1", Roles: []string{"services"}})
	return settings
}

func TestWorkerMemorySignalSyntheticLiveHeapSkew(t *testing.T) {
	now := time.Date(2026, 8, 30, 15, 17, 50, 0, time.UTC)
	const gib = float64(uint64(1) << 30)
	workers := []workerMetricFixture{
		{host: "edge-0", block: "g1", instance: "a", heap: 0.125 * gib, cpuRate: 0.02, allocRate: 1 << 20, gcRate: 0.01, gcPauseRate: 0.000001},
		{host: "edge-1", block: "g1", instance: "b", heap: 0.1875 * gib, cpuRate: 0.03, allocRate: 2 << 20, gcRate: 0.02, gcPauseRate: 0.000002},
		{host: "edge-3", block: "g1", instance: "c", heap: 0.25 * gib, cpuRate: 0.04, allocRate: 3 << 20, gcRate: 0.03, gcPauseRate: 0.000003},
		workerMetricFixture{
			host: "edge-3", block: "g2", instance: "hot", heap: 32 * gib,
			objects: 27_250_919, rss: 34 * gib, allocTotal: 8 * 1024 * gib,
			gcCycles: 3400, startTime: float64(now.Add(-15 * time.Hour).Unix()),
			cpuRate: 4, allocRate: 900 << 20, gcRate: 0.125, gcPauseRate: 0.00004,
		},
	}
	payload := workerMetricsFixtureJSON(t, now, workers...)
	ratePayload := workerRatesFixtureJSON(t, now, workers...)
	source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		if host.Name != "metrics-1" || !strings.Contains(command, "/prometheus/api/v1/query?query=") ||
			!strings.Contains(command, "%22synthetic%22") {
			t.Fatalf("unexpected Mimir command on %s: %s", host.Name, command)
		}
		if strings.Contains(command, "monitor_rate") {
			return ratePayload, nil
		}
		return payload, nil
	}, localFn: func(name string, args ...string) (string, error) {
		joined := strings.Join(args, " ")
		if name != "warpctl" || !strings.Contains(joined, "--since=2m") ||
			!strings.Contains(joined, "--limit=5000") || !strings.Contains(joined, "--query=eval") {
			t.Fatalf("unexpected active-task command: %s %s", name, joined)
		}
		return "[edge-3][taskworker][g2][cid:hot][I][2026-08-30T15:17:40Z][task.go:1938][01a0530b-0e6a-9c14-6694-11a165f3c27b]eval active(620.50s) github.com/urnetwork/server/taskworker/work.UpdateClientScores({})\n" +
			"[edge-3][taskworker][g2][cid:hot][I][2026-08-30T15:17:41Z][task.go:1938][01a0530c-65aa-153e-19d8-82ad3698cf40]eval active(130.25s) github.com/urnetwork/server/taskworker/work.CloseExpiredContracts({})\n" +
			"[edge-3][taskworker][g2][cid:hot][I][2026-08-30T15:16:40Z][task.go:1938][01a0530f-65aa-153e-19d8-82ad3698cf40]eval active(500.00s) github.com/urnetwork/server/taskworker/work.UpdateClientLocations({})\n" +
			"[edge-3][taskworker][g2][cid:hot][I][2026-08-30T15:19:00Z][task.go:1938][01a05310-65aa-153e-19d8-82ad3698cf40]eval active(700.00s) github.com/urnetwork/server/taskworker/work.ExportStats({})\n" +
			"[edge-3][taskworker][g2][cid:hot][I][2026-08-30T15:17:39Z][task.go:1938][01a0530e-65aa-153e-19d8-82ad3698cf40]eval active(90.00s) github.com/urnetwork/server/taskworker/work.ReconcileNetEscrow({})\n" +
			"[edge-3][taskworker][g2][cid:hot][I][2026-08-30T15:17:42Z][task.go:1927][01a0530e-65aa-153e-19d8-82ad3698cf40]eval done(93.00s) github.com/urnetwork/server/taskworker/work.ReconcileNetEscrow({}) = {}\n" +
			"[edge-3][taskworker][g1][cid:cold][I][2026-08-30T15:17:42Z][task.go:1938][01a0530d-65aa-153e-19d8-82ad3698cf40]eval active(999.00s) github.com/urnetwork/server/taskworker/work.ReconcileNetEscrow({})", nil
	}}

	alerts, err := NewWorkerMemorySignal().Run(context.Background(), workerMemorySyntheticSettings(source, now))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "worker-memory-skew")
	if alert.Target != "edge-3/g2" || alert.Frame != "hot" || alert.Sustain != 2 {
		t.Fatalf("wrong worker identity or sustain: %+v", alert)
	}
	markdown := alert.Markdown()
	for _, want := range []string{
		"32.00GiB of allocated Go heap",
		"fleet_median_gib=0.22",
		"fleet_ratio=146.3",
		"heap_objects=27250919",
		"rss_bytes=36507222016",
		"alloc_total_bytes=8796093022208",
		"process_age_s=54000",
		"cpu_cores_5m=4.000",
		"fleet_median_cpu_cores_5m=0.035",
		"cpu_ratio_5m=114.3",
		"alloc_bytes_per_s_5m=943718400",
		"alloc_mib_per_s_5m=900.00",
		"fleet_median_alloc_bytes_per_s_5m=2621440",
		"alloc_ratio_5m=360.0",
		"gc_cycles_per_s_5m=0.125",
		"gc_pause_seconds_per_s_5m=0.000040",
		"active_task_count=2",
		"active_tasks=UpdateClientScores:620s@01a0530b-0e6a-9c14-6694-11a165f3c27b,CloseExpiredContracts:130s@01a0530c-65aa-153e-19d8-82ad3698cf40",
		"process-local allocator/GC contention",
		"unreachable objects not yet reclaimed",
		"bounded or streaming working-set fixes",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("worker-memory diagnosis missing %q:\n%s", want, markdown)
		}
	}
}

func TestWorkerMemorySignalSyntheticIgnoresStaleAndInBandWorkers(t *testing.T) {
	now := time.Date(2026, 8, 30, 15, 17, 50, 0, time.UTC)
	const gib = float64(uint64(1) << 30)
	workers := []workerMetricFixture{
		{host: "edge-0", block: "g1", instance: "a", heap: 0.125 * gib},
		{host: "edge-1", block: "g1", instance: "b", heap: 0.25 * gib},
		{host: "edge-3", block: "g1", instance: "c", heap: 0.5 * gib},
		{host: "edge-4", block: "old", instance: "stale", heap: 64 * gib, age: 2 * time.Minute},
	}
	payload := workerMetricsFixtureJSON(t, now, workers...)
	ratePayload := workerRatesFixtureJSON(t, now, workers...)
	source := &syntheticSource{hostFn: func(_ HostSettings, command string) (string, error) {
		if strings.Contains(command, "monitor_rate") {
			return ratePayload, nil
		}
		return payload, nil
	}}

	alerts, err := NewWorkerMemorySignal().Run(context.Background(), workerMemorySyntheticSettings(source, now))
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 0 {
		t.Fatalf("in-band workers or stale old generation alerted: %+v", alerts)
	}
}

func TestWorkerMemorySignalSyntheticRateFailureDoesNotHideHeapSkew(t *testing.T) {
	now := time.Date(2026, 8, 30, 15, 17, 50, 0, time.UTC)
	const gib = float64(uint64(1) << 30)
	payload := workerMetricsFixtureJSON(t, now,
		workerMetricFixture{host: "edge-0", block: "g1", instance: "a", heap: 0.125 * gib},
		workerMetricFixture{host: "edge-1", block: "g1", instance: "b", heap: 0.25 * gib},
		workerMetricFixture{host: "edge-3", block: "g2", instance: "hot", heap: 24 * gib},
	)
	source := &syntheticSource{
		hostFn: func(_ HostSettings, command string) (string, error) {
			if strings.Contains(command, "monitor_rate") {
				return "", errors.New("synthetic rate query unavailable")
			}
			return payload, nil
		},
		localFn: func(string, ...string) (string, error) { return "", nil },
	}

	alerts, err := NewWorkerMemorySignal().Run(context.Background(), workerMemorySyntheticSettings(source, now))
	if err != nil {
		t.Fatal(err)
	}
	markdown := requireAlertClass(t, alerts, "worker-memory-skew").Markdown()
	if !strings.Contains(markdown, "best-effort five-minute rate lookup failed: synthetic rate query unavailable") {
		t.Fatalf("heap alert did not preserve rate-query degradation evidence:\n%s", markdown)
	}
	if strings.Contains(markdown, "cpu_cores_5m=") {
		t.Fatalf("heap alert rendered unavailable rate values:\n%s", markdown)
	}
}
