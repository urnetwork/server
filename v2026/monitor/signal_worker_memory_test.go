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
	source := &syntheticSource{
		hostFn: func(host HostSettings, command string) (string, error) {
			if host.Name != "metrics-1" || !strings.Contains(command, "/prometheus/api/v1/query?query=") ||
				!strings.Contains(command, "%22synthetic%22") {
				t.Fatalf("unexpected Mimir command on %s: %s", host.Name, command)
			}
			if strings.Contains(command, "monitor_rate") {
				return ratePayload, nil
			}
			return payload, nil
		},
		redisFn: func(host HostSettings, port int, args ...string) (string, error) {
			if host.Name != "redis-1" || port != 6379 || strings.Join(args, " ") != "-c --raw GET "+redisScoreAliasReadyKey {
				t.Fatalf("unexpected score-alias lookup: host=%s port=%d args=%v", host.Name, port, args)
			}
			return redisScoreAliasReadyValue, nil
		},
		localFn: func(name string, args ...string) (string, error) {
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
		},
	}

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
		"active_tasks=UpdateClientScores:620s,CloseExpiredContracts:130s",
		"active_log_source=warpctl",
		"process-local allocator/GC contention",
		"score_alias_schema_ready=true",
		"score_alias_marker_scope=global score_exporter_capability=unverified",
		"some writer previously completed a compatibility pass",
		"not provenance for this exact worker artifact",
		"correlated allocation candidate, not established ownership of the whole heap",
		"CloseExpiredContracts is active on the same host/block",
		"First verify the exact outlier Taskworker artifact",
		"co-resident close checkpoint also returns below 120 seconds",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("worker-memory diagnosis missing %q:\n%s", want, markdown)
		}
	}
	requireAlertOmits(t, alert,
		"01a0530b-0e6a-9c14-6694-11a165f3c27b",
		"01a0530c-65aa-153e-19d8-82ad3698cf40",
	)
	if strings.Contains(markdown, "Deploy the target-oriented UpdateClientScores fanout") {
		t.Fatalf("deployed alias marker retained the pre-deploy action:\n%s", markdown)
	}
}

func TestWorkerMemorySignalSyntheticFallsBackToHostJournalForActiveTasks(t *testing.T) {
	now := time.Date(2026, 8, 31, 5, 37, 50, 0, time.UTC)
	const gib = float64(uint64(1) << 30)
	workers := []workerMetricFixture{
		{host: "edge-0", block: "g1", instance: "a", heap: 0.125 * gib, cpuRate: 0.02, allocRate: 1 << 20, gcRate: 0.01, gcPauseRate: 0.000001},
		{host: "edge-1", block: "g1", instance: "b", heap: 0.1875 * gib, cpuRate: 0.03, allocRate: 2 << 20, gcRate: 0.02, gcPauseRate: 0.000002},
		{host: "edge-3", block: "g1", instance: "hot", heap: 12 * gib, cpuRate: 4, allocRate: 640 << 20, gcRate: 0.12, gcPauseRate: 0.00003},
	}
	payload := workerMetricsFixtureJSON(t, now, workers...)
	ratePayload := workerRatesFixtureJSON(t, now, workers...)
	journal := fmt.Sprintf(
		`{"SYSLOG_TIMESTAMP":%q,"MESSAGE":%q,"CONTAINER_TAG":"warp|synthetic|taskworker|g1","CONTAINER_ID":"hot","_HOSTNAME":"edge-3"}`,
		now.Add(-5*time.Second).Format(time.RFC3339Nano),
		"I0831 05:37:45.000000 1 task.go:1938] [01a05616-2af9-07af-9ce6-8ba1bc304862]eval active(3940.00s) github.com/urnetwork/server/taskworker/work.UpdateClientScores({})",
	)
	source := &syntheticSource{
		hostFn: func(host HostSettings, command string) (string, error) {
			if host.Name != "metrics-1" {
				t.Fatalf("unexpected Mimir host %s", host.Name)
			}
			if strings.Contains(command, "monitor_rate") {
				return ratePayload, nil
			}
			return payload, nil
		},
		hostTimeoutFn: func(host HostSettings, command string, timeout time.Duration) (string, error) {
			if host.Name != "metrics-1" || timeout != taskworkerJournalTimeout || !strings.Contains(command, "--grep='eval'") {
				t.Fatalf("unexpected journal fallback: host=%s timeout=%s command=%s", host.Name, timeout, command)
			}
			return journal, nil
		},
		localFn: func(name string, args ...string) (string, error) {
			return "", fmt.Errorf("synthetic %s gateway unavailable", name)
		},
	}

	alerts, err := NewWorkerMemorySignal().Run(context.Background(), workerMemorySyntheticSettings(source, now))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "worker-memory-skew")
	markdown := alert.Markdown()
	for _, want := range []string{
		"active_tasks=UpdateClientScores:3940s",
		"active_log_source=host-journal-fallback",
		"UpdateClientScores has a fresh heartbeat on the outlier's host/block",
		"target-oriented UpdateClientScores fanout and alias-aware cache",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("worker-memory journal fallback missing %q:\n%s", want, markdown)
		}
	}
	requireAlertOmits(t, alert, "01a05616-2af9-07af-9ce6-8ba1bc304862")
	if strings.Contains(markdown, "task-lifecycle lookup was degraded") {
		t.Fatalf("complete host-journal fallback was reported as degraded:\n%s", markdown)
	}
}

