// Synthetic Mimir evaluations exercise the contract horizon reaper signal
// through its public Run. No Mimir is read.
package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// One synthetic evaluation: each class's value, absent when not listed.
type contractHorizonReaperFixture map[string]float64

// A reaper on, deleting and keeping up.
func contractHorizonReaperHealthyFixture() contractHorizonReaperFixture {
	return contractHorizonReaperFixture{
		"runs": 240, "error_runs": 0, "control_runs": 0, "progress_runs": 230, "block_points": 0,
		"mode": 2, "lag": 3600, "lag_before": 3600, "pass_start": 1.79e9, "pass_start_before": 1.79e9,
	}
}

func (self contractHorizonReaperFixture) with(changes contractHorizonReaperFixture, removed ...string) contractHorizonReaperFixture {
	values := contractHorizonReaperFixture{}
	for class, value := range self {
		values[class] = value
	}
	for class, value := range changes {
		values[class] = value
	}
	for _, class := range removed {
		delete(values, class)
	}
	return values
}

var contractHorizonReaperFixtureNow = time.Date(2026, 11, 2, 8, 0, 0, 0, time.UTC)

// A Mimir vector for the probe's query; extra series are added as given.
func contractHorizonReaperFixtureJSON(t testing.TB, values contractHorizonReaperFixture, age time.Duration, extra ...map[string]any) string {
	t.Helper()
	result := []map[string]any{}
	for class, value := range values {
		result = append(result, map[string]any{
			"metric": map[string]string{"monitor_metric": class},
			"value":  []any{float64(contractHorizonReaperFixtureNow.Add(-age).Unix()), fmt.Sprintf("%g", value)},
		})
	}
	result = append(result, extra...)
	payload, err := json.Marshal(map[string]any{
		"status": "success",
		"data":   map[string]any{"resultType": "vector", "result": result},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(payload)
}

// Runs the signal over payload, checking the query it sends.
func runContractHorizonReaperPayload(t *testing.T, payload string) []Alert {
	t.Helper()
	source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		for _, want := range []string{
			"urnetwork_contract_horizon_runs_total",
			"stop%3D%22error%22",
			"stop%3D%22control_unavailable%22",
			"stop%3D~%22horizon%7Cbudget%22",
			"urnetwork_contract_horizon_protected_contracts_total",
			"reason%3D%22block_points%22",
			"urnetwork_contract_horizon_mode",
			"urnetwork_contract_horizon_lag_seconds",
			"urnetwork_contract_horizon_pass_start_timestamp_seconds",
			"offset+6h",
			"%5B1h%5D",
			"%22synthetic%22",
			"monitor_metric",
		} {
			if !strings.Contains(command, want) {
				return "", fmt.Errorf("contract horizon reaper query missing %q: %s", want, command)
			}
		}
		return payload, nil
	}}
	settings := syntheticSettings(source)
	settings.Now = func() time.Time { return contractHorizonReaperFixtureNow }
	settings.Hosts = append(settings.Hosts, HostSettings{Name: "metrics-1", Roles: []string{"services"}})
	alerts, err := NewContractHorizonReaperSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	return alerts
}

func runContractHorizonReaperFixture(t *testing.T, values contractHorizonReaperFixture) []Alert {
	t.Helper()
	return runContractHorizonReaperPayload(t, contractHorizonReaperFixtureJSON(t, values, 0))
}

func TestContractHorizonReaperRegistered(t *testing.T) {
	found := 0
	for _, signal := range NewSignals() {
		if signal.Key() != "contract-horizon-reaper" {
			continue
		}
		found++
		if signal.Number() != "2.4r" || signal.Cadence() != 5*time.Minute || signal.ID() != "mimir/contract-horizon-reaper" {
			t.Fatal("the contract horizon reaper signal lost its catalog identity or cadence")
		}
	}
	if found != 1 {
		t.Fatal("the §2.4r contract horizon reaper signal is not registered")
	}
}

