package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"
)

func requireWorkerScorePhaseSourceQuery(t *testing.T, command string) {
	t.Helper()
	query, err := url.QueryUnescape(command)
	if err != nil {
		t.Fatal(err)
	}
	// Source-time gates must sit inside each family expression. Filtering the
	// final instant result by its evaluation timestamp would accept old data.
	for _, metric := range []string{
		"urnetwork_update_client_scores_phase_active",
		"urnetwork_update_client_scores_phase_duration_seconds_total",
		"urnetwork_update_client_scores_phase_exits_total",
		"urnetwork_update_client_scores_phase_work_items_total",
		"urnetwork_update_client_scores_phase_work_bytes_total",
	} {
		selector := metric + `{env="synthetic",job="taskworker"}`
		gate := " and (timestamp(" + selector + ") >= time() - 90) and (timestamp(" + selector + ") <= time() + 30)"
		if !strings.Contains(query, gate) {
			t.Fatalf("phase family %s lacks its original source-time gate", metric)
		}
	}
	if strings.Count(query, "label_replace(") != 5 || strings.Count(query, "timestamp(") != 10 ||
		strings.Count(query, "rate(") != 4 || strings.Count(query, " or ") != 4 ||
		strings.Contains(query, " bool ") || !strings.Contains(command, "--max-time 15 --max-filesize 1048576") {
		t.Fatal("phase query changed its bounded five-family, single-request contract")
	}
	for _, forbidden := range []string{"device_id", "network_id", "task_id", "address", "group_left", "group_right"} {
		if strings.Contains(query, forbidden) {
			t.Fatalf("phase query added an unbounded/private dimension %q", forbidden)
		}
	}
}

// Model the source-time-filtered Mimir result while retaining a fresh instant
// evaluation timestamp. The command contract above is asserted separately;
// these fixtures do not pretend to execute a PromQL engine.
func workerScorePhaseSourceFilteredFixture(t *testing.T, now time.Time, command, mode string) string {
	t.Helper()
	requireWorkerScorePhaseSourceQuery(t, command)
	var response mimirInstantResponse
	if err := json.Unmarshal([]byte(workerScorePhaseFixtureJSON(t, now, "worker-hot", "g2", "runtime-hot", true)), &response); err != nil {
		t.Fatal(err)
	}
	rows := response.Data.Result
	response.Data.Result = nil
	for _, row := range rows {
		family := row.Metric["monitor_score_phase_metric"]
		if family == "bytes" && row.Metric["phase"] == "cache_write" {
			switch mode {
			case "incomplete", "new-process-one-scrape":
				continue // Missing rate output is not a fresh zero.
			case "invalid-value":
				row.Value[1] = json.RawMessage(`"NaN"`)
			}
		}
		sourceAge := 5 * time.Second
		if mode == "stale-active-source" && family == "active" || mode == "stale-counter-source" && family == "bytes" {
			sourceAge = 91 * time.Second
		}
		if mode == "future-source" && family == "items" {
			sourceAge = -31 * time.Second
		}
		if sourceAge > 90*time.Second || sourceAge < -30*time.Second {
			continue // timestamp(original metric) filter, not evaluation time.
		}
		switch mode {
		case "wrong-generation":
			row.Metric["instance"] = "runtime-previous"
		case "wrong-host":
			row.Metric["host"] = "worker-peer"
		case "wrong-block":
			row.Metric["block"] = "g3"
		case "missing-generation":
			delete(row.Metric, "instance")
		case "stale-evaluation":
			row.Value[0] = json.RawMessage(fmt.Sprint(now.Add(-2 * time.Minute).Unix()))
		}
		// Never reflect additional runtime labels or unknown phase values into
		// Markdown/JSON. Only the fixed phase-name and numeric fields may escape.
		row.Metric["network_id"] = "private-network-canary"
		row.Metric["device_id"] = "private-device-canary"
		row.Metric["address"] = "198.51.100.23"
		response.Data.Result = append(response.Data.Result, row)
	}
	if mode == "unknown-phase" {
		for _, row := range rows {
			row.Metric["phase"] = "private-phase-canary"
		}
		response.Data.Result = rows
	}
	if strings.HasPrefix(mode, "duplicate-") {
		duplicateFamily := "active"
		if family := strings.TrimPrefix(mode, "duplicate-"); family == "duration" || family == "exits" || family == "items" || family == "bytes" {
			duplicateFamily = family
		}
		for _, row := range response.Data.Result {
			if row.Metric["phase"] != "gob_encode" || row.Metric["monitor_score_phase_metric"] != duplicateFamily {
				continue
			}
			duplicate := row
			duplicate.Metric = map[string]string{}
			for key, value := range row.Metric {
				duplicate.Metric[key] = value
			}
			duplicate.Metric["ignored_replica"] = "private-duplicate-canary"
			duplicate.Value = append([]json.RawMessage(nil), row.Value...)
			if mode != "duplicate-equal" {
				duplicate.Value[1] = json.RawMessage(`"999"`)
			}
			if mode == "duplicate-invalid" {
				duplicate.Value[1] = json.RawMessage(`"NaN"`)
			}
			response.Data.Result = append(response.Data.Result, duplicate)
			if mode == "duplicate-first" {
				last := len(response.Data.Result) - 1
				response.Data.Result[0], response.Data.Result[last] = response.Data.Result[last], response.Data.Result[0]
			}
			break
		}
	}
	payload, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	return string(payload)
}