func TestWorkerMemoryGlobalAliasMarkerDoesNotAttestOutlierArtifact(t *testing.T) {
	now := time.Date(2026, 9, 16, 11, 58, 30, 0, time.UTC)
	const gib = float64(uint64(1) << 30)
	const privateTaskID = "01a0530b-0e6a-9c14-6694-11a165f3c27b"
	workers := []workerMetricFixture{
		{host: "edge-0", block: "g1", instance: "peer", heap: 0.25 * gib},
		{host: "edge-1", block: "g1", instance: "unattested", heap: 9 * gib, cpuRate: 1.4, allocRate: 200 << 20},
		{host: "edge-3", block: "g1", instance: "control", heap: 0.3 * gib},
	}
	metrics := workerMetricsFixtureJSON(t, now, workers...)
	rates := workerRatesFixtureJSON(t, now, workers...)
	for _, tc := range []struct {
		name, marker, state, mechanism string
		err                            error
	}{
		{"prior writer completed", redisScoreAliasReadyValue, "true", "some writer previously completed a compatibility pass", nil},
		{"no marker", "", "false", "does not identify this worker's artifact", nil},
		{"marker unavailable", "", "unknown", "Neither marker state nor exact exporter capability is established", errors.New("synthetic marker unavailable")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			phaseReads := 0
			source := &syntheticSource{
				hostFn: func(_ HostSettings, command string) (string, error) {
					if strings.Contains(command, "monitor_score_phase_metric") {
						phaseReads++
						return workerScorePhaseFixtureJSON(t, now, "edge-1", "g1", "unattested", true), nil
					}
					if strings.Contains(command, "monitor_rate") {
						return rates, nil
					}
					return metrics, nil
				},
				redisFn: func(_ HostSettings, _ int, args ...string) (string, error) {
					if strings.Join(args, " ") != "-c --raw GET "+redisScoreAliasReadyKey {
						t.Fatal("unexpected marker operation")
					}
					return tc.marker, tc.err
				},
				localFn: func(string, ...string) (string, error) {
					return fmt.Sprintf("[edge-1][taskworker][g1][cid:unattested][I][%s][task.go:1938][%s]eval active(101.0s) github.com/urnetwork/server/taskworker/work.UpdateClientScores({})\n", now.Add(-5*time.Second).Format(time.RFC3339Nano), privateTaskID), nil
				},
			}
			alerts, err := NewWorkerMemorySignal().Run(context.Background(), workerMemorySyntheticSettings(source, now))
			if err != nil {
				t.Fatal(err)
			}
			alert := requireAlertClass(t, alerts, "worker-memory-skew")
			if len(alerts) != 1 || alert.Target != "edge-1/g1" || alert.Frame != "unattested" || alert.Sustain != 2 || alert.Severity != Severity(tierWarn) {
				t.Fatal("marker state changed the real skew's identity, severity or sustain")
			}
			if phaseReads != 1 {
				t.Fatalf("phase reads = %d, want 1 despite marker state and below-churn rates", phaseReads)
			}
			for _, want := range []string{
				"9.00GiB of allocated Go heap", "active_tasks=UpdateClientScores:101s",
				"score_alias_schema_ready=" + tc.state,
				"score_alias_marker_scope=global score_exporter_capability=unverified",
				tc.mechanism, "First verify the exact outlier Taskworker artifact",
				"If target-oriented capability is independently proven",
				"score_phase_observability=ready", "the scrape found 12 active gob_encode spans",
				"not heap ownership", "completed-span seconds are not CPU time",
			} {
				if !strings.Contains(alert.Markdown(), want) {
					t.Fatalf("marker provenance regression missing %q", want)
				}
			}
			requireAlertOmits(t, alert, privateTaskID,
				"This is therefore residual allocation in the deployed sparse exporter",
				"target-oriented fanout and alias-aware cache are already active; do not redeploy them",
				"UpdateClientScores is active on the exact heap outlier",
			)
			encoded, err := json.Marshal(alert)
			if err != nil || strings.Contains(string(encoded), privateTaskID) {
				t.Fatal("marker provenance regression leaked the private task identity in JSON")
			}
		})
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
	if !strings.Contains(markdown, "best-effort five-minute rate lookup failed (error_class=observation-unclassified)") {
		t.Fatalf("heap alert did not preserve rate-query degradation evidence:\n%s", markdown)
	}
	if strings.Contains(markdown, "cpu_cores_5m=") {
		t.Fatalf("heap alert rendered unavailable rate values:\n%s", markdown)
	}
}