// A reaper keeping up, an off reaper that still runs, a pass catching up, a
// dry run whose cursor never moves, a rescan that restarted the pass, and
// errors among runs that make progress are all healthy.
func TestContractHorizonReaperHealthy(t *testing.T) {
	healthy := contractHorizonReaperHealthyFixture()
	for name, values := range map[string]contractHorizonReaperFixture{
		"keeping up": healthy,
		// off: every run stops at its control read, and lag is frozen
		"off":                  healthy.with(contractHorizonReaperFixture{"runs": 12, "progress_runs": 0, "mode": 0}),
		"catching up":          healthy.with(contractHorizonReaperFixture{"lag": 20 * 3600, "lag_before": 40 * 3600}),
		"dry run":              healthy.with(contractHorizonReaperFixture{"mode": 1, "progress_runs": 0, "lag": 30 * 3600, "lag_before": 20 * 3600}),
		"rescan":               healthy.with(contractHorizonReaperFixture{"lag": 200 * 24 * 3600, "pass_start": 1.791e9}),
		"errors with progress": healthy.with(contractHorizonReaperFixture{"error_runs": 5, "control_runs": 4}),
		// the reaper did not run six hours ago
		"no lag before": healthy.with(nil, "lag_before", "pass_start_before"),
		"growth inside": healthy.with(contractHorizonReaperFixture{"lag": 3600 + 2*3600 - 1}),
	} {
		if alerts := runContractHorizonReaperFixture(t, values); len(alerts) != 0 {
			t.Fatalf("%s: a healthy reaper alerted: %s %s", name, alerts[0].Class, alerts[0].Observed)
		}
	}
}

// A chain that stopped running warns, though nothing it exports changed.
func TestContractHorizonReaperStalled(t *testing.T) {
	alerts := runContractHorizonReaperFixture(t, contractHorizonReaperFixture{
		"runs": 0, "error_runs": 0, "control_runs": 0, "progress_runs": 0, "block_points": 0,
	})
	alert := requireAlertClass(t, alerts, "contract-horizon-reaper-stalled")
	if alert.SignalNumber != "2.4r" || alert.SignalKey != "contract-horizon-reaper" || alert.Sustain != 2 {
		t.Fatalf("wrong stalled alert identity: %+v", alert)
	}
	for _, want := range []string{"No contract horizon reaper run ended in 1h", "remove_contracts_past_horizon", "SIGNALS.md §2.4r"} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("stalled alert missing %q:\n%s", want, alert.Markdown())
		}
	}
	if len(alerts) != 1 {
		t.Fatalf("a stalled reaper raised %d alerts: %+v", len(alerts), alerts)
	}
}

// Runs that end in errors, or on an unreadable control, with none reaching
// the horizon or the budget.
func TestContractHorizonReaperFailingAndControlUnavailable(t *testing.T) {
	healthy := contractHorizonReaperHealthyFixture()
	failing := requireAlertClass(t, runContractHorizonReaperFixture(t, healthy.with(contractHorizonReaperFixture{"runs": 12, "error_runs": 12, "progress_runs": 0})),
		"contract-horizon-reaper-failing")
	if failing.Sustain != 2 || !strings.Contains(failing.Markdown(), "12 contract horizon reaper runs failed in 1h") ||
		!strings.Contains(failing.Markdown(), "Never delete contract rows by hand") {
		t.Fatalf("wrong failing alert:\n%s", failing.Markdown())
	}
	if alerts := runContractHorizonReaperFixture(t, healthy.with(contractHorizonReaperFixture{"runs": 12, "error_runs": 2, "progress_runs": 0})); len(alerts) != 0 {
		t.Fatalf("two errors raised: %s", alerts[0].Class)
	}
	control := requireAlertClass(t, runContractHorizonReaperFixture(t, healthy.with(contractHorizonReaperFixture{"runs": 12, "control_runs": 12, "progress_runs": 0})),
		"contract-horizon-control-unavailable")
	if control.Sustain != 2 || !strings.Contains(control.Markdown(), "{contract_horizon}control") {
		t.Fatalf("wrong control alert:\n%s", control.Markdown())
	}
}

