// Initialized current counters and range history have distinct absence rules.
package monitor

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// Exercise parsing, disabled reduction and the public adapter with recent
// retained pass history, which previously concealed a missing current child.
func TestCapabilityProbeSignalRequiresInitializedCurrentDisabledCounter(t *testing.T) {
	for _, test := range []struct {
		name            string
		current         *float64
		increase        *float64
		wantClass       string
		wantCounterpart string
	}{
		{name: "both absent", wantClass: "cannot-observe"},
		{name: "current absent range zero", increase: new(float64), wantClass: "cannot-observe"},
		{name: "current zero range absent", current: new(float64)},
		{name: "both zero", current: new(float64), increase: new(float64)},
		{name: "current positive range absent", current: capabilityProbeCurrentTestCount(1), wantClass: "capability-probe-disabled-pass", wantCounterpart: "disabled_passes_30m=unknown"},
		{name: "retained positive current absent", increase: capabilityProbeCurrentTestCount(1), wantClass: "capability-probe-disabled-pass", wantCounterpart: "disabled_passes_running_taskworkers=unknown"},
	} {
		values := map[string]float64{
			"last_pass":    capabilityProbeTestUnix(time.Minute),
			"oldest_start": capabilityProbeTestUnix(3 * time.Hour),
			"exporting":    4,
		}
		if test.current != nil {
			values["disabled_running"] = *test.current
		}
		if test.increase != nil {
			values["disabled_increase"] = *test.increase
		}
		payload := capabilityProbeMimirFixture(t, values)
		payload = strings.ReplaceAll(payload, `"metric":{`, `"metric":{"synthetic_private_label":"synthetic-private-sentinel",`)
		observation, err := parseCapabilityProbeObservation(payload, capabilityProbeTestNow, defaultCapabilityProbeSettings())
		if err != nil {
			t.Fatal("synthetic presence fixture failed to parse")
		}
		reduced := capabilityProbeDisabledPassFinding(observation)
		if reduced.healthy != (test.wantClass == "") {
			t.Errorf("%s: missing current supplied an internal healthy sentinel", test.name)
		}
		alerts, _ := runCapabilityProbeSignal(t, capabilityProbeDesired{enabled: true}, nil, payload, nil)
		if test.wantClass == "" {
			if len(alerts) != 0 {
				t.Errorf("%s: narrow measured-zero control alerted", test.name)
			}
			continue
		}
		if len(alerts) != 1 {
			t.Fatalf("%s: want exactly one independent finding", test.name)
		}
		alert := requireAlertClass(t, alerts, test.wantClass)
		if test.wantCounterpart != "" && !strings.Contains(alert.Observed, test.wantCounterpart) {
			t.Errorf("%s: absent counterpart rendered as measured zero", test.name)
		}
		if strings.Contains(alerts.Markdown(), "synthetic-private") {
			t.Errorf("%s: rendered private synthetic label", test.name)
		}
	}
}

// A pointer marks an explicitly present positive fixture value.
func capabilityProbeCurrentTestCount(value float64) *float64 {
	return &value
}

// Missing current cannot hide an independent stall or a retained disabled
// positive. Unsupported age payloads still cannot establish stall health.
func TestCapabilityProbeSignalCurrentAbsenceKeepsIndependentFindings(t *testing.T) {
	for _, test := range []struct {
		name       string
		lastPass   float64
		increase   float64
		wantClass  string
		wantAlerts int
	}{
		{name: "stall plus missing current", lastPass: capabilityProbeTestUnix(time.Hour), wantClass: "capability-probe-stalled", wantAlerts: 2},
		{name: "future age plus missing current", lastPass: capabilityProbeTestUnix(-time.Second), wantClass: "cannot-observe", wantAlerts: 1},
		{name: "future age plus retained positive", lastPass: capabilityProbeTestUnix(-time.Second), increase: 1, wantClass: "capability-probe-disabled-pass", wantAlerts: 2},
	} {
		alerts, _ := runCapabilityProbeSignal(t, capabilityProbeDesired{enabled: true}, nil, capabilityProbeMimirFixture(t, map[string]float64{
			"last_pass":         test.lastPass,
			"oldest_start":      capabilityProbeTestUnix(3 * time.Hour),
			"disabled_increase": test.increase,
		}), nil)
		if len(alerts) != test.wantAlerts {
			t.Fatalf("%s: independent findings changed", test.name)
		}
		requireAlertClass(t, alerts, test.wantClass)
		requireAlertClass(t, alerts, "cannot-observe")
	}
}