// Every value in this fixture is synthetic, including the credential, address,
// task ID and response-body markers. A real optional error must never be needed
// to exercise the durable-output privacy boundary.
const workerOptionalPrivateText = "synthetic-private-marker credential=synthetic-only Bearer synthetic-token task=aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa address=192.0.2.91 peer=2001:db8::91 url=https://synthetic-user:synthetic-password@example.invalid/private?token=synthetic-query payload=synthetic-payload"

func requireWorkerOptionalErrorPrivacy(t *testing.T, alert Alert, wantClass string) {
	t.Helper()
	structured, err := json.Marshal(alert)
	if err != nil {
		t.Fatal("synthetic Alert encoding failed")
	}
	var jsonl strings.Builder
	if err := (Alerts{alert}).WriteJSONL(&jsonl); err != nil {
		t.Fatal("synthetic JSONL encoding failed")
	}
	for format, rendered := range map[string]string{
		"Alert": string(structured), "Markdown": (Alerts{alert}).Markdown(), "JSONL": jsonl.String(),
	} {
		if wantClass != "" && !strings.Contains(rendered, "error_class="+wantClass) {
			t.Errorf("%s lost the fixed optional error class", format)
		}
		for index, forbidden := range []string{
			"synthetic-private-marker", "synthetic-only", "synthetic-token",
			"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "192.0.2.91", "2001:db8::91",
			"synthetic-user", "synthetic-password", "example.invalid", "synthetic-query", "synthetic-payload",
		} {
			if strings.Contains(rendered, forbidden) {
				t.Errorf("%s leaked private fixture component %d", format, index)
			}
		}
	}
}