func TestWorkerScorePhaseEvidencePreservesRealAlerts(t *testing.T) {
	now := time.Date(2026, 9, 16, 11, 58, 30, 0, time.UTC)
	for _, probe := range []struct {
		name, class string
		newSignal   func() Signal
		cpu, alloc  float64
	}{
		{"heap below churn guards", "worker-memory-skew", NewWorkerMemorySignal, 1.4, 200 << 20},
		{"churn", "worker-cpu-allocation-churn", NewWorkerChurnSignal, 4, 320 << 20},
	} {
		for _, mode := range []string{
			"complete", "incomplete", "stale-active-source", "stale-counter-source", "future-source",
			"wrong-generation", "wrong-host", "wrong-block", "missing-generation", "stale-evaluation",
			"new-process-one-scrape", "invalid-value", "unknown-phase", "query-failed",
			"duplicate-equal", "duplicate-conflicting", "duplicate-first", "duplicate-invalid",
			"duplicate-duration", "duplicate-exits", "duplicate-items", "duplicate-bytes", "response-too-large",
		} {
			t.Run(probe.name+"/"+mode, func(t *testing.T) {
				workers := []workerMetricFixture{
					{host: "worker-peer", block: "g1", instance: "peer-a", heap: 1 << 28, cpuRate: .1, allocRate: 2 << 20},
					{host: "worker-other", block: "g1", instance: "peer-b", heap: 1 << 29, cpuRate: .2, allocRate: 4 << 20},
					{host: "worker-hot", block: "g2", instance: "runtime-hot", heap: 9 << 30, cpuRate: probe.cpu, allocRate: probe.alloc},
				}
				phaseReads := 0
				source := &syntheticSource{
					hostFn: func(_ HostSettings, command string) (string, error) {
						switch {
						case strings.Contains(command, "monitor_score_phase_metric"):
							phaseReads++
							if mode == "query-failed" {
								return "", errors.New("private-query-error-canary http://198.51.100.23/raw")
							}
							if mode == "response-too-large" {
								requireWorkerScorePhaseSourceQuery(t, command)
								return strings.Repeat(" ", workerScorePhaseResponseMax) + "private-overflow-canary", nil
							}
							return workerScorePhaseSourceFilteredFixture(t, now, command, mode), nil
						case strings.Contains(command, "monitor_rate"):
							return workerRatesFixtureJSON(t, now, workers...), nil
						default:
							return workerMetricsFixtureJSON(t, now, workers...), nil
						}
					},
					localFn: func(string, ...string) (string, error) {
						return "[worker-hot][taskworker][g2][cid:runtime-hot][I][2026-09-16T11:58:25Z][task.go:1938][01a0530b-0e6a-9c14-6694-11a165f3c27b]eval active(101s) synthetic/taskworker/work.UpdateClientScores({})", nil
					},
					redisFn: func(HostSettings, int, ...string) (string, error) { return redisScoreAliasReadyValue, nil },
				}
				alerts, err := probe.newSignal().Run(context.Background(), workerMemorySyntheticSettings(source, now))
				if err != nil {
					t.Fatal(err)
				}
				alert := requireAlertClass(t, alerts, probe.class)
				if len(alerts) != 1 || alert.Target != "worker-hot/g2" || alert.Frame != "runtime-hot" || alert.Sustain != 2 || alert.Severity != SeverityWarn {
					t.Fatal("phase enrichment changed or suppressed the real process alert")
				}
				if phaseReads != 1 {
					t.Fatalf("phase reads = %d, want 1", phaseReads)
				}
				markdown := alert.Markdown()
				if mode == "complete" {
					for _, want := range []string{"score_phase_observability=ready", "12 active gob_encode spans", "not heap ownership", "work bytes are not process heap allocations", "completed-span seconds are not CPU time", "underlying source timestamp"} {
						if !strings.Contains(markdown, want) {
							t.Fatalf("complete phase evidence omitted %q", want)
						}
					}
				} else {
					for _, want := range []string{"score_phase_observability=unavailable", "explicitly unobservable, not healthy", "no partial set or other worker was substituted"} {
						if !strings.Contains(markdown, want) {
							t.Fatalf("unobservable phase evidence omitted %q", want)
						}
					}
					requireAlertOmits(t, alert, "score_phase_observability=ready", "active gob_encode spans")
				}
				encoded, err := json.Marshal(alert)
				if err != nil {
					t.Fatal(err)
				}
				for _, private := range []string{"private-network-canary", "private-device-canary", "private-phase-canary", "private-query-error-canary", "private-duplicate-canary", "private-overflow-canary", "198.51.100.23", "01a0530b-0e6a-9c14-6694-11a165f3c27b"} {
					if strings.Contains(markdown, private) || strings.Contains(string(encoded), private) {
						t.Fatalf("phase evidence leaked %q", private)
					}
				}
			})
		}
	}
}

