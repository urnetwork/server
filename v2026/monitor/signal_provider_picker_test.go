// Synthetic endpoint, process-clock and privacy controls for SIGNALS.md §2.9b.
package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strings"
	"testing"
	"time"
)

func pickerTestNow() time.Time { return time.Date(2026, 9, 25, 8, 0, 0, 0, time.UTC) }

// Eleven finite children, two exact source-clock bounds, and range witnesses.
func pickerTestRows(deltas map[string]float64) []map[string]any {
	now := pickerTestNow()
	rows := []map[string]any{}
	add := func(key string, value float64) {
		rows = append(rows, map[string]any{"metric": map[string]string{"env": "synthetic", "job": "api", "host": "api-synthetic", "block": "blue", "instance": "private-synthetic-instance", "monitor_picker": key}, "value": []any{now.Unix(), fmt.Sprint(value)}})
	}
	add("presence", 20)
	for _, field := range append([]string{"start"}, pickerFields()...) {
		if field != "start" {
			add("resets/"+field, 0)
		}
		for _, bound := range []string{"now", "prior"} {
			value := 100.0
			stamp := now.Add(-5 * time.Second)
			if bound == "now" {
				value += deltas[field]
			} else {
				stamp = stamp.Add(-providerPickerWindow)
			}
			if field == "start" {
				value = float64(now.Add(-time.Hour).Unix())
			}
			add(bound+"/"+field, value)
			add(bound+"/"+field+"/time", float64(stamp.Unix()))
		}
	}
	return rows
}

