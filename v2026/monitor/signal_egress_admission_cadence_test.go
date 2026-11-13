package monitor

// SIGNALS.md §2.19a requires source-owned recent intervals at the operator's
// active cadence. Manual ticks distinguish that clock from scrape history.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Emits what the requested query actually observes. The legacy query gets
// only its current bundle; the proposed window query gets both native points.
func egressAdmissionCadencePayload(t *testing.T, now time.Time, current, previous []egressAdmissionTestProcess, paired bool) string {
	t.Helper()
	if !paired {
		return egressAdmissionTestJson(t, now, current)
	}
	rows := []map[string]any{}
	for _, point := range []struct {
		window    string
		at        time.Time
		processes []egressAdmissionTestProcess
	}{
		{window: "current", at: now, processes: current},
		{window: "previous", at: now.Add(-time.Minute), processes: previous},
	} {
		var response struct {
			Data struct {
				Result []map[string]any `json:"result"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(egressAdmissionTestJson(t, point.at, point.processes)), &response); err != nil {
			t.Fatal(err)
		}
		for _, row := range response.Data.Result {
			row["metric"].(map[string]any)["monitor_egress_window"] = point.window
			// Mimir evaluates both sides now, while timestamp(selector offset)
			// carries the original scrape clock as the companion's value.
			row["value"].([]any)[0] = now.Unix()
			rows = append(rows, row)
		}
	}
	encoded, err := json.Marshal(map[string]any{
		"status": "success", "data": map[string]any{"resultType": "vector", "result": rows},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

// All mutations are synthetic native counter values. The executable start is
// fixed across observer ticks, so the red case isolates the observer-gap guard.
func runEgressAdmissionCadence(t *testing.T, modes ...string) []SignalExecution {
	t.Helper()
	anchor := time.Unix(1_700_000_000, 0).UTC()
	legacyBodies, pairedBodies := []string{}, []string{}
	for i, mode := range modes {
		now := anchor.Add(time.Duration(i+1) * 15 * time.Minute)
		current, previous := egressAdmissionTestProcesses(anchor), egressAdmissionTestProcesses(anchor)
		for _, processes := range [][]egressAdmissionTestProcess{current, previous} {
			processes[0].values["requests"] = 100 + float64(i)*100
			processes[0].values["selected/stale-health/false"] = 100 + float64(i)*100
			processes[1].values["submission/health/acknowledged"] = 100 + float64(i)*100
			processes[1].values["submission/attempt/acknowledged"] = 100 + float64(i)*100
		}
		current[0].values["requests"]++
		current[0].values["selected/stale-health/false"]++
		current[1].values["submission/health/acknowledged"]++
		current[1].values["submission/attempt/acknowledged"]++
		switch mode {
		case "clean":
		case "recent-expired":
			current[0].values["selected/stale-health/true"] = 1
		case "historical-expired":
			// The event is already present at the beginning of this recent
			// pair. It is not a newly observed recent admission failure.
			current[0].values["selected/stale-health/true"] = 1
			previous[0].values["selected/stale-health/true"] = 1
		default:
			t.Fatalf("unknown synthetic cadence mode %q", mode)
		}
		legacyBodies = append(legacyBodies, egressAdmissionCadencePayload(t, now, current, previous, false))
		pairedBodies = append(pairedBodies, egressAdmissionCadencePayload(t, now, current, previous, true))
	}
	var sourceCalls, clockNanos atomic.Int64
	clockNanos.Store(anchor.UnixNano())
	source := &syntheticSource{hostFn: func(_ HostSettings, command string) (string, error) {
		i := int(sourceCalls.Add(1) - 1)
		if i >= len(modes) {
			return "", fmt.Errorf("unexpected extra source request")
		}
		decoded, err := url.QueryUnescape(command)
		if err != nil {
			return "", err
		}
		if !strings.Contains(command, "--max-time 15") || !strings.Contains(command, "--max-filesize 2097152") {
			return "", fmt.Errorf("existing source limits changed")
		}
		if strings.Contains(decoded, `"monitor_egress_window"`) {
			return pairedBodies[i], nil
		}
		return legacyBodies[i], nil
	}}
	settings := syntheticSettings(source)
	settings.Hosts = []HostSettings{{Name: "synthetic-metrics.example", Roles: []string{"services"}}}
	settings.Now = func() time.Time { return time.Unix(0, clockNanos.Load()).UTC() }
	type cadenceTick struct {
		cadence time.Duration
		ticks   chan time.Time
	}
	created := make(chan cadenceTick, len(modes)+1)
	handled := make(chan SignalExecution, len(modes))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- NewWithSignals(settings, NewEgressAdmissionSignal()).runExecutionLoop(
			ctx, RunLoopOptions{MinimumProbeCadence: 15 * time.Minute},
			func(_ context.Context, _ Signal, execution SignalExecution) error {
				handled <- execution
				return nil
			},
			func(cadence time.Duration) runLoopTicker {
				ticks := make(chan time.Time, 1)
				created <- cadenceTick{cadence: cadence, ticks: ticks}
				return &manualRunLoopTicker{c: ticks}
			},
		)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("cadence loop cleanup: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("cadence loop did not join after cancellation")
		}
	})
	executions := []SignalExecution{}
	for i := range modes {
		var tick cadenceTick
		select {
		case tick = <-created:
		case <-time.After(5 * time.Second):
			t.Fatal("cadence loop did not arm its next manual barrier")
		}
		if tick.cadence != 15*time.Minute || sourceCalls.Load() != int64(i) {
			t.Fatal("active cadence or one-request-per-tick contract changed")
		}
		now := anchor.Add(time.Duration(i+1) * 15 * time.Minute)
		clockNanos.Store(now.UnixNano())
		tick.ticks <- now
		select {
		case execution := <-handled:
			if execution.Failed || len(execution.HeldAlerts) != 0 {
				t.Fatal("synthetic source did not reach the egress reducer")
			}
			executions = append(executions, execution)
		case <-time.After(5 * time.Second):
			t.Fatal("manual cadence did not complete its synthetic observation")
		}
		if sourceCalls.Load() != int64(i+1) {
			t.Fatal("one source request per scheduled observation was not preserved")
		}
	}
	return executions
}

func TestEgressAdmissionFifteenMinuteLoopHasObservableRecentHealthyInterval(t *testing.T) {
	executions := runEgressAdmissionCadence(t, "clean", "clean", "clean")
	requireAlertClass(t, executions[0].Alerts, "egress-admission-unobservable")
	for _, execution := range executions[1:] {
		if len(execution.Alerts) != 0 {
			t.Fatal("causal assertion: a complete advancing recent native pair remained unobservable solely because observer executions are fifteen minutes apart")
		}
	}
}

func TestEgressAdmissionFifteenMinuteLoopDetectsRecentExpiredAdmission(t *testing.T) {
	executions := runEgressAdmissionCadence(t, "clean", "recent-expired")
	requireAlertClass(t, executions[0].Alerts, "egress-admission-unobservable")
	if len(executions[1].Alerts) != 1 || executions[1].Alerts[0].Class != "egress-admission-expired" {
		t.Fatal("causal assertion: a recent expired native delta was lost solely because observer executions are fifteen minutes apart")
	}
	alert := executions[1].Alerts[0]
	if alert.Severity != SeverityWarn || alert.Sustain != 1 || alert.Frame != "stale-health" {
		t.Fatal("existing expired admission threshold, severity, sustain or lane changed")
	}
}

func TestEgressAdmissionFifteenMinuteLoopDoesNotInventRecentFailureFromResidualGap(t *testing.T) {
	executions := runEgressAdmissionCadence(t, "clean", "historical-expired", "historical-expired")
	requireAlertClass(t, executions[0].Alerts, "egress-admission-unobservable")
	for _, execution := range executions[1:] {
		if len(execution.Alerts) != 0 {
			t.Fatal("causal assertion: an event outside the recent native pair became a recent failure or prevented observation of current clean traffic")
		}
	}
}