func TestWorkerMemoryOptionalRateErrorsStayPrivate(t *testing.T) {
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	const gib = float64(uint64(1) << 30)
	payload := workerMetricsFixtureJSON(t, now,
		workerMetricFixture{host: "edge-0", block: "g1", instance: "a", heap: 0.125 * gib},
		workerMetricFixture{host: "edge-1", block: "g1", instance: "b", heap: 0.25 * gib},
		workerMetricFixture{host: "edge-3", block: "g2", instance: "hot", heap: 24 * gib},
	)
	response, err := json.Marshal(map[string]any{"status": "error", "error": "permission denied: " + workerOptionalPrivateText})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, response, wantClass string
		err                       error
	}{
		{name: "transport", err: errors.New("permission denied: " + workerOptionalPrivateText), wantClass: observationErrorClassAccessDenied},
		{name: "response body", response: string(response), wantClass: observationErrorClassAccessDenied},
		{name: "timeout", err: fmt.Errorf("%s: %w", workerOptionalPrivateText, context.DeadlineExceeded), wantClass: observationErrorClassTimeout},
		{name: "canceled", err: fmt.Errorf("%s: %w", workerOptionalPrivateText, context.Canceled), wantClass: observationErrorClassCanceled},
		{name: "unclassified", err: errors.New(workerOptionalPrivateText), wantClass: observationErrorClassUnclassified},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := &syntheticSource{
				hostFn: func(_ HostSettings, command string) (string, error) {
					if strings.Contains(command, "monitor_rate") {
						return test.response, test.err
					}
					return payload, nil
				},
				localFn: func(string, ...string) (string, error) { return "", nil },
			}
			alerts, err := NewWithSignals(workerMemorySyntheticSettings(source, now), NewWorkerMemorySignal()).Run(context.Background())
			if err != nil || len(alerts) != 1 {
				t.Fatal("optional rate failure replaced or erased the primary heap finding")
			}
			alert := requireAlertClass(t, alerts, "worker-memory-skew")
			if alert.Target != "edge-3/g2" || alert.Frame != "hot" || alert.Severity != SeverityWarn || alert.Sustain != 2 ||
				!strings.Contains(alert.Observed, "heap_gib=24.00") || strings.Contains(alert.Observed, "cpu_cores_5m=") {
				t.Fatal("optional rate failure changed heap identity, guards, or unavailable-rate meaning")
			}
			if !strings.Contains(alert.Evidence, "best-effort five-minute rate lookup failed") {
				t.Fatal("optional rate failure lost its source qualifier")
			}
			requireWorkerOptionalErrorPrivacy(t, alert, test.wantClass)
		})
	}
}

// Both owners use readTaskLifecycleLog. Exercise its real fallback and partial
// normalization before testing the owner projection, not a stubbed finding.
func testWorkerOptionalLifecycleErrorPrivacy(t *testing.T, newSignal func() Signal, class string) {
	t.Helper()
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	const gib = float64(uint64(1) << 30)
	workers := []workerMetricFixture{
		{host: "edge-0", block: "g1", instance: "a", heap: 0.125 * gib, cpuRate: 0.02, allocRate: 1 << 20},
		{host: "edge-1", block: "g1", instance: "b", heap: 0.25 * gib, cpuRate: 0.04, allocRate: 2 << 20},
		{host: "edge-3", block: "g2", instance: "hot", heap: 24 * gib, cpuRate: 4, allocRate: 650 << 20},
	}
	heapPayload := workerMetricsFixtureJSON(t, now, workers...)
	ratePayload := workerRatesFixtureJSON(t, now, workers...)
	journal := fmt.Sprintf(
		`{"SYSLOG_TIMESTAMP":%q,"MESSAGE":%q,"CONTAINER_TAG":"warp|synthetic|taskworker|g2","CONTAINER_ID":"hot","_HOSTNAME":"edge-3"}`,
		now.Add(-5*time.Second).Format(time.RFC3339Nano),
		"[aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa]eval active(130.00s) github.com/urnetwork/server/taskworker/work.CloseExpiredContracts({})",
	)
	badTag, err := json.Marshal(map[string]string{
		"SYSLOG_TIMESTAMP": now.Format(time.RFC3339Nano), "MESSAGE": "synthetic ignored record", "CONTAINER_TAG": workerOptionalPrivateText,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, wantClass string
		wantActive      bool
	}{
		{name: "unavailable", wantClass: observationErrorClassAccessDenied},
		{name: "partial fleet", wantClass: observationErrorClassAccessDenied, wantActive: true},
		{name: "partial normalized", wantClass: observationErrorClassUnclassified, wantActive: true},
		{name: "complete fallback", wantActive: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := &syntheticSource{
				hostFn: func(_ HostSettings, command string) (string, error) {
					if strings.Contains(command, "monitor_rate") {
						return ratePayload, nil
					}
					return heapPayload, nil
				},
				localFn: func(string, ...string) (string, error) {
					return workerOptionalPrivateText, errors.New("synthetic gateway: " + workerOptionalPrivateText)
				},
				hostTimeoutFn: func(host HostSettings, command string, timeout time.Duration) (string, error) {
					if timeout != taskworkerJournalTimeout || !strings.Contains(command, "--grep='eval'") {
						t.Error("unexpected lifecycle fallback command or bound")
					}
					if test.name == "unavailable" || host.Name == "metrics-2" {
						return workerOptionalPrivateText, errors.New("permission denied: " + workerOptionalPrivateText)
					}
					if test.name == "partial normalized" {
						return journal + "\n" + string(badTag), nil
					}
					return journal, nil
				},
			}
			settings := workerMemorySyntheticSettings(source, now)
			if test.name == "partial fleet" {
				settings.Hosts = append(settings.Hosts, HostSettings{Name: "metrics-2", Roles: []string{"services"}})
			}
			alerts, err := NewWithSignals(settings, newSignal()).Run(context.Background())
			if err != nil || len(alerts) != 1 {
				t.Fatal("optional lifecycle failure replaced or erased the primary worker finding")
			}
			alert := requireAlertClass(t, alerts, class)
			if alert.Target != "edge-3/g2" || alert.Frame != "hot" || alert.Severity != SeverityWarn || alert.Sustain != 2 {
				t.Fatal("optional lifecycle result changed the primary worker identity or sustain")
			}
			if got := strings.Contains(alert.Observed, "active_tasks=CloseExpiredContracts:130s"); got != test.wantActive {
				t.Fatal("partial lifecycle result lost valid attribution or invented unavailable attribution")
			}
			if test.wantActive && !strings.Contains(alert.Observed, "active_log_source=host-journal-fallback") {
				t.Fatal("partial lifecycle result lost its authoritative source qualifier")
			}
			if got := strings.Contains(alert.Evidence, "task-lifecycle lookup was degraded"); got != (test.wantClass != "") {
				t.Fatal("complete and degraded lifecycle observations were conflated")
			}
			requireWorkerOptionalErrorPrivacy(t, alert, test.wantClass)
		})
	}
}

