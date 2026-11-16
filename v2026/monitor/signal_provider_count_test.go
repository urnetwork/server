package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
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

// Candidate selection and the app's location picker must never certify each other.
func TestProviderCountSignalDistinguishesPickerAndTargetIntent(t *testing.T) {
	now := time.Date(2026, 9, 25, 8, 0, 0, 0, time.UTC)
	alerts, err := NewProviderCountSignal().Run(context.Background(), providerCountSyntheticSettings(t, now, providerCountFixture(t, now, map[string]float64{"0": 20}, "false")))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "provider-count-effective-empty")
	for _, text := range []string{"connection-selection", "provider-picker (§2.9b)", "cannot prove picker health", "caller country is not selected target country", "ForceCount/requested Count"} {
		if !strings.Contains(alert.Markdown(), text) {
			t.Errorf("missing app-list distinction %q", text)
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

// The online rank mode is bounded vocabulary like the others: its ordinary
// cohort is judged, and its ForceMinimum cohort stays a diagnostic control.
func TestProviderCountSignalJudgesOnlineRankMode(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	online := func(bands map[string]float64, forceMinimum string) string {
		payload := providerCountFixture(t, now, bands, forceMinimum)
		if !strings.Contains(payload, `"rank_mode":"quality"`) {
			t.Fatal("fixture no longer labels its rank mode")
		}
		return strings.ReplaceAll(payload, `"rank_mode":"quality"`, `"rank_mode":"online"`)
	}
	alerts, err := NewProviderCountSignal().Run(context.Background(), providerCountSyntheticSettings(t, now, online(map[string]float64{"0": 16, "3-9": 4}, "false")))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "provider-count-effective-empty")
	if !strings.Contains(alert.Markdown(), "rank_mode=online") {
		t.Fatalf("online cohort lost its rank mode:\n%s", alert.Markdown())
	}
	alerts, err = NewProviderCountSignal().Run(context.Background(), providerCountSyntheticSettings(t, now, online(map[string]float64{"0": 100}, "true")))
	if err != nil || len(alerts) != 0 {
		t.Fatalf("forced online cohort: alerts=%+v err=%v", alerts, err)
	}
}

// The zero-share cohorts exclude capability requests (FP2-MODERN.md): a
// request that keeps only modern or only post-quantum providers can rightly
// answer short. The outcomes query drops capability modern, post_quantum and
// modern_post_quantum, and keeps capability none and the series without the
// label, which older processes export. Prometheus matches a regular
// expression against the whole label value, an absent label being the empty
// string, in Go's RE2 syntax, so the matcher read from the query is
// evaluated that way here.
func TestProviderCountQueryExcludesCapabilityRequests(t *testing.T) {
	query := providerCountQuery("synthetic")
	if !strings.Contains(query, `increase(urnetwork_findproviders2_outcomes_total{env="synthetic",capability!~"modern.*|post_quantum"}[5m])`) {
		t.Fatalf("query %q does not exclude capability requests", query)
	}
	matcher := regexp.MustCompile(`capability!~"([^"]*)"`).FindStringSubmatch(query)
	if matcher == nil {
		t.Fatalf("query %q has no capability matcher", query)
	}
	excluded := regexp.MustCompile("^(?:" + matcher[1] + ")$")
	for value, counted := range map[string]bool{
		"":                    true,
		"none":                true,
		"modern":              false,
		"post_quantum":        false,
		"modern_post_quantum": false,
	} {
		if excluded.MatchString(value) == counted {
			t.Errorf("capability %q: counted %t, want %t", value, !counted, counted)
		}
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
		// every cohort the signal judges is capability-blind
		if host.Name != "metrics-1" || !strings.Contains(command, "urnetwork_findproviders2_outcomes_total") || !strings.Contains(command, "%5B5m%5D") || !strings.Contains(command, "%22synthetic%22") || !strings.Contains(command, url.QueryEscape(`capability!~"modern.*|post_quantum"`)) {
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
