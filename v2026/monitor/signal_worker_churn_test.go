package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestWorkerChurnOptionalLifecycleErrorsStayPrivate(t *testing.T) {
	testWorkerOptionalLifecycleErrorPrivacy(t, NewWorkerChurnSignal, "worker-cpu-allocation-churn")
}

func workerScorePhaseFixtureJSON(t *testing.T, now time.Time, host, block, instance string, complete bool) string {
	t.Helper()
	result := []any{}
	for phaseIndex, phase := range workerScorePhaseNames {
		metrics := map[string]float64{
			"active":   0,
			"duration": float64(phaseIndex+1) / 10,
			"exits":    float64(phaseIndex + 1),
			"items":    float64((phaseIndex + 1) * 10),
			"bytes":    float64((phaseIndex + 1) << 20),
		}
		if phase == "gob_encode" {
			metrics["active"] = 12
		}
		for metricName, value := range metrics {
			if !complete && phase == "cache_write" && metricName == "bytes" {
				continue
			}
			result = append(result, map[string]any{
				"metric": map[string]string{
					"monitor_score_phase_metric": metricName,
					"phase":                      phase,
					"env":                        "synthetic",
					"job":                        "taskworker",
					"host":                       host,
					"block":                      block,
					"instance":                   instance,
				},
				"value": []any{float64(now.Unix()), fmt.Sprintf("%.6f", value)},
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
			if strings.Contains(command, "monitor_score_phase_metric") {
				return workerScorePhaseFixtureJSON(t, now, "edge-3", "g2", "hot", true), nil
			}
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
		redisFn: func(host HostSettings, port int, args ...string) (string, error) {
			if host.Name != "redis-1" || port != 6379 || strings.Join(args, " ") != "-c --raw GET "+redisScoreAliasReadyKey {
				t.Fatalf("unexpected score-alias lookup: host=%s port=%d args=%v", host.Name, port, args)
			}
			return "", nil
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
		"active_tasks=UpdateClientScores:2875s,CloseExpiredContracts:7s",
		"score_alias_schema_ready=false",
		"does not identify this worker's artifact",
		"score_phase_observability=ready",
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
	requireAlertOmits(t, alert,
		"01a055c8-759e-406e-4061-603f0dc86869",
		"01a055f4-ccee-406e-4061-603f0dc86869",
	)
}

func runWorkerChurnAliasSynthetic(
	t *testing.T,
	taskName string,
	markerValue string,
	markerErr error,
	phaseCompleteOption ...bool,
) (Alert, int) {
	t.Helper()
	phaseComplete := true
	if 0 < len(phaseCompleteOption) {
		phaseComplete = phaseCompleteOption[0]
	}
	now := time.Date(2026, 9, 12, 14, 30, 0, 0, time.UTC)
	workers := []workerMetricFixture{
		{host: "worker-a", block: "g1", instance: "runtime-a", cpuRate: 0.02, allocRate: 1 << 20},
		{host: "worker-b", block: "g1", instance: "runtime-b", cpuRate: 0.04, allocRate: 2 << 20},
		{host: "worker-c", block: "g1", instance: "runtime-c", cpuRate: 0.06, allocRate: 4 << 20},
		{host: "worker-hot", block: "g2", instance: "runtime-hot", cpuRate: 4.001, allocRate: 320 << 20},
	}
	redisReads := 0
	phaseReads := 0
	source := &syntheticSource{
		hostFn: func(_ HostSettings, command string) (string, error) {
			if strings.Contains(command, "monitor_score_phase_metric") {
				phaseReads++
				return workerScorePhaseFixtureJSON(t, now, "worker-hot", "g2", "runtime-hot", phaseComplete), nil
			}
			return workerRatesFixtureJSON(t, now, workers...), nil
		},
		localFn: func(string, ...string) (string, error) {
			return "[worker-hot][taskworker][g2][cid:runtime-hot][I][2026-09-12T14:29:55Z][task.go:1938][02b166d9-86af-517f-5172-714a1ed9797a]eval active(90.00s) synthetic/taskworker/work." + taskName + "({})", nil
		},
		redisFn: func(host HostSettings, port int, args ...string) (string, error) {
			redisReads++
			if host.Name != "redis-1" || port != 6379 || strings.Join(args, " ") != "-c --raw GET "+redisScoreAliasReadyKey {
				t.Fatalf("unexpected score-alias lookup: host=%s port=%d args=%v", host.Name, port, args)
			}
			return markerValue, markerErr
		},
	}

	alerts, err := NewWorkerChurnSignal().Run(context.Background(), workerMemorySyntheticSettings(source, now))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "worker-cpu-allocation-churn")
	wantPhaseReads := 0
	if taskName == "UpdateClientScores" {
		wantPhaseReads = 1
	}
	if phaseReads != wantPhaseReads {
		t.Fatalf("phase reads = %d, want %d regardless of marker state", phaseReads, wantPhaseReads)
	}
	requireAlertOmits(t, alert, "02b166d9-86af-517f-5172-714a1ed9797a")
	return alert, redisReads
}

func TestWorkerChurnSignalTreatsMixedPhaseRolloutAsUnobservable(t *testing.T) {
	alert, _ := runWorkerChurnAliasSynthetic(
		t,
		"UpdateClientScores",
		redisScoreAliasReadyValue,
		nil,
		false,
	)
	markdown := alert.Markdown()
	for _, want := range []string{
		"score_phase_observability=unavailable",
		"complete source-fresh phase set is unavailable for this exact runtime",
		"explicitly unobservable, not healthy",
		"no partial set or other worker was substituted",
		"The process-rate finding remains valid",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("mixed phase rollout diagnosis missing %q:\n%s", want, markdown)
		}
	}
	for _, omit := range []string{"score_phase_observability=ready", "active gob_encode spans"} {
		if strings.Contains(markdown, omit) {
			t.Fatalf("mixed phase rollout was classified with complete telemetry %q:\n%s", omit, markdown)
		}
	}
}

func TestWorkerChurnSignalSyntheticScoreAliasStates(t *testing.T) {
	tests := []struct {
		name        string
		markerValue string
		markerErr   error
		want        []string
		omit        []string
	}{
		{
			name:        "absent",
			markerValue: "",
			want: []string{
				"score_alias_schema_ready=false",
				"does not identify this worker's artifact",
				"If the artifact is caller-oriented, deploy the target-oriented UpdateClientScores fanout",
				"score_phase_observability=ready",
			},
			omit: []string{"already active; do not redeploy"},
		},
		{
			name:        "ready",
			markerValue: redisScoreAliasReadyValue,
			want: []string{
				"score_alias_schema_ready=true",
				"some writer previously completed a compatibility pass",
				"not provenance for this exact worker artifact",
				"score_phase_observability=ready",
				"score_phase_active=source_load:0,target_export:0,target_map:0,gob_encode:12,cache_write:0",
				"score_phase_mib_per_s_1m=source_load:1.00,target_export:2.00,target_map:3.00,gob_encode:4.00,cache_write:5.00",
				"the scrape found 12 active gob_encode spans",
				"work bytes are not process heap allocations",
				"completed-span seconds are not CPU time",
				"First verify the exact outlier Taskworker artifact",
				"profile recurring source-load, target-map, gob-encode, or cache-write work",
			},
			omit: []string{"Deploy the target-oriented UpdateClientScores fanout"},
		},
		{
			name:      "unknown",
			markerErr: fmt.Errorf("synthetic marker unavailable"),
			want: []string{
				"score_alias_schema_ready=unknown",
				"alias-schema marker lookup failed",
				"Neither marker state nor exact exporter capability is established",
				"global score-alias compatibility marker could not be read",
				"First verify the exact outlier Taskworker artifact",
				"score_phase_observability=ready",
			},
			omit: []string{"score_alias_schema_ready=false", "score_alias_schema_ready=true"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			alert, redisReads := runWorkerChurnAliasSynthetic(
				t,
				"UpdateClientScores",
				test.markerValue,
				test.markerErr,
			)
			if redisReads != 1 {
				t.Fatalf("score-alias reads = %d, want 1", redisReads)
			}
			markdown := alert.Markdown()
			if alert.Target != "worker-hot/g2" || alert.Frame != "runtime-hot" || alert.Sustain != 2 || alert.Severity != SeverityWarn {
				t.Fatal("marker state changed the real churn finding")
			}
			for _, want := range []string{"score_alias_marker_scope=global score_exporter_capability=unverified", "not heap ownership"} {
				if !strings.Contains(markdown, want) {
					t.Fatalf("missing phase/provenance limit %q", want)
				}
			}
			requireAlertOmits(t, alert, "This is therefore residual allocation in the deployed sparse exporter", "already active; do not redeploy them")
			for _, want := range test.want {
				if !strings.Contains(markdown, want) {
					t.Fatalf("worker-churn %s diagnosis missing %q:\n%s", test.name, want, markdown)
				}
			}
			for _, omit := range test.omit {
				if strings.Contains(markdown, omit) {
					t.Fatalf("worker-churn %s diagnosis retained %q:\n%s", test.name, omit, markdown)
				}
			}
		})
	}
}

func TestWorkerChurnSignalSyntheticNonScoreContextSkipsAliasLookup(t *testing.T) {
	alert, redisReads := runWorkerChurnAliasSynthetic(
		t,
		"ProviderEgressProbe",
		redisScoreAliasReadyValue,
		nil,
	)
	if redisReads != 0 {
		t.Fatalf("non-score context read score-alias marker %d time(s)", redisReads)
	}
	markdown := alert.Markdown()
	for _, want := range []string{
		"active_tasks=ProviderEgressProbe:90s",
		"Co-resident task heartbeats narrow the candidate work",
		"Profile or inspect the active task families on this exact executor",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("generic worker-churn diagnosis missing %q:\n%s", want, markdown)
		}
	}
	for _, omit := range []string{"score_alias_schema_ready=", "client_score_alias_v1_ready", "target-oriented fanout"} {
		if strings.Contains(markdown, omit) {
			t.Fatalf("generic worker-churn diagnosis retained score-specific %q:\n%s", omit, markdown)
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
	alert := requireAlertClass(t, alerts, "worker-cpu-allocation-churn")
	markdown := alert.Markdown()
	for _, want := range []string{
		"active_tasks=UpdateClientScores:4190s",
		"does not identify this worker's artifact",
		"target-oriented UpdateClientScores fanout",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("delayed log collection lost %q:\n%s", want, markdown)
		}
	}
	requireAlertOmits(t, alert, "01a055c8-759e-406e-4061-603f0dc86869")
}
