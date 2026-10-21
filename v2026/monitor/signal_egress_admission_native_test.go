package monitor

// Each malformed pair is exercised through the real bounded source adapter.
// No missing native history is supplied by prior observer counter values.

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Mutates synthetic response cells while preserving the surrounding envelope.
func changeEgressAdmissionNativeRows(t *testing.T, body string, change func([]map[string]any) []map[string]any) string {
	t.Helper()
	var response struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string           `json:"resultType"`
			Result     []map[string]any `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &response); err != nil {
		t.Fatal(err)
	}
	response.Data.Result = change(response.Data.Result)
	encoded, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func TestEgressAdmissionNativePairCannotBorrowMissingOrInvalidHistory(t *testing.T) {
	for _, mode := range []string{
		"missing-previous", "missing-current", "unknown-window", "missing-fixed-zero",
		"previous-capability-disabled", "stale-previous", "future-previous", "stale-current", "future-current",
		"mixed-previous-clocks", "same-source-point", "backward-source-points", "counter-reset", "different-start",
		"duplicate-previous", "overlapping-previous-generation",
	} {
		first := time.Unix(1_700_000_000, 0).UTC()
		processes := egressAdmissionTestProcesses(first)
		signal := NewEgressAdmissionSignal()
		runEgressAdmissionTest(t, signal, first, egressAdmissionTestJson(t, first, processes))
		now := first.Add(15 * time.Minute)
		body := egressAdmissionTestPairJson(t, now, egressAdmissionTestJson(t, now, processes))
		body = changeEgressAdmissionNativeRows(t, body, func(rows []map[string]any) []map[string]any {
			changed := []map[string]any{}
			for _, row := range rows {
				metric := row["metric"].(map[string]any)
				previous := metric["monitor_egress_window"] == "previous"
				value := row["value"].([]any)
				if mode == "missing-previous" && previous || mode == "missing-current" && !previous ||
					mode == "missing-fixed-zero" && previous && metric["monitor_egress_family"] == "pass_error" && metric["step"] == "pins" {
					continue
				}
				if mode == "unknown-window" && previous {
					metric["monitor_egress_window"] = "synthetic-unknown"
				}
				if metric["monitor_egress_part"] == "timestamp" {
					at, err := strconv.ParseFloat(value[1].(string), 64)
					if err != nil {
						t.Fatal(err)
					}
					switch {
					case mode == "stale-previous" && previous, mode == "stale-current" && !previous:
						at -= 91
					case mode == "future-previous" && previous:
						at = float64(now.Add(-time.Minute + 31*time.Second).Unix())
					case mode == "future-current" && !previous:
						at = float64(now.Add(31 * time.Second).Unix())
					case mode == "mixed-previous-clocks" && previous && metric["monitor_egress_family"] == "enabled":
						at--
					case mode == "same-source-point" && !previous:
						at -= 60
					case mode == "backward-source-points" && !previous:
						at -= 61
					}
					value[1] = strconv.FormatFloat(at, 'f', -1, 64)
				} else if previous {
					switch {
					case mode == "previous-capability-disabled" && metric["monitor_egress_family"] == "enabled":
						value[1] = "0"
					case mode == "counter-reset" && metric["monitor_egress_family"] == "requests":
						value[1] = "11"
					case mode == "different-start" && metric["monitor_egress_family"] == "start":
						value[1] = strconv.FormatInt(first.Add(-2*time.Hour).Unix(), 10)
					}
				}
				changed = append(changed, row)
				if mode == "duplicate-previous" && previous {
					changed = append(changed, row)
				}
				if mode == "overlapping-previous-generation" && previous && metric["job"] == "taskworker" {
					otherMetric := map[string]any{}
					for name, label := range metric {
						otherMetric[name] = label
					}
					otherMetric["instance"] = "synthetic-history-overlap"
					changed = append(changed, map[string]any{"metric": otherMetric, "value": value})
				}
			}
			return changed
		})
		alerts := runEgressAdmissionNativeTest(t, signal, now, body)
		if len(alerts) != 1 || alerts[0].Class != "egress-admission-unobservable" {
			t.Fatalf("%s native history manufactured health or an incident", mode)
		}
	}
}

func TestEgressAdmissionNativeSourceWindowKeepsThreeMinuteBoundary(t *testing.T) {
	for _, gap := range []time.Duration{3 * time.Minute, 3*time.Minute + time.Second} {
		first := time.Unix(1_700_000_000, 0).UTC()
		processes := egressAdmissionTestProcesses(first)
		signal := NewEgressAdmissionSignal()
		runEgressAdmissionTest(t, signal, first, egressAdmissionTestJson(t, first, processes))
		now := first.Add(15 * time.Minute)
		body := egressAdmissionTestPairJson(t, now, egressAdmissionTestJson(t, now, processes))
		body = changeEgressAdmissionNativeRows(t, body, func(rows []map[string]any) []map[string]any {
			for _, row := range rows {
				metric := row["metric"].(map[string]any)
				if metric["monitor_egress_part"] != "timestamp" {
					continue
				}
				// The existing +30s/-90s freshness edges admit exactly 180s
				// across a one-minute offset. One second farther is unknown.
				at := now.Add(30 * time.Second)
				if metric["monitor_egress_window"] == "previous" {
					at = at.Add(-gap)
				}
				row["value"].([]any)[1] = strconv.FormatInt(at.Unix(), 10)
			}
			return rows
		})
		alerts := runEgressAdmissionNativeTest(t, signal, now, body)
		if gap == 3*time.Minute && len(alerts) != 0 {
			t.Fatal("exact native interval/freshness boundary became unobservable")
		}
		if gap > 3*time.Minute && (len(alerts) != 1 || alerts[0].Class != "egress-admission-unobservable") {
			t.Fatal("out-of-range native history became healthy")
		}
	}
}

func TestEgressAdmissionNativePairStillWarmsEveryNewProcess(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	processes := egressAdmissionTestProcesses(now)
	signal := NewEgressAdmissionSignal()
	runEgressAdmissionTest(t, signal, now, egressAdmissionTestJson(t, now, processes))
	processes[0].instance = "synthetic-new-process"
	now = now.Add(15 * time.Minute)
	requireAlertClass(t, runEgressAdmissionTest(t, signal, now, egressAdmissionTestJson(t, now, processes)), "egress-admission-unobservable")
	now = now.Add(15 * time.Minute)
	if alerts := runEgressAdmissionTest(t, signal, now, egressAdmissionTestJson(t, now, processes)); len(alerts) != 0 {
		t.Fatal("new process did not finish warmup using its own complete native history")
	}
	// Replaying that exact native pair remains unknown even at a fresh query
	// evaluation timestamp, rather than double-counting its prior interval.
	body := egressAdmissionTestPairJson(t, now, egressAdmissionTestJson(t, now, processes))
	requireAlertClass(t, runEgressAdmissionNativeTest(t, signal, now.Add(time.Second), body), "egress-admission-unobservable")
}

func TestEgressAdmissionBadHistoricalSiblingKeepsKnownRecentFailure(t *testing.T) {
	first := time.Unix(1_700_000_000, 0).UTC()
	processes := egressAdmissionTestProcesses(first)
	signal := NewEgressAdmissionSignal()
	runEgressAdmissionTest(t, signal, first, egressAdmissionTestJson(t, first, processes))
	now := first.Add(15 * time.Minute)
	previous := egressAdmissionTestJson(t, now.Add(-time.Minute), processes)
	processes[1].values["submission/health/error_or_unknown"]++
	body := egressAdmissionTestPairJson(t, now, egressAdmissionTestJson(t, now, processes), previous)
	body = changeEgressAdmissionNativeRows(t, body, func(rows []map[string]any) []map[string]any {
		kept := []map[string]any{}
		for _, row := range rows {
			metric := row["metric"].(map[string]any)
			if metric["job"] != "api" || metric["monitor_egress_window"] != "previous" {
				kept = append(kept, row)
			}
		}
		return kept
	})
	alerts := runEgressAdmissionNativeTest(t, signal, now, body)
	requireAlertClass(t, alerts, "egress-admission-unobservable")
	incident := requireAlertClass(t, alerts, "egress-submission-unacknowledged")
	for _, phrase := range []string{"Time between scheduled observations is unsampled", "historical gap", "two clean traffic-bearing intervals", "direct deadline/submission controls"} {
		if !strings.Contains(incident.Markdown(), phrase) {
			t.Fatal("native interval scope or current recovery qualifier is missing")
		}
	}
}

// A previously confirmed event remains part of history, while two subsequent
// native intervals can show current clean traffic without covering the gap.
func TestEgressAdmissionFifteenMinuteLoopAllowsCurrentRecoveryWithoutRewritingGap(t *testing.T) {
	executions := runEgressAdmissionCadence(t, "clean", "recent-expired", "historical-expired", "historical-expired")
	requireAlertClass(t, executions[0].Alerts, "egress-admission-unobservable")
	incident := requireAlertClass(t, executions[1].Alerts, "egress-admission-expired")
	for _, execution := range executions[2:] {
		if len(execution.Alerts) != 0 {
			t.Fatal("old residual history prevented observation of current clean traffic")
		}
	}
	for _, phrase := range []string{
		"two complete fresh traffic-bearing process intervals", "direct §2.19 deadline health",
		"historical gap", "Time between scheduled observations is unsampled",
	} {
		if !strings.Contains(incident.Markdown(), phrase) {
			t.Fatal("current recovery lost its direct control or historical-gap qualifier")
		}
	}
	if incident.Severity != SeverityWarn || incident.Sustain != 1 || incident.Frame != "stale-health" {
		t.Fatal("current recovery changed the owning incident policy")
	}
}

func TestEgressAdmissionNativePairOverflowCannotBecomeTruncatedHealth(t *testing.T) {
	first := time.Unix(1_700_000_000, 0).UTC()
	processes := egressAdmissionTestProcesses(first)
	signal := NewEgressAdmissionSignal()
	runEgressAdmissionTest(t, signal, first, egressAdmissionTestJson(t, first, processes))
	now := first.Add(15 * time.Minute)
	body := egressAdmissionTestPairJson(t, now, egressAdmissionTestJson(t, now, processes))
	body = changeEgressAdmissionNativeRows(t, body, func(rows []map[string]any) []map[string]any {
		rows[0]["metric"].(map[string]any)["ignored_private_label"] = strings.Repeat("synthetic", egressAdmissionResponseMax/len("synthetic")+1)
		return rows
	})
	if len(body) <= egressAdmissionResponseMax || !json.Valid([]byte(body)) {
		t.Fatal("overflow control did not contain a valid oversized native pair")
	}
	alerts := runEgressAdmissionNativeTest(t, signal, now, body)
	if len(alerts) != 1 || alerts[0].Class != "egress-admission-unobservable" ||
		!strings.Contains(alerts[0].Observed, "reason=bounded-source-unavailable") {
		t.Fatal("oversized complete pair was truncated into healthy or incident evidence")
	}
}