// Public signal -> real cadence gate -> real notifier reducer: retained pass
// history with missing current must not clear an existing disabled incident.
func TestCapabilityProbeSignalMissingCurrentCannotClearDisabledIncident(t *testing.T) {
	signal := newCapabilityProbeSignal(func() (capabilityProbeDesired, error) {
		return capabilityProbeDesired{enabled: true}, nil
	}, defaultCapabilityProbeSettings())
	gate := newCadenceAlertGate()
	notifier := &SlackAlertNotifier{
		settings:              DefaultSlackAlertNotifierSettings(),
		keyIncidents:          map[string]*slackIncident{},
		signalKeyCompleteRuns: map[string]int{},
	}
	if notifier.settings.ClearAfterCompleteRuns != 2 {
		t.Fatal("control requires the native complete-run clear policy")
	}
	key := ""
	for run := 0; run < 6; run++ {
		values := map[string]float64{
			"last_pass":         capabilityProbeTestUnix(time.Minute),
			"oldest_start":      capabilityProbeTestUnix(3 * time.Hour),
			"disabled_increase": 0,
		}
		if run == 0 {
			values["disabled_running"] = 1
		} else if run >= 4 {
			values["disabled_running"] = 0
		}
		alerts, _ := runCapabilityProbeSignal(t, capabilityProbeDesired{enabled: true}, nil, capabilityProbeMimirFixture(t, values), nil)
		if run == 0 {
			key = slackIncidentKey(requireAlertClass(t, alerts, "capability-probe-disabled-pass"))
		}
		ready, held := gate.split(signal, alerts)
		notifier.stateLock.Lock()
		transitions := notifier.observeWithLock(capabilityProbeTestNow.Add(time.Duration(run)*time.Minute), signal.Key(), SignalExecution{Alerts: ready, HeldAlerts: held})
		present := notifier.keyIncidents[key] != nil
		notifier.stateLock.Unlock()
		if present != (run < 5) {
			t.Fatalf("run %d: missing current granted recovery or measured-zero recovery failed", run)
		}
		for _, transition := range transitions {
			if transition.kind == slackCleared && slackIncidentKey(transition.alert) == key && run != 5 {
				t.Fatal("disabled incident cleared before two measured-zero observations")
			}
		}
	}
}

// Presence is authority only after existing value, uniqueness and evaluation
// validation; the initialized-child rule must not turn malformed into zero.
func TestCapabilityProbeSignalCurrentCounterValidationRemainsStrict(t *testing.T) {
	for _, test := range []struct {
		name      string
		value     string
		duplicate bool
		stale     bool
	}{
		{name: "negative", value: "-1"},
		{name: "nan", value: "NaN"},
		{name: "infinite", value: "+Inf"},
		{name: "malformed", value: "synthetic-invalid-number"},
		{name: "duplicate zero", value: "0", duplicate: true},
		{name: "stale zero", value: "0", stale: true},
	} {
		evaluation := capabilityProbeTestNow.Unix()
		if test.stale {
			evaluation = capabilityProbeTestNow.Add(-time.Hour).Unix()
		}
		current := fmt.Sprintf(`{"metric":{"monitor_capability_series":"disabled_running"},"value":[%d,%q]}`, evaluation, test.value)
		if test.duplicate {
			current += "," + current
		}
		payload := fmt.Sprintf(`{"status":"success","data":{"resultType":"vector","result":[%s,{"metric":{"monitor_capability_series":"last_pass"},"value":[%d,"%d"]},{"metric":{"monitor_capability_series":"oldest_start"},"value":[%d,"%d"]}]}}`, current, capabilityProbeTestNow.Unix(), capabilityProbeTestNow.Add(-time.Minute).Unix(), capabilityProbeTestNow.Unix(), capabilityProbeTestNow.Add(-3*time.Hour).Unix())
		alerts, _ := runCapabilityProbeSignal(t, capabilityProbeDesired{enabled: true}, nil, payload, nil)
		if len(alerts) != 1 || alerts[0].Class != "cannot-observe" {
			t.Errorf("%s: invalid current counter granted health", test.name)
		}
	}
}