// The pass falls behind while on and inside one pass.
func TestContractHorizonReaperBehind(t *testing.T) {
	healthy := contractHorizonReaperHealthyFixture()
	alert := requireAlertClass(t, runContractHorizonReaperFixture(t, healthy.with(contractHorizonReaperFixture{"lag": 30 * 3600, "lag_before": 20 * 3600})),
		"contract-horizon-reaper-behind")
	for _, want := range []string{"fell 10.0 hours further behind its horizon in 6h", "lag_hours=30.0", "lag_before_hours=20.0", "maxRowsPerSecond"} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("behind alert missing %q:\n%s", want, alert.Markdown())
		}
	}
	// exactly the growth allowed raises
	requireAlertClass(t, runContractHorizonReaperFixture(t, healthy.with(contractHorizonReaperFixture{"lag": 3600 + 2*3600})), "contract-horizon-reaper-behind")
}

// Operator block points keep contracts past the horizon.
func TestContractHorizonReaperBlockPointsHolding(t *testing.T) {
	alerts := runContractHorizonReaperFixture(t, contractHorizonReaperHealthyFixture().with(contractHorizonReaperFixture{"block_points": 4}))
	alert := requireAlertClass(t, alerts, "contract-horizon-block-points-holding")
	if alert.Sustain != 1 || !strings.Contains(alert.Markdown(), "block_points_1h=4") || !strings.Contains(alert.Markdown(), "never delete the kept contracts") {
		t.Fatalf("wrong block points alert:\n%s", alert.Markdown())
	}
	if len(alerts) != 1 {
		t.Fatalf("block points raised %d alerts", len(alerts))
	}
}

// Missing, stale or ambiguous parts are unknown, never a measured zero, and a
// positive its own parts prove still raises.
func TestContractHorizonReaperUnobservable(t *testing.T) {
	healthy := contractHorizonReaperHealthyFixture()
	for name, test := range map[string]struct {
		payload string
		reason  string
	}{
		"no runs series":     {payload: contractHorizonReaperFixtureJSON(t, healthy.with(nil, "runs"), 0), reason: "missing_runs"},
		"no block points":    {payload: contractHorizonReaperFixtureJSON(t, healthy.with(nil, "block_points"), 0), reason: "missing_block_points"},
		"ran without a mode": {payload: contractHorizonReaperFixtureJSON(t, healthy.with(nil, "mode"), 0), reason: "missing_mode"},
		"ran without a lag":  {payload: contractHorizonReaperFixtureJSON(t, healthy.with(nil, "lag"), 0), reason: "missing_lag"},
		"stale":              {payload: contractHorizonReaperFixtureJSON(t, healthy, 20*time.Minute), reason: "invalid_or_stale_metric"},
		"duplicate": {payload: contractHorizonReaperFixtureJSON(t, healthy, 0, map[string]any{
			"metric": map[string]string{"monitor_metric": "runs"},
			"value":  []any{float64(contractHorizonReaperFixtureNow.Unix()), "240"},
		}), reason: "duplicate_metric"},
		"unknown class": {payload: contractHorizonReaperFixtureJSON(t, healthy, 0, map[string]any{
			"metric": map[string]string{"monitor_metric": "other"},
			"value":  []any{float64(contractHorizonReaperFixtureNow.Unix()), "1"},
		}), reason: "unknown_metric_class"},
	} {
		alerts := runContractHorizonReaperPayload(t, test.payload)
		alert := requireAlertClass(t, alerts, "contract-horizon-reaper-unobservable")
		if alert.Sustain != 2 || !strings.Contains(alert.Observed, test.reason) {
			t.Fatalf("%s: wrong unobservable alert, want reason %q: %s", name, test.reason, alert.Observed)
		}
	}
	// failing runs are proven by their own counts, whatever else is missing
	alerts := runContractHorizonReaperFixture(t, healthy.with(contractHorizonReaperFixture{"runs": 12, "error_runs": 12, "progress_runs": 0}, "block_points"))
	requireAlertClass(t, alerts, "contract-horizon-reaper-failing")
	requireAlertClass(t, alerts, "contract-horizon-reaper-unobservable")
}
