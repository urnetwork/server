// Direct caller-chosen IDs cannot measure the size of the discovery pool.
package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestProviderCountDirectEmptyIsUnknownNotSupplyFailure(t *testing.T) {
	now := time.Date(2026, 9, 23, 2, 0, 0, 0, time.UTC)
	payload := providerCountDirectControlFixture(t, now, "direct", map[string]float64{"0": 80, "1-2": 20})
	alerts, err := NewProviderCountSignal().Run(context.Background(), providerCountSyntheticSettings(t, now, payload))
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 1 {
		t.Fatalf("direct cohort produced %d alerts, want one intent-unknown warning", len(alerts))
	}
	alert := requireAlertClass(t, alerts, "provider-count-direct-unclassified")
	if alert.Severity != SeverityWarn || alert.PageSustain != 0 || alert.Frame != "any/direct/unknown/quality" {
		t.Fatal("unknown direct intent became a page or a different request cohort")
	}
	for _, want := range []string{"completed_requests=100", "zero=80", "zero_or_1_2=100", "range=5m"} {
		if !strings.Contains(alert.Observed, want) {
			t.Errorf("direct evidence lost %q", want)
		}
	}
	for _, want := range []string{"empty specification", "all-excluded", "does not prove supply depletion", "Intent remains unknown, not healthy", "default quality label", "nonexcluded explicit-result count"} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Errorf("direct qualifier lost %q", want)
		}
	}
	for _, forbidden := range []string{"effectively empty", "materially degraded", "after network-only, IP-family"} {
		if strings.Contains(alert.Markdown(), forbidden) {
			t.Errorf("direct warning retained false discovery attribution %q", forbidden)
		}
	}
}

// A caller deliberately asking for one or two IDs can have perfect responses.
// The old small-list threshold cannot supply the missing intent authority.
func TestProviderCountDirectOneTwoIsNotSupplyScarcity(t *testing.T) {
	now := time.Date(2026, 9, 23, 2, 0, 0, 0, time.UTC)
	payload := providerCountDirectControlFixture(t, now, "direct", map[string]float64{"1-2": 100})
	alerts, err := NewProviderCountSignal().Run(context.Background(), providerCountSyntheticSettings(t, now, payload))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "provider-count-direct-unclassified")
	if len(alerts) != 1 || alert.Severity != SeverityWarn || !strings.Contains(alert.Observed, "zero=0") || !strings.Contains(alert.Mechanism, "can legitimately return one or two") {
		t.Fatal("valid small direct-response shape acquired a supply diagnosis")
	}
}

// A low-volume direct cohort does not justify either a repeated warning or a
// positive discovery-health sentinel. No traffic is manufactured to classify it.
func TestProviderCountDirectBelowThresholdHasNoHealthySentinel(t *testing.T) {
	now := time.Date(2026, 9, 23, 2, 0, 0, 0, time.UTC)
	payload := providerCountDirectControlFixture(t, now, "direct", map[string]float64{"1-2": 5})
	settings := providerCountSyntheticSettings(t, now, payload).withDefaults()
	env, err := newProbeEnv(settings)
	if err != nil {
		t.Fatal(err)
	}
	findings, err := (providerCountProbe{}).check(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 0 {
		t.Fatal("below-threshold direct-only traffic acquired a warning or discovery-healthy sentinel")
	}
}

func TestProviderCountDirectDoesNotHideDiscoveryFailure(t *testing.T) {
	now := time.Date(2026, 9, 23, 2, 0, 0, 0, time.UTC)
	direct := providerCountDirectControlFixture(t, now, "direct", map[string]float64{"0": 100})
	discovery := providerCountDirectControlFixture(t, now, "location", map[string]float64{"0": 20})
	payload := providerCountJoinedControlFixtures(t, direct, discovery)
	alerts, err := NewProviderCountSignal().Run(context.Background(), providerCountSyntheticSettings(t, now, payload))
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 2 {
		t.Fatalf("mixed cohorts produced %d alerts, want both boundaries", len(alerts))
	}
	unknown := requireAlertClass(t, alerts, "provider-count-direct-unclassified")
	failure := requireAlertClass(t, alerts, "provider-count-effective-empty")
	if unknown.Severity != SeverityWarn || failure.Severity != SeverityPage || failure.Frame != "any/location/us/quality" || !strings.Contains(failure.Observed, "completed_requests=20") {
		t.Fatal("direct qualification changed an independently empty discovery cohort")
	}
}

func TestProviderCountDirectCorrectionPreservesDiscoveryHealthyControl(t *testing.T) {
	now := time.Date(2026, 9, 23, 2, 0, 0, 0, time.UTC)
	payload := providerCountDirectControlFixture(t, now, "location", map[string]float64{"3-9": 100})
	settings := providerCountSyntheticSettings(t, now, payload).withDefaults()
	env, err := newProbeEnv(settings)
	if err != nil {
		t.Fatal(err)
	}
	findings, err := (providerCountProbe{}).check(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 || !findings[0].healthy || findings[0].class != "provider-count-degraded" {
		t.Fatal("ordinary healthy discovery lost its existing in-band sentinel")
	}
}

func providerCountDirectControlFixture(t testing.TB, now time.Time, kind string, bands map[string]float64) string {
	t.Helper()
	country := "us"
	if kind == "direct" {
		country = "unknown"
	}
	result := []map[string]any{}
	for _, band := range []string{"0", "1-2", "3-9", "10+"} {
		value, ok := bands[band]
		if !ok {
			continue
		}
		result = append(result, map[string]any{
			"metric": map[string]string{"ip_family": "any", "location_kind": kind, "caller_country": country, "rank_mode": "quality", "force_minimum": "false", "result_count": band},
			"value":  []any{float64(now.Unix()), fmt.Sprintf("%.3f", value)},
		})
	}
	payload, err := json.Marshal(map[string]any{"status": "success", "data": map[string]any{"resultType": "vector", "result": result}})
	if err != nil {
		t.Fatal(err)
	}
	return string(payload)
}

func providerCountJoinedControlFixtures(t testing.TB, payloads ...string) string {
	t.Helper()
	result := []json.RawMessage{}
	for _, payload := range payloads {
		var decoded struct {
			Data struct {
				Result []json.RawMessage `json:"result"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
			t.Fatal(err)
		}
		result = append(result, decoded.Data.Result...)
	}
	payload, err := json.Marshal(map[string]any{"status": "success", "data": map[string]any{"resultType": "vector", "result": result}})
	if err != nil {
		t.Fatal(err)
	}
	return string(payload)
}