func TestWorkerScorePhaseResponseByteLimit(t *testing.T) {
	now := time.Date(2026, 9, 16, 11, 58, 30, 0, time.UTC)
	payload := workerScorePhaseFixtureJSON(t, now, "worker-hot", "g2", "runtime-hot", true)
	if len(payload) >= workerScorePhaseResponseMax {
		t.Fatal("ordinary fixed-family response exceeds the aggregate cap")
	}
	atLimit := payload + strings.Repeat(" ", workerScorePhaseResponseMax-len(payload))
	for _, overflow := range []bool{false, true} {
		t.Run(fmt.Sprintf("overflow=%t", overflow), func(t *testing.T) {
			source := &syntheticSource{hostFn: func(_ HostSettings, command string) (string, error) {
				requireWorkerScorePhaseSourceQuery(t, command)
				if overflow {
					return atLimit + "private-overflow-canary", nil
				}
				return atLimit, nil
			}}
			env, err := newProbeEnv(workerMemorySyntheticSettings(source, now).withDefaults())
			if err != nil {
				t.Fatal(err)
			}
			observations, err := loadWorkerScorePhaseObservations(context.Background(), env, env.cfg.hostsWithRole("services"), nil)
			if overflow {
				if err == nil || err.Error() != "phase metrics response exceeds byte limit" || len(observations) != 0 {
					t.Fatalf("overflow did not fail closed with a fixed privacy-safe error: %v", err)
				}
			} else if err != nil || len(observations) != 1 {
				t.Fatalf("exact-cap valid aggregate was rejected: observations=%d err=%v", len(observations), err)
			}
		})
	}
}

