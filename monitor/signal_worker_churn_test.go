package monitor

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestWorkerChurnSignalSyntheticScoreFanout(t *testing.T) {
	now := time.Date(2026, 8, 31, 3, 55, 0, 0, time.UTC)
	workers := []workerMetricFixture{
		{host: "edge-0", block: "g1", instance: "a", cpuRate: 0.02, allocRate: 1 << 20},
		{host: "edge-1", block: "g1", instance: "b", cpuRate: 0.04, allocRate: 2 << 20},
		{host: "edge-4", block: "g1", instance: "c", cpuRate: 0.06, allocRate: 4 << 20},
		{host: "edge-3", block: "g2", instance: "hot", cpuRate: 4.003, allocRate: 650 << 20},
	}
	ratePayload := workerRatesFixtureJSON(t, now, workers...)
	source := &syntheticSource{
		hostFn: func(host HostSettings, command string) (string, error) {
			if host.Name != "metrics-1" || !strings.Contains(command, "monitor_rate") ||
				!strings.Contains(command, "%22synthetic%22") {
				t.Fatalf("unexpected Mimir command on %s: %s", host.Name, command)
			}
			return ratePayload, nil
		},
		localFn: func(name string, args ...string) (string, error) {
			joined := strings.Join(args, " ")
			if name != "warpctl" || !strings.Contains(joined, "--since=2m") ||
				!strings.Contains(joined, "--limit=5000") || !strings.Contains(joined, "--query=eval") {
				t.Fatalf("unexpected active-task command: %s %s", name, joined)
			}
			return "[edge-3][taskworker][g2][cid:hot][I][2026-08-31T03:54:50Z][task.go:1938][01a055c8-759e-406e-4061-603f0dc86869]eval active(2875.00s) github.com/urnetwork/server/taskworker/work.UpdateClientScores({})\n" +
				"[edge-3][taskworker][g2][cid:hot][I][2026-08-31T03:54:51Z][task.go:1938][01a055f4-ccee-406e-4061-603f0dc86869]eval active(7.00s) github.com/urnetwork/server/taskworker/work.CloseExpiredContracts({})", nil
		},
	}

	alerts, err := NewWorkerChurnSignal().Run(context.Background(), workerMemorySyntheticSettings(source, now))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "worker-cpu-allocation-churn")
	if alert.Target != "edge-3/g2" || alert.Frame != "hot" || alert.Sustain != 2 {
		t.Fatalf("wrong worker identity or sustain: %+v", alert)
	}
	markdown := alert.Markdown()
	for _, want := range []string{
		"consumes 4.003 CPU cores while allocating 650.00MiB/s",
		"fleet_samples=4",
		"fleet_median_cpu_cores_1m=0.050",
		"cpu_ratio_1m=80.1",
		"alloc_bytes_per_s_1m=681574400",
		"fleet_median_alloc_bytes_per_s_1m=3145728",
		"alloc_ratio_1m=216.7",
		"active_tasks=UpdateClientScores:2875s@01a055c8-759e-406e-4061-603f0dc86869,CloseExpiredContracts:7s@01a055f4-ccee-406e-4061-603f0dc86869",
		"target's exported score payload is caller-invariant",
		"CloseExpiredContracts is active on the same host/block",
		"delay its Go work between otherwise short PostgreSQL statements",
		"target-oriented UpdateClientScores fanout",
		"co-resident close checkpoint also returns below 120 seconds",
		"Exact co-residency proves a shared process budget",
		"SIGNALS.md 2.12a",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("worker-churn diagnosis missing %q:\n%s", want, markdown)
		}
	}
}

func TestWorkerChurnSignalSyntheticRequiresBothRatesAndFreshSkew(t *testing.T) {
	now := time.Date(2026, 8, 31, 3, 55, 0, 0, time.UTC)
	workers := []workerMetricFixture{
		{host: "edge-0", block: "g1", instance: "base-a", cpuRate: 0.02, allocRate: 1 << 20},
		{host: "edge-1", block: "g1", instance: "base-b", cpuRate: 0.04, allocRate: 2 << 20},
		{host: "edge-2", block: "g1", instance: "cpu-only", cpuRate: 4.0, allocRate: 8 << 20},
		{host: "edge-3", block: "g1", instance: "alloc-only", cpuRate: 0.1, allocRate: 650 << 20},
		{host: "edge-4", block: "old", instance: "stale", cpuRate: 4.0, allocRate: 650 << 20, age: 2 * time.Minute},
	}
	source := &syntheticSource{hostFn: func(_ HostSettings, command string) (string, error) {
		if !strings.Contains(command, "monitor_rate") {
			return "", fmt.Errorf("unexpected query: %s", command)
		}
		return workerRatesFixtureJSON(t, now, workers...), nil
	}}

	alerts, err := NewWorkerChurnSignal().Run(context.Background(), workerMemorySyntheticSettings(source, now))
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 0 {
		t.Fatalf("single-rate or stale workers alerted: %+v", alerts)
	}
}

func TestWorkerChurnSignalSyntheticRefreshesClockAfterDelayedLogFallback(t *testing.T) {
	metricNow := time.Date(2026, 8, 31, 4, 15, 45, 0, time.UTC)
	clock := metricNow
	workers := []workerMetricFixture{
		{host: "edge-0", block: "g1", instance: "a", cpuRate: 0.02, allocRate: 1 << 20},
		{host: "edge-1", block: "g1", instance: "b", cpuRate: 0.04, allocRate: 2 << 20},
		{host: "edge-4", block: "g1", instance: "c", cpuRate: 0.06, allocRate: 4 << 20},
		{host: "edge-3", block: "g2", instance: "hot", cpuRate: 4.0, allocRate: 650 << 20},
	}
	ratePayload := workerRatesFixtureJSON(t, metricNow, workers...)
	source := &syntheticSource{
		hostFn: func(_ HostSettings, _ string) (string, error) { return ratePayload, nil },
		localFn: func(string, ...string) (string, error) {
			// Simulate a slow fleet gateway/fallback. Relative to metricNow this
			// heartbeat is 60s in the future and the parser must reject it; at
			// the post-collection clock it is a fresh five-second-old line.
			clock = metricNow.Add(65 * time.Second)
			return "[edge-3][taskworker][g2][cid:hot][I][2026-08-31T04:16:45Z][task.go:1938][01a055c8-759e-406e-4061-603f0dc86869]eval active(4190.00s) github.com/urnetwork/server/taskworker/work.UpdateClientScores({})", nil
		},
	}
	settings := workerMemorySyntheticSettings(source, metricNow)
	settings.Now = func() time.Time { return clock }

	alerts, err := NewWorkerChurnSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	markdown := requireAlertClass(t, alerts, "worker-cpu-allocation-churn").Markdown()
	for _, want := range []string{
		"active_tasks=UpdateClientScores:4190s@01a055c8-759e-406e-4061-603f0dc86869",
		"target's exported score payload is caller-invariant",
		"target-oriented UpdateClientScores fanout",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("delayed log collection lost %q:\n%s", want, markdown)
		}
	}
}