func TestWorkerMemoryOptionalLifecycleErrorsStayPrivate(t *testing.T) {
	testWorkerOptionalLifecycleErrorPrivacy(t, NewWorkerMemorySignal, "worker-memory-skew")
}

func TestWorkerMemorySignalSyntheticFallsBackToAnotherServiceGateway(t *testing.T) {
	now := time.Date(2026, 8, 30, 21, 57, 10, 0, time.UTC)
	const gib = float64(uint64(1) << 30)
	workers := []workerMetricFixture{
		{host: "edge-0", block: "g1", instance: "a", heap: 0.125 * gib, cpuRate: 0.02, allocRate: 1 << 20, gcRate: 0.01, gcPauseRate: 0.000001},
		{host: "edge-1", block: "g1", instance: "hot", heap: 12 * gib, cpuRate: 4, allocRate: 700 << 20, gcRate: 0.125, gcPauseRate: 0.00004},
		{host: "edge-3", block: "g1", instance: "c", heap: 0.25 * gib, cpuRate: 0.04, allocRate: 3 << 20, gcRate: 0.03, gcPauseRate: 0.000003},
	}
	payload := workerMetricsFixtureJSON(t, now, workers...)
	ratePayload := workerRatesFixtureJSON(t, now, workers...)
	calls := []string{}
	source := &syntheticSource{
		hostFn: func(host HostSettings, command string) (string, error) {
			query := "metrics"
			if strings.Contains(command, "monitor_rate") {
				query = "rates"
			}
			calls = append(calls, host.Name+":"+query)
			if host.Name == "metrics-1" {
				return "", errors.New("synthetic gateway timeout")
			}
			if query == "rates" {
				return ratePayload, nil
			}
			return payload, nil
		},
		localFn: func(string, ...string) (string, error) { return "", nil },
	}
	settings := workerMemorySyntheticSettings(source, now)
	settings.Hosts = append(settings.Hosts, HostSettings{Name: "metrics-2", Roles: []string{"services"}})

	alerts, err := NewWorkerMemorySignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	markdown := requireAlertClass(t, alerts, "worker-memory-skew").Markdown()
	if !strings.Contains(markdown, "12.00GiB of allocated Go heap") || !strings.Contains(markdown, "cpu_cores_5m=4.000") {
		t.Fatalf("fallback did not preserve the primary and rate observations:\n%s", markdown)
	}
	wantCalls := []string{"metrics-1:metrics", "metrics-2:metrics", "metrics-2:rates"}
	if fmt.Sprint(calls) != fmt.Sprint(wantCalls) {
		t.Fatalf("gateway calls = %v, want %v", calls, wantCalls)
	}
}
