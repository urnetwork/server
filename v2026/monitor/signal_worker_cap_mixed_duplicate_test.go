// Mixed valid/rejected duplicates must not restore exact-runtime authority.
package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// Adds exactly one metric row to an otherwise complete fixture, preserving
// malformed strings for the public parser and avoiding other duplicate kinds.
func workerCapSingleDuplicatePayload(t *testing.T, now time.Time, kind, value string, age time.Duration, first bool, workers ...workerCapFixture) string {
	t.Helper()
	var response struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string            `json:"resultType"`
			Result     []json.RawMessage `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(workerCapFixtureJSON(t, now, workers...)), &response); err != nil {
		t.Fatal(err)
	}
	worker := workers[0]
	duplicate, err := json.Marshal(map[string]any{
		"metric": map[string]string{
			"monitor_metric": kind,
			"host":           worker.host, "block": worker.block, "instance": worker.instance,
		},
		"value": []any{float64(now.Add(-age).Unix()), value},
	})
	if err != nil {
		t.Fatal(err)
	}
	if first {
		response.Data.Result = append([]json.RawMessage{duplicate}, response.Data.Result...)
	} else {
		response.Data.Result = append(response.Data.Result, duplicate)
	}
	payload, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	return string(payload)
}

// A single rejected duplicate is unknown in both orders for every mandatory
// metric, alone and beside independent quiet or pressured complete siblings.
func TestWorkerCapSignalRejectedDuplicateRemainsUnknown(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	for _, kind := range []string{"cpu", "maxprocs", "goroutines"} {
		for _, rejected := range []struct {
			name  string
			value string
			age   time.Duration
		}{
			{name: "nan", value: "NaN"},
			{name: "positive-infinity", value: "+Inf"},
			{name: "negative-infinity", value: "-Inf"},
			{name: "negative", value: "-1"},
			{name: "stale", value: "1", age: 91 * time.Second},
			{name: "future", value: "1", age: -31 * time.Second},
		} {
			for _, first := range []bool{false, true} {
				for siblings := 0; siblings < 3; siblings++ {
					workers := []workerCapFixture{{host: "synthetic-a", block: "g1", instance: "synthetic-private-duplicate", cpu: 1, maxprocs: 4, goroutines: 1000}}
					wantCapacity := 0
					if siblings != 0 {
						sibling := workerCapFixture{host: "synthetic-b", block: "g2", instance: "synthetic-sibling", cpu: 1, maxprocs: 4, goroutines: 1000}
						if siblings == 2 {
							sibling.cpu, sibling.goroutines = 3.6, 10_000
							wantCapacity = 1
						}
						workers = append(workers, sibling)
					}
					control, err := NewWorkerCapSignal().Run(context.Background(), workerCapSyntheticSettings(t, now, workers...))
					if err != nil || requireAlertClassCount(control, "worker-scheduler-capacity") != wantCapacity || requireAlertClassCount(control, "worker-scheduler-unobservable") != 0 {
						t.Fatalf("%s/%s first=%t siblings=%d: complete control changed", kind, rejected.name, first, siblings)
					}
					payload := workerCapSingleDuplicatePayload(t, now, kind, rejected.value, rejected.age, first, workers...)
					source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) { return payload, nil }}
					settings := workerMemorySyntheticSettings(source, now)
					alerts, err := NewWorkerCapSignal().Run(context.Background(), settings)
					if err != nil {
						t.Fatalf("%s/%s first=%t siblings=%d: parsed rejection became a source error", kind, rejected.name, first, siblings)
					}
					if requireAlertClassCount(alerts, "worker-scheduler-unobservable") != 1 {
						t.Fatalf("rejected duplicate retained healthy authority: kind=%s rejection=%s first=%t siblings=%d", kind, rejected.name, first, siblings)
					}
					visibility := requireAlertClass(t, alerts, "worker-scheduler-unobservable")
					paired := 0
					if siblings != 0 {
						paired = 1
					}
					if visibility.SignalID != "runtime/worker-scheduler-capacity" || visibility.Target != settings.Environment+"/taskworker" || visibility.Frame != "" || visibility.Sustain != 2 || !visibility.unobservable ||
						!strings.Contains(visibility.Markdown(), fmt.Sprintf("paired_runtimes=%d incomplete_runtimes=1", paired)) || strings.Contains(visibility.Markdown(), workers[0].instance) {
						t.Fatalf("%s/%s first=%t siblings=%d: visibility identity, counts or privacy changed", kind, rejected.name, first, siblings)
					}
					if requireAlertClassCount(alerts, "worker-scheduler-capacity") != wantCapacity {
						t.Fatalf("%s/%s first=%t siblings=%d: independent pressure changed", kind, rejected.name, first, siblings)
					}
					if wantCapacity != 0 {
						capacity := requireAlertClass(t, alerts, "worker-scheduler-capacity")
						if capacity.Target != "synthetic-b/g2" || capacity.Frame != "synthetic-sibling" {
							t.Fatal("ambiguous runtime replaced the independent pressure target")
						}
					}
					// Public Run filters healthy findings; inspect the owning reducer
					// as well to prove the rejected runtime has no healthy verdict.
					env, err := newProbeEnv(settings.withDefaults())
					if err != nil {
						t.Fatal(err)
					}
					findings, err := (workerCapProbe{}).check(context.Background(), env)
					if err != nil {
						t.Fatal(err)
					}
					capacityFindings := 0
					for _, finding := range findings {
						if finding.class == "worker-scheduler-capacity" {
							capacityFindings++
							if finding.frame != "synthetic-sibling" || finding.healthy != (siblings == 1) {
								t.Fatal("rejected duplicate retained capacity or changed the healthy sibling")
							}
						}
					}
					if capacityFindings != paired {
						t.Fatal("complete sibling capacity finding disappeared")
					}
				}
			}
		}
	}
}

// A malformed duplicate remains a fixed whole-probe error in either order;
// unlike parsed rejection it cannot publish even independent sibling findings.
func TestWorkerCapSignalMalformedDuplicateRemainsSourceError(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	for _, kind := range []string{"cpu", "maxprocs", "goroutines"} {
		for _, first := range []bool{false, true} {
			payload := workerCapSingleDuplicatePayload(t, now, kind, "synthetic-private-invalid-scalar", 0, first,
				workerCapFixture{host: "synthetic-a", block: "g1", instance: "synthetic-quiet", cpu: 1, maxprocs: 4, goroutines: 1000},
				workerCapFixture{host: "synthetic-b", block: "g2", instance: "synthetic-pressure", cpu: 3.6, maxprocs: 4, goroutines: 10_000})
			source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) { return payload, nil }}
			alerts, err := NewWorkerCapSignal().Run(context.Background(), workerMemorySyntheticSettings(source, now))
			if err == nil || err.Error() != "worker cap: invalid metric value" || len(alerts) != 0 {
				t.Fatalf("%s first=%t: malformed duplicate changed source-error/privacy contract", kind, first)
			}
		}
	}
}

// The public signal, real cadence gate and notifier retain an existing pressure
// incident through held/raised unknown and require two corrected quiet runs.
func TestWorkerCapRejectedDuplicateNotifierRequiresCompleteRecovery(t *testing.T) {
	for _, first := range []bool{false, true} {
		now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
		signal := NewWorkerCapSignal()
		gate := newCadenceAlertGate()
		notifier := &SlackAlertNotifier{settings: SlackAlertNotifierSettings{ClearAfterCompleteRuns: 2}, keyIncidents: map[string]*slackIncident{}, signalKeyCompleteRuns: map[string]int{}}
		for run := 0; run < 6; run++ {
			runtime := workerCapFixture{host: "synthetic-a", block: "g1", instance: "synthetic-incident", cpu: 1, maxprocs: 4, goroutines: 1000}
			if run < 2 {
				runtime.cpu, runtime.goroutines = 3.6, 10_000
			}
			payload := workerCapFixtureJSON(t, now, runtime)
			if run == 2 || run == 3 {
				payload = workerCapSingleDuplicatePayload(t, now, "cpu", "NaN", 0, first, runtime)
			}
			source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) { return payload, nil }}
			alerts, err := signal.Run(context.Background(), workerMemorySyntheticSettings(source, now))
			if err != nil {
				t.Fatal("notifier source observation failed")
			}
			ready, held := gate.split(signal, alerts)
			transitions := notifier.observeWithLock(now, signal.Key(), SignalExecution{Alerts: ready, HeldAlerts: held})
			pressureRaised, pressureCleared, visibilityRaised, visibilityCleared := 0, 0, 0, 0
			for _, transition := range transitions {
				switch transition.alert.Class {
				case "worker-scheduler-capacity":
					if transition.kind == slackRaised {
						pressureRaised++
					}
					if transition.kind == slackCleared {
						pressureCleared++
					}
				case "worker-scheduler-unobservable":
					if transition.kind == slackRaised {
						visibilityRaised++
					}
					if transition.kind == slackCleared {
						visibilityCleared++
					}
				}
			}
			wantPressureRaised, wantVisibilityRaised, wantCleared := 0, 0, 0
			if run == 1 {
				wantPressureRaised = 1
			}
			if run == 3 {
				wantVisibilityRaised = 1
			}
			if run == 5 {
				wantCleared = 1
			}
			if pressureRaised != wantPressureRaised || visibilityRaised != wantVisibilityRaised || pressureCleared != wantCleared || visibilityCleared != wantCleared {
				t.Fatalf("first=%t run=%d: held/raised unknown or corrected recovery changed", first, run)
			}
			if run == 0 && (len(held) != 1 || held[0].Class != "worker-scheduler-capacity") || run == 2 && (len(held) != 1 || held[0].Class != "worker-scheduler-unobservable") {
				t.Fatalf("first=%t run=%d: sustain gate did not hold first observation", first, run)
			}
			now = now.Add(time.Minute)
		}
	}
}

// A single finite duplicate stays ambiguous even if both values agree;
// retaining the tuple's values must not override uniqueness or sibling pressure.
func TestWorkerCapSignalSingleFiniteDuplicateRemainsUnknown(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	for _, metric := range []struct {
		kind  string
		value string
	}{
		{kind: "cpu", value: "1"},
		{kind: "maxprocs", value: "4"},
		{kind: "goroutines", value: "1000"},
	} {
		for _, value := range []string{metric.value, "2"} {
			for _, first := range []bool{false, true} {
				payload := workerCapSingleDuplicatePayload(t, now, metric.kind, value, 0, first,
					workerCapFixture{host: "synthetic-a", block: "g1", instance: "synthetic-duplicate", cpu: 1, maxprocs: 4, goroutines: 1000},
					workerCapFixture{host: "synthetic-b", block: "g2", instance: "synthetic-pressure", cpu: 3.6, maxprocs: 4, goroutines: 10_000})
				source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) { return payload, nil }}
				alerts, err := NewWorkerCapSignal().Run(context.Background(), workerMemorySyntheticSettings(source, now))
				if err != nil || requireAlertClassCount(alerts, "worker-scheduler-unobservable") != 1 || requireAlertClassCount(alerts, "worker-scheduler-capacity") != 1 {
					t.Fatalf("%s first=%t: finite duplicate or independent pressure changed", metric.kind, first)
				}
				if !strings.Contains(requireAlertClass(t, alerts, "worker-scheduler-unobservable").Markdown(), "paired_runtimes=1 incomplete_runtimes=1") || requireAlertClass(t, alerts, "worker-scheduler-capacity").Frame != "synthetic-pressure" {
					t.Fatal("finite duplicate replaced the independent pressure runtime")
				}
			}
		}
	}
}