func pickerTestPayload(t testing.TB, rows []map[string]any) string {
	t.Helper()
	data, err := json.Marshal(map[string]any{"status": "success", "data": map[string]any{"resultType": "vector", "result": rows}})
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func pickerTestSettings(t testing.TB, raw string) SignalSettings {
	t.Helper()
	source := &syntheticSource{hostFn: func(h HostSettings, command string) (string, error) {
		if h.Name != "api-synthetic" || !strings.Contains(command, "--max-time 15 --max-filesize 4194304") || !strings.Contains(command, "urnetwork_provider_picker_outcomes_total") || !strings.Contains(command, "offset 5m") || !strings.Contains(command, "timestamp(") || !strings.Contains(command, "resets(") {
			return "", fmt.Errorf("unexpected bounded picker query")
		}
		return raw, nil
	}}
	settings := syntheticSettings(source)
	settings.Environment = "synthetic"
	settings.Now = pickerTestNow
	settings.Hosts = []HostSettings{{Name: "api-synthetic", Roles: []string{"services"}}}
	settings.LogServices = []string{"api"}
	settings.LogServiceHosts = map[string][]string{"api": {"api-synthetic"}}
	settings.LogServiceBlocks = map[string][]string{"api": {"blue"}}
	return settings
}

func runPickerFixture(t testing.TB, rows []map[string]any) Alerts {
	t.Helper()
	alerts, err := NewProviderPickerSignal().Run(context.Background(), pickerTestSettings(t, pickerTestPayload(t, rows)))
	if err != nil {
		t.Fatal(err)
	}
	return alerts
}

func pickerChange(rows []map[string]any, key string, value string) {
	for _, row := range rows {
		if row["metric"].(map[string]string)["monitor_picker"] == key {
			row["value"].([]any)[1] = value
		}
	}
}

func TestProviderPickerSignalEffectiveEmpty(t *testing.T) {
	alerts := runPickerFixture(t, pickerTestRows(map[string]float64{"initial/empty": 20}))
	a := requireAlertClass(t, alerts, "provider-picker-effective-empty")
	if a.Severity != SeverityPage {
		t.Fatal("empty initial list did not page")
	}
	for _, word := range []string{"initial_empty=20", "countries, promoted groups and devices", "not FindProviders2", "§2.9b", "observed-process-subset"} {
		if !strings.Contains(a.Markdown(), word) {
			t.Errorf("missing qualification %q", word)
		}
	}
	for _, secret := range []string{"private-synthetic-instance", "api-synthetic", "redis://", "token="} {
		if strings.Contains(a.Markdown(), secret) {
			t.Fatal("private source context escaped into Markdown")
		}
	}
}

func TestProviderPickerSignalNonemptyHealthy(t *testing.T) {
	if alerts := runPickerFixture(t, pickerTestRows(map[string]float64{"initial/nonempty": 20})); len(alerts) != 0 {
		t.Fatalf("healthy picker emitted %d alerts", len(alerts))
	}
}

func TestProviderPickerSignalLegitimateSearchEmpty(t *testing.T) {
	alerts := runPickerFixture(t, pickerTestRows(map[string]float64{"search/empty": 100}))
	a := requireAlertClass(t, alerts, "provider-picker-unobservable")
	if len(alerts) != 1 || !strings.Contains(a.Observed, "initial-traffic-below-health-floor") {
		t.Fatal("search miss became supply failure or initial health")
	}
}

func TestProviderPickerSignalSearchEmptyDoesNotSpoilInitialControl(t *testing.T) {
	if alerts := runPickerFixture(t, pickerTestRows(map[string]float64{"initial/nonempty": 20, "search/empty": 100, "direct/empty": 100})); len(alerts) != 0 {
		t.Fatal("legitimate noninitial misses acquired a supply verdict")
	}
}

func TestProviderPickerSignalReadErrorVisible(t *testing.T) {
	alerts := runPickerFixture(t, pickerTestRows(map[string]float64{"initial/nonempty": 20, "read/filters": 1}))
	a := requireAlertClass(t, alerts, "provider-picker-read-or-request-error")
	if a.Severity != SeverityWarn || !strings.Contains(a.Observed, "read_filters=1") {
		t.Fatal("read error became health or empty result")
	}
}

func TestProviderPickerSignalRequestErrorPage(t *testing.T) {
	a := requireAlertClass(t, runPickerFixture(t, pickerTestRows(map[string]float64{"initial/error": 20})), "provider-picker-read-or-request-error")
	if a.Severity != SeverityPage {
		t.Fatal("repeated picker errors did not page")
	}
}

func TestProviderPickerSignalPartialCoverageKeepsPositive(t *testing.T) {
	rows := pickerTestRows(map[string]float64{"initial/empty": 20})
	settings := pickerTestSettings(t, pickerTestPayload(t, rows))
	settings.LogServiceBlocks["api"] = []string{"blue", "green"}
	alerts, err := NewProviderPickerSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	requireAlertClass(t, alerts, "provider-picker-effective-empty")
	requireAlertClass(t, alerts, "provider-picker-unobservable")
}

func TestProviderPickerSignalMissingProducer(t *testing.T) {
	requireAlertClass(t, runPickerFixture(t, nil), "provider-picker-unobservable")
}

func TestProviderPickerSignalStaleClockIsUnknown(t *testing.T) {
	rows := pickerTestRows(map[string]float64{"initial/nonempty": 20})
	pickerChange(rows, "now/initial/nonempty/time", fmt.Sprint(pickerTestNow().Add(-3*time.Minute).Unix()))
	requireAlertClass(t, runPickerFixture(t, rows), "provider-picker-unobservable")
}

func TestProviderPickerSignalChangedProcessIsUnknown(t *testing.T) {
	rows := pickerTestRows(map[string]float64{"initial/nonempty": 20})
	pickerChange(rows, "now/start", fmt.Sprint(pickerTestNow().Add(-time.Minute).Unix()))
	requireAlertClass(t, runPickerFixture(t, rows), "provider-picker-unobservable")
}

func TestProviderPickerSignalRangeGenerationPreventsRecovery(t *testing.T) {
	rows := pickerTestRows(map[string]float64{"initial/nonempty": 20})
	rows = append(rows, map[string]any{"metric": map[string]string{"env": "synthetic", "job": "api", "host": "api-synthetic", "block": "blue", "instance": "short-lived-synthetic", "monitor_picker": "presence"}, "value": []any{pickerTestNow().Unix(), "1"}})
	requireAlertClass(t, runPickerFixture(t, rows), "provider-picker-unobservable")
}

func TestProviderPickerSignalCounterResetIsUnknown(t *testing.T) {
	rows := pickerTestRows(map[string]float64{"initial/nonempty": 20})
	pickerChange(rows, "resets/initial/nonempty", "1")
	requireAlertClass(t, runPickerFixture(t, rows), "provider-picker-unobservable")
}

func TestProviderPickerSignalMalformedSourceIsUnknown(t *testing.T) {
	for _, value := range []string{"NaN", "+Inf", "-1", "1e308", fmt.Sprint(math.Inf(-1))} {
		rows := pickerTestRows(map[string]float64{"initial/nonempty": 20})
		pickerChange(rows, "now/initial/nonempty", value)
		a := requireAlertClass(t, runPickerFixture(t, rows), "provider-picker-unobservable")
		if !strings.Contains(a.Observed, "invalid-source-response") {
			t.Fatal("malformed value was accepted")
		}
	}
}

func TestProviderPickerSignalDuplicateIsUnknown(t *testing.T) {
	rows := pickerTestRows(map[string]float64{"initial/nonempty": 20})
	rows = append(rows, rows[0])
	requireAlertClass(t, runPickerFixture(t, rows), "provider-picker-unobservable")
}

func TestProviderPickerSignalResponseBound(t *testing.T) {
	settings := pickerTestSettings(t, strings.Repeat("x", providerPickerMaxBytes+1))
	alerts, err := NewProviderPickerSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	requireAlertClass(t, alerts, "provider-picker-unobservable")
}

func TestProviderPickerSignalCancellation(t *testing.T) {
	settings := pickerTestSettings(t, "")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := NewProviderPickerSignal().Run(ctx, settings)
	if err == nil {
		t.Fatal("canceled collection became healthy")
	}
}

func TestProviderPickerSignalCatalogAndRegistration(t *testing.T) {
	signals, err := IncludeSignals(NewSignals(), "provider-picker")
	if err != nil || len(signals) != 1 || signals[0].Number() != "2.9b" {
		t.Fatal("picker signal missing from registry")
	}
	catalog, err := os.ReadFile("SIGNALS.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"Probe: `provider-picker`", "/network/provider-locations", "/network/find-provider-locations", "legitimate empty search", "rendered", "process"} {
		if !strings.Contains(string(catalog), text) {
			t.Errorf("catalog missing %q", text)
		}
	}
}

func pickerFindingHealthy(findings []finding, class string) bool {
	for _, f := range findings {
		if f.class == class && f.healthy {
			return true
		}
	}
	return false
}

func TestProviderPickerSignalEmptyRecoveryRetainsReadWarning(t *testing.T) {
	findings := providerPickerFindings(pickerEvidence{complete: true, paired: 1, expected: 1, deltas: map[string]float64{"initial/nonempty": 20, "read/filters": 1}})
	if !pickerFindingHealthy(findings, "provider-picker-effective-empty") || !pickerFindingHealthy(findings, "provider-picker-unobservable") || pickerFindingHealthy(findings, "provider-picker-read-or-request-error") {
		t.Fatal("read WARN prevented independent empty/visibility recovery or falsely cleared itself")
	}
}

func TestProviderPickerSignalReadRecoveryRetainsEmptyPage(t *testing.T) {
	findings := providerPickerFindings(pickerEvidence{complete: true, paired: 1, expected: 1, deltas: map[string]float64{"initial/empty": 20}})
	if pickerFindingHealthy(findings, "provider-picker-effective-empty") || !pickerFindingHealthy(findings, "provider-picker-read-or-request-error") || !pickerFindingHealthy(findings, "provider-picker-unobservable") {
		t.Fatal("empty PAGE prevented independent error/visibility recovery or falsely cleared itself")
	}
}

func TestProviderPickerSignalPartialCannotClearPriorPage(t *testing.T) {
	for _, e := range []pickerEvidence{
		{complete: false, paired: 1, expected: 2, deltas: map[string]float64{"initial/nonempty": 20}},
		{complete: true, paired: 1, expected: 1, deltas: map[string]float64{"initial/nonempty": 19}},
	} {
		for _, f := range providerPickerFindings(e) {
			if f.healthy {
				t.Fatal("partial or below-floor evidence cleared an independent prior class")
			}
		}
	}
}
