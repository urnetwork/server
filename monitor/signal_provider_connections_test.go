package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// The registered signal for the connected-provider gauge. Before §1.1a
// nothing read urnetwork_connect_provider_connections, so the 2026-10-19 fall
// from 76k to 1.8k connected providers paged no one.
func providerConnectionsSignalForTest(t *testing.T) Signal {
	t.Helper()
	for _, signal := range NewSignals() {
		if signal.Key() == "provider-connections" {
			return signal
		}
	}
	t.Fatal("no registered signal reads urnetwork_connect_provider_connections; a collapse of connected providers pages no one")
	return nil
}

// The tagged scalars of one evaluation. A missing key is absent from the
// vector.
type providerConnectionsFixture map[string]float64

func providerConnectionsSteady(current, best, level float64) providerConnectionsFixture {
	return providerConnectionsFixture{
		"current": current, "best_5m": best, "p90_24h": level, "points_24h": 288,
		"processes": 22, "max_processes_24h": 22,
	}
}

// A Mimir vector for the probe's query, evaluated at now. The newest sample is
// sampleAge before now.
func providerConnectionsFixtureJson(t testing.TB, now time.Time, sampleAge time.Duration, fixture providerConnectionsFixture) string {
	t.Helper()
	results := []map[string]any{}
	values := map[string]float64{}
	for metric, value := range fixture {
		values[metric] = value
	}
	if _, ok := values["sample_time"]; !ok && 0 < len(fixture) {
		values["sample_time"] = float64(now.Add(-sampleAge).Add(-9*time.Second).UnixMilli()) / 1000
	}
	for metric, value := range values {
		results = append(results, map[string]any{
			"metric": map[string]string{"monitor_metric": metric},
			"value":  []any{float64(now.Unix()), fmt.Sprintf("%g", value)},
		})
	}
	payload, err := json.Marshal(map[string]any{
		"status": "success",
		"data":   map[string]any{"resultType": "vector", "result": results},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(payload)
}

// Runs the signal over a Mimir payload, checking the query it sends.
func runProviderConnectionsPayload(t *testing.T, now time.Time, payload string) Alerts {
	t.Helper()
	signal := providerConnectionsSignalForTest(t)
	source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		if host.Name != "metrics-1" {
			return "", fmt.Errorf("provider connections queried %s, not the services host", host.Name)
		}
		for _, want := range []string{
			"urnetwork_connect_provider_connections",
			"env%3D%22synthetic%22",
			"%5B5m%3A1m%5D",
			"%5B24h%3A5m%5D",
			"quantile_over_time%280.9%2C",
			"count_over_time",
			"timestamp%28urnetwork_connect_provider_connections",
			"count+by+%28host%2Cblock%2Cinstance%29",
			"monitor_metric",
		} {
			if !strings.Contains(command, want) {
				return "", fmt.Errorf("provider connections query missing %q: %s", want, command)
			}
		}
		return payload, nil
	}}
	settings := syntheticSettings(source)
	settings.Now = func() time.Time { return now }
	settings.Hosts = append(settings.Hosts, HostSettings{Name: "metrics-1", Roles: []string{"services"}})
	alerts, err := signal.Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	return alerts
}

func runProviderConnectionsFixture(t *testing.T, fixture providerConnectionsFixture) Alerts {
	t.Helper()
	now := time.Date(2026, 10, 19, 11, 50, 0, 0, time.UTC)
	return runProviderConnectionsPayload(t, now, providerConnectionsFixtureJson(t, now, 0, fixture))
}

// A steady fleet is quiet.
func TestProviderConnectionsSignalSteadyFleetIsQuiet(t *testing.T) {
	if alerts := runProviderConnectionsFixture(t, providerConnectionsSteady(75100, 75400, 77292)); len(alerts) != 0 {
		t.Fatalf("steady fleet alerted:\n%s", alerts[0].Markdown())
	}
}

// The 2026-10-19 collapse at 11:46Z: 1,764 providers, 2,131 at best in the
// last five minutes, against a 24 h level of 76,473, from 6 of the usual 22
// Connect processes.
func TestProviderConnectionsSignalPagesOnTheOctober19Collapse(t *testing.T) {
	fixture := providerConnectionsSteady(1764, 2131, 76473)
	fixture["processes"] = 6
	alert := requireAlertClass(t, runProviderConnectionsFixture(t, fixture), "provider-connections-collapse")
	if alert.SignalNumber != "1.1a" || alert.SignalKey != "provider-connections" || alert.Severity != SeverityPage ||
		alert.Sustain != 1 || alert.Target != "synthetic/connect-fleet" {
		t.Fatalf("wrong collapse identity: %+v", alert)
	}
	for _, want := range []string{
		"Connected providers fell to 1764; the best minute of the last five was 2131, 3% of the 24 h level 76473 (90th percentile)",
		"connected_providers=1764 best_5m=2131 p90_24h=76473 best_5m_of_p90=2.8% reporting_processes=6 max_processes_24h=22",
		"metrics_gateway=metrics-1",
		"Deploy fail ... not ready",
		"SIGNALS.md §1.1a and §5.1",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("collapse page missing %q:\n%s", want, alert.Markdown())
		}
	}
}

