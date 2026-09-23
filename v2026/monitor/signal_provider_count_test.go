package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestProviderCountSignalSyntheticEffectiveEmpty(t *testing.T) {
	now := time.Date(2026, 9, 22, 19, 30, 0, 0, time.UTC)
	alerts, err := NewProviderCountSignal().Run(context.Background(), providerCountSyntheticSettings(t, now, providerCountFixture(t, now, map[string]float64{"0": 16, "3-9": 4}, "false")))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "provider-count-effective-empty")
	if alert.Severity != SeverityPage {
		t.Fatalf("severity=%s, want page", alert.Severity)
	}
	for _, want := range []string{"effectively empty", "zero=80.0%", "completed response boundary", "no client, network, provider, location ID", "SIGNALS.md §2.9a"} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("alert missing %q:\n%s", want, alert.Markdown())
		}
	}
}

func TestProviderCountSignalSyntheticSmallListAndForceMinimumControl(t *testing.T) {
	now := time.Date(2026, 9, 22, 19, 30, 0, 0, time.UTC)
	alerts, err := NewProviderCountSignal().Run(context.Background(), providerCountSyntheticSettings(t, now, providerCountFixture(t, now, map[string]float64{"0": 25, "1-2": 10, "3-9": 15}, "false")))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "provider-count-small-list")
	if alert.Severity != SeverityWarn {
		t.Fatalf("severity=%s, want warn", alert.Severity)
	}

	alerts, err = NewProviderCountSignal().Run(context.Background(), providerCountSyntheticSettings(t, now, providerCountFixture(t, now, map[string]float64{"0": 100}, "true")))
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 0 {
		t.Fatalf("ForceMinimum diagnostic cohort alerted: %+v", alerts)
	}
}

func TestProviderCountSignalSyntheticMissingTelemetryIsVisible(t *testing.T) {
	now := time.Date(2026, 9, 22, 19, 30, 0, 0, time.UTC)
	_, err := NewProviderCountSignal().Run(context.Background(), providerCountSyntheticSettings(t, now, providerCountFixture(t, now, nil, "false")))
	if err == nil || !strings.Contains(err.Error(), "outcome metric is absent") {
		t.Fatalf("missing telemetry error = %v, want explicit visibility loss", err)
	}
}

func providerCountSyntheticSettings(t testing.TB, now time.Time, payload string) SignalSettings {
	t.Helper()
	source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		if host.Name != "metrics-1" || !strings.Contains(command, "urnetwork_findproviders2_outcomes_total") || !strings.Contains(command, "%5B5m%5D") || !strings.Contains(command, "%22synthetic%22") {
			return "", fmt.Errorf("unexpected provider-count Mimir command on %s", host.Name)
		}
		return payload, nil
	}}
	settings := syntheticSettings(source)
	settings.Environment = "synthetic"
	settings.Now = func() time.Time { return now }
	settings.Hosts = append(settings.Hosts, HostSettings{Name: "metrics-1", Roles: []string{"services"}})
	return settings
}

func providerCountFixture(t testing.TB, now time.Time, bands map[string]float64, forceMinimum string) string {
	t.Helper()
	result := []map[string]any{}
	for _, band := range []string{"0", "1-2", "3-9", "10+"} {
		value, ok := bands[band]
		if !ok {
			continue
		}
		result = append(result, map[string]any{
			"metric": map[string]string{"ip_family": "any", "location_kind": "location", "caller_country": "us", "rank_mode": "quality", "force_minimum": forceMinimum, "result_count": band},
			"value":  []any{float64(now.Unix()), fmt.Sprintf("%.3f", value)},
		})
	}
	payload, err := json.Marshal(map[string]any{"status": "success", "data": map[string]any{"resultType": "vector", "result": result}})
	if err != nil {
		t.Fatal(err)
	}
	return string(payload)
}