func TestWorkerScorePhaseDuplicateDoesNotInvalidateAnotherWorker(t *testing.T) {
	now := time.Date(2026, 9, 16, 11, 58, 30, 0, time.UTC)
	source := &syntheticSource{hostFn: func(_ HostSettings, command string) (string, error) {
		var ambiguous, healthy mimirInstantResponse
		if err := json.Unmarshal([]byte(workerScorePhaseSourceFilteredFixture(t, now, command, "duplicate-conflicting")), &ambiguous); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(workerScorePhaseFixtureJSON(t, now, "worker-control", "g2", "runtime-control", true)), &healthy); err != nil {
			t.Fatal(err)
		}
		ambiguous.Data.Result = append(ambiguous.Data.Result, healthy.Data.Result...)
		payload, err := json.Marshal(ambiguous)
		if err != nil {
			t.Fatal(err)
		}
		return string(payload), nil
	}}
	env, err := newProbeEnv(workerMemorySyntheticSettings(source, now).withDefaults())
	if err != nil {
		t.Fatal(err)
	}
	observations, err := loadWorkerScorePhaseObservations(context.Background(), env, env.cfg.hostsWithRole("services"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := observations[workerScorePhaseKey("worker-control", "g2", "runtime-control")]; !ok || len(observations) != 1 {
		t.Fatal("duplicate series must invalidate only the affected exact worker")
	}
}

func TestWorkerScorePhaseQueryCostBoundary(t *testing.T) {
	now := time.Date(2026, 9, 16, 11, 58, 30, 0, time.UTC)
	for _, newSignal := range []func() Signal{NewWorkerMemorySignal, NewWorkerChurnSignal} {
		for _, mode := range []string{"two-outliers", "healthy", "non-score", "stale-heartbeat"} {
			t.Run(newSignal().Key()+"/"+mode, func(t *testing.T) {
				workers := []workerMetricFixture{
					{host: "peer-a", block: "g1", instance: "a", heap: 1 << 28, cpuRate: .1, allocRate: 1 << 20},
					{host: "peer-b", block: "g1", instance: "b", heap: 1 << 28, cpuRate: .1, allocRate: 1 << 20},
					{host: "peer-c", block: "g1", instance: "c", heap: 1 << 28, cpuRate: .1, allocRate: 1 << 20},
					{host: "worker-hot", block: "g2", instance: "runtime-hot", heap: 9 << 30, cpuRate: 4, allocRate: 320 << 20},
					{host: "worker-hot", block: "g3", instance: "runtime-second", heap: 10 << 30, cpuRate: 4.1, allocRate: 330 << 20},
				}
				if mode == "healthy" {
					for i := range workers {
						workers[i].heap, workers[i].cpuRate, workers[i].allocRate = 1<<28, .1, 1<<20
					}
				}
				phaseReads, totalReads := 0, 0
				source := &syntheticSource{
					hostFn: func(_ HostSettings, command string) (string, error) {
						totalReads++
						if strings.Contains(command, "monitor_score_phase_metric") {
							phaseReads++
							requireWorkerScorePhaseSourceQuery(t, command)
							return workerScorePhaseFixtureJSON(t, now, "worker-hot", "g2", "runtime-hot", true), nil
						}
						if strings.Contains(command, "monitor_rate") {
							return workerRatesFixtureJSON(t, now, workers...), nil
						}
						return workerMetricsFixtureJSON(t, now, workers...), nil
					},
					localFn: func(string, ...string) (string, error) {
						family, observedAt := "UpdateClientScores", now.Add(-5*time.Second)
						if mode == "non-score" {
							family = "ProviderEgressProbe"
						}
						if mode == "stale-heartbeat" {
							observedAt = now.Add(-2 * time.Minute)
						}
						lines := []string{}
						for i, worker := range workers[3:] {
							lines = append(lines, fmt.Sprintf("[%s][taskworker][%s][cid:%s][I][%s][task.go:1938][01a0530b-0e6a-9c14-6694-11a165f3c27%d]eval active(101s) synthetic/taskworker/work.%s({})", worker.host, worker.block, worker.instance, observedAt.Format(time.RFC3339Nano), i, family))
						}
						return strings.Join(lines, "\n"), nil
					},
					redisFn: func(HostSettings, int, ...string) (string, error) { return "", nil },
				}
				alerts, err := newSignal().Run(context.Background(), workerMemorySyntheticSettings(source, now))
				if err != nil {
					t.Fatal(err)
				}
				wantPhaseReads, wantAlerts := 0, 2
				if mode == "two-outliers" {
					wantPhaseReads = 1
					if len(alerts) != 2 || !strings.Contains(alerts[0].Markdown(), "score_phase_observability=ready") || !strings.Contains(alerts[1].Markdown(), "score_phase_observability=unavailable") {
						t.Fatal("two outliers must share one query but never each other's phase evidence")
					}
				}
				if mode == "healthy" {
					wantAlerts = 0
				}
				baseReads := 1
				if newSignal().Key() == "worker-memory" {
					baseReads = 2
				}
				if len(alerts) != wantAlerts || phaseReads != wantPhaseReads || totalReads != baseReads+wantPhaseReads {
					t.Fatalf("alerts=%d phase_reads=%d total_reads=%d, want %d/%d/%d", len(alerts), phaseReads, totalReads, wantAlerts, wantPhaseReads, baseReads+wantPhaseReads)
				}
			})
		}
	}
}
