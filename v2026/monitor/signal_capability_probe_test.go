package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

var capabilityProbeTestNow = time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)

// A Mimir response holding the named fleet series, evaluated at the
// synthetic clock; a series left out is absent, as an aggregate over no
// series is.
func capabilityProbeMimirFixture(t *testing.T, values map[string]float64) string {
	t.Helper()
	results := []any{}
	for name, value := range values {
		results = append(results, map[string]any{
			"metric": map[string]string{"monitor_capability_series": name},
			"value":  []any{capabilityProbeTestNow.Unix(), strconv.FormatFloat(value, 'f', -1, 64)},
		})
	}
	body, err := json.Marshal(map[string]any{"status": "success", "data": map[string]any{"resultType": "vector", "result": results}})
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// Runs the signal against a desired state and a Mimir response, and returns
// its alerts and the PromQL it sent.
func runCapabilityProbeSignal(t *testing.T, desired capabilityProbeDesired, desiredErr error, mimirResponse string, mimirErr error) (Alerts, string) {
	t.Helper()
	var observedPromql string
	source := &syntheticSource{
		hostFn: func(host HostSettings, command string) (string, error) {
			if host.Name != "metrics-1" {
				return "", fmt.Errorf("unexpected Mimir host %s", host.Name)
			}
			start := strings.Index(command, "query=")
			if start < 0 {
				return "", errors.New("Mimir command omitted query")
			}
			query, err := url.QueryUnescape(strings.TrimSuffix(command[start+len("query="):], "'"))
			if err != nil {
				return "", err
			}
			observedPromql = query
			return mimirResponse, mimirErr
		},
	}
	settings := syntheticSettings(source)
	settings.Hosts = append(settings.Hosts, HostSettings{Name: "metrics-1", Roles: []string{"services"}})
	alerts, err := newCapabilityProbeSignal(func() (capabilityProbeDesired, error) { return desired, desiredErr }).Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	return alerts, observedPromql
}

// Seconds since the epoch of the synthetic clock less ago.
func capabilityProbeTestUnix(ago time.Duration) float64 {
	return float64(capabilityProbeTestNow.Add(-ago).Unix())
}

// While the config switches the probe on, a chain that passes recently with
// no disabled pass is healthy, and the query reads only fleet aggregates of
// the probe's series and the taskworkers' starts.
func TestCapabilityProbeSignalHealthyWhilePassing(t *testing.T) {
	alerts, promql := runCapabilityProbeSignal(t, capabilityProbeDesired{enabled: true, gateOn: true}, nil, capabilityProbeMimirFixture(t, map[string]float64{
		"disabled_increase": 0,
		"disabled_running":  0,
		"last_pass":         capabilityProbeTestUnix(2 * time.Minute),
		"exporting":         4,
		"oldest_start":      capabilityProbeTestUnix(3 * time.Hour),
	}), nil)
	if len(alerts) != 0 {
		t.Fatalf("a passing chain alerted: %+v", alerts)
	}
	for _, required := range []string{
		`sum(increase(urnetwork_capability_probe_passes_total{env="synthetic",job="taskworker",result="disabled"}[30m]))`,
		`sum(urnetwork_capability_probe_passes_total{env="synthetic",job="taskworker",result="disabled"})`,
		`max(max_over_time(urnetwork_capability_probe_last_pass_timestamp_seconds{env="synthetic",job="taskworker"}[6h]))`,
		`count(urnetwork_capability_probe_last_pass_timestamp_seconds{env="synthetic",job="taskworker"})`,
		`min(process_start_time_seconds{env="synthetic",job="taskworker"})`,
	} {
		if !strings.Contains(promql, required) {
			t.Errorf("the query lacks %s: %s", required, promql)
		}
	}
}

// Re-review finding 2: a taskworker that kept a config switching the probe
// off ended the probe's chain at the first pass it claimed, and nothing
// showed it. Any disabled pass while the config switches the probe on is a
// finding: one counted in the window, and one a running taskworker ran
// before its first scrape, which only its running total shows.
func TestCapabilityProbeSignalAlertsOnADisabledPassWhileEnabled(t *testing.T) {
	healthy := map[string]float64{
		"last_pass":    capabilityProbeTestUnix(time.Minute),
		"exporting":    4,
		"oldest_start": capabilityProbeTestUnix(3 * time.Hour),
	}
	for _, c := range []struct {
		name             string
		disabledIncrease float64
		disabledRunning  float64
	}{
		{name: "in the window", disabledIncrease: 1},
		{name: "before the first scrape", disabledRunning: 1},
		{name: "by a process that exited since", disabledIncrease: 1.04},
	} {
		values := map[string]float64{"disabled_increase": c.disabledIncrease, "disabled_running": c.disabledRunning}
		for name, value := range healthy {
			values[name] = value
		}
		alerts, _ := runCapabilityProbeSignal(t, capabilityProbeDesired{enabled: true}, nil, capabilityProbeMimirFixture(t, values), nil)
		if len(alerts) != 1 {
			t.Fatalf("%s: alerts %+v, want only the disabled pass", c.name, alerts)
		}
		alert := requireAlertClass(t, alerts, "capability-probe-disabled-pass")
		if alert.Severity != Severity(tierWarn) || alert.Target != capabilityProbeTarget {
			t.Fatalf("%s: alert %+v", c.name, alert)
		}
	}

	// switched off, a disabled pass ends the chain as it should
	alerts, _ := runCapabilityProbeSignal(t, capabilityProbeDesired{}, nil, capabilityProbeMimirFixture(t, map[string]float64{"disabled_increase": 1, "disabled_running": 1}), nil)
	if len(alerts) != 0 {
		t.Fatalf("a switched-off probe's disabled pass alerted: %+v", alerts)
	}
}

// No measured or waiting pass for 30 minutes while the config switches the
// probe on is a stall, a page past two hours with the gate on. A fleet that
// restarted since its last pass, as a switch-on restarts it, has 30 minutes
// from its oldest process's start; a probe whose series no running
// taskworker exports is stalled too.
func TestCapabilityProbeSignalAlertsOnAStalledChain(t *testing.T) {
	for _, c := range []struct {
		name        string
		gateOn      bool
		lastPass    time.Duration
		exporting   float64
		oldestStart time.Duration
		// "" for healthy
		tier string
	}{
		{name: "passed recently", lastPass: 10 * time.Minute, exporting: 4, oldestStart: 3 * time.Hour},
		{name: "no pass for 45 minutes", lastPass: 45 * time.Minute, exporting: 4, oldestStart: 3 * time.Hour, tier: tierWarn},
		{name: "no pass for three hours, gate off", lastPass: 3 * time.Hour, exporting: 4, oldestStart: 5 * time.Hour, tier: tierWarn},
		{name: "no pass for three hours, gate on", gateOn: true, lastPass: 3 * time.Hour, exporting: 4, oldestStart: 5 * time.Hour, tier: tierPage},
		{name: "switched on ten minutes ago", gateOn: true, exporting: 4, oldestStart: 10 * time.Minute},
		{name: "switched on 40 minutes ago, no pass", exporting: 4, oldestStart: 40 * time.Minute, tier: tierWarn},
		{name: "restarted after an old pass", lastPass: 4 * time.Hour, exporting: 4, oldestStart: 20 * time.Minute},
		{name: "no taskworker has the probe", oldestStart: 3 * time.Hour, tier: tierWarn},
	} {
		values := map[string]float64{
			"disabled_increase": 0,
			"disabled_running":  0,
			"oldest_start":      capabilityProbeTestUnix(c.oldestStart),
		}
		if 0 < c.lastPass {
			values["last_pass"] = capabilityProbeTestUnix(c.lastPass)
		}
		if 0 < c.exporting {
			values["exporting"] = c.exporting
		}
		alerts, _ := runCapabilityProbeSignal(t, capabilityProbeDesired{enabled: true, gateOn: c.gateOn}, nil, capabilityProbeMimirFixture(t, values), nil)
		if c.tier == "" {
			if len(alerts) != 0 {
				t.Errorf("%s: alerts %+v, want none", c.name, alerts)
			}
			continue
		}
		if len(alerts) != 1 {
			t.Errorf("%s: alerts %+v, want only the stall", c.name, alerts)
			continue
		}
		alert := requireAlertClass(t, alerts, "capability-probe-stalled")
		if alert.Severity != Severity(c.tier) {
			t.Errorf("%s: severity %s, want %s", c.name, alert.Severity, c.tier)
		}
	}

	// switched off, a probe that never passes is silent
	alerts, _ := runCapabilityProbeSignal(t, capabilityProbeDesired{}, nil, capabilityProbeMimirFixture(t, map[string]float64{"oldest_start": capabilityProbeTestUnix(5 * time.Hour)}), nil)
	if len(alerts) != 0 {
		t.Fatalf("a switched-off probe alerted: %+v", alerts)
	}
}

// What cannot be read is reported as such, never as a healthy or broken
// chain: the desired config, Mimir, a malformed or skewed response, and a
// fleet without taskworker series.
func TestCapabilityProbeSignalReportsWhatItCannotObserve(t *testing.T) {
	healthy := capabilityProbeMimirFixture(t, map[string]float64{
		"last_pass":    capabilityProbeTestUnix(time.Minute),
		"exporting":    4,
		"oldest_start": capabilityProbeTestUnix(3 * time.Hour),
	})
	skewed, err := json.Marshal(map[string]any{"status": "success", "data": map[string]any{"resultType": "vector", "result": []any{
		map[string]any{"metric": map[string]string{"monitor_capability_series": "oldest_start"}, "value": []any{capabilityProbeTestNow.Add(-time.Hour).Unix(), "1"}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name       string
		desiredErr error
		response   string
		mimirErr   error
	}{
		{name: "desired config", desiredErr: errors.New("capability_probe.yml does not parse"), response: healthy},
		{name: "Mimir", mimirErr: errors.New("synthetic gateway failure")},
		{name: "malformed response", response: `{"status":"success","data":{"resultType":"vector","result":[{"metric":{"monitor_capability_series":"unknown"},"value":[0,"1"]}]}}`},
		{name: "skewed response", response: string(skewed)},
		{name: "no taskworker series", response: capabilityProbeMimirFixture(t, map[string]float64{})},
	} {
		alerts, _ := runCapabilityProbeSignal(t, capabilityProbeDesired{enabled: true}, c.desiredErr, c.response, c.mimirErr)
		if len(alerts) != 1 || alerts[0].Class != "cannot-observe" {
			t.Errorf("%s: alerts %+v, want one cannot-observe", c.name, alerts)
		}
	}
}