// A planned restart of every Connect block drops the sum, but providers come
// back inside the five minute window: the best minute stays above the line.
func TestProviderConnectionsSignalRestartInsideTheWindowDoesNotPage(t *testing.T) {
	if alerts := runProviderConnectionsFixture(t, providerConnectionsSteady(10400, 74100, 76000)); len(alerts) != 0 {
		t.Fatalf("restart dip paged:\n%s", alerts[0].Markdown())
	}
}

// The week before held a day at 55k against a 74k level: a partial drop
// above half the level is not this page.
func TestProviderConnectionsSignalPartialDropAboveHalfDoesNotPage(t *testing.T) {
	if alerts := runProviderConnectionsFixture(t, providerConnectionsSteady(55401, 56100, 74419)); len(alerts) != 0 {
		t.Fatalf("partial drop paged:\n%s", alerts[0].Markdown())
	}
}

// The line is exactly half the 24 h level.
func TestProviderConnectionsSignalCollapseBoundary(t *testing.T) {
	for _, c := range []struct {
		best  float64
		pages bool
	}{
		{best: 50000, pages: false},
		{best: 49999, pages: true},
	} {
		alerts := runProviderConnectionsFixture(t, providerConnectionsSteady(c.best, c.best, 100000))
		if pages := requireAlertClassCount(alerts, "provider-connections-collapse") == 1; pages != c.pages {
			t.Errorf("best_5m=%g of a 100000 level: pages=%v, want %v", c.best, pages, c.pages)
		}
	}
}

// An environment whose 24 h level is below 1,000 providers is too small to
// judge.
func TestProviderConnectionsSignalSmallEnvironmentIsNotJudged(t *testing.T) {
	if alerts := runProviderConnectionsFixture(t, providerConnectionsSteady(3, 10, 999)); len(alerts) != 0 {
		t.Fatalf("small environment paged:\n%s", alerts[0].Markdown())
	}
}

// No gauge, a stale gauge, too little history and malformed frames are
// unknown, never healthy and never a measured collapse.
func TestProviderConnectionsSignalUnreadableFramesAreUnobservable(t *testing.T) {
	now := time.Date(2026, 10, 19, 11, 50, 0, 0, time.UTC)
	short := providerConnectionsSteady(1764, 2131, 76473)
	short["points_24h"] = 11
	incomplete := providerConnectionsSteady(1764, 2131, 76473)
	delete(incomplete, "best_5m")
	unknown := providerConnectionsSteady(1764, 2131, 76473)
	unknown["window_min"] = 1
	negative := providerConnectionsSteady(1764, 2131, 76473)
	negative["current"] = -1
	for _, c := range []struct {
		name      string
		payload   string
		reason    string
		duplicate bool
	}{
		{name: "missing", payload: providerConnectionsFixtureJson(t, now, 0, nil), reason: "metric_missing"},
		{name: "stale", payload: providerConnectionsFixtureJson(t, now, 10*time.Minute, providerConnectionsSteady(1764, 2131, 76473)), reason: "stale_series"},
		{name: "short history", payload: providerConnectionsFixtureJson(t, now, 0, short), reason: "baseline_insufficient"},
		{name: "incomplete", payload: providerConnectionsFixtureJson(t, now, 0, incomplete), reason: "incomplete_frame"},
		{name: "unknown tag", payload: providerConnectionsFixtureJson(t, now, 0, unknown), reason: "unknown_metric_class"},
		{name: "negative", payload: providerConnectionsFixtureJson(t, now, 0, negative), reason: "invalid_metric"},
		{name: "duplicate", reason: "duplicate_metric", duplicate: true},
	} {
		payload := c.payload
		if c.duplicate {
			var decoded map[string]any
			if err := json.Unmarshal([]byte(providerConnectionsFixtureJson(t, now, 0, providerConnectionsSteady(1764, 2131, 76473))), &decoded); err != nil {
				t.Fatal(err)
			}
			data := decoded["data"].(map[string]any)
			result := data["result"].([]any)
			data["result"] = append(result, result[0])
			encoded, err := json.Marshal(decoded)
			if err != nil {
				t.Fatal(err)
			}
			payload = string(encoded)
		}
		alerts := runProviderConnectionsPayload(t, now, payload)
		if count := requireAlertClassCount(alerts, "provider-connections-collapse"); count != 0 {
			t.Errorf("%s: unreadable frame paged a collapse", c.name)
		}
		alert := requireAlertClass(t, alerts, "provider-connections-unobservable")
		if !alert.unobservable || alert.Severity != SeverityWarn || alert.Sustain != 2 ||
			!strings.Contains(alert.Markdown(), "visibility_reason="+c.reason) {
			t.Errorf("%s: not an unknown warning with reason %s: %+v", c.name, c.reason, alert)
		}
	}
}
