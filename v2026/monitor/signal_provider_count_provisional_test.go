// The current fixed-window observation is not a learned-baseline regression.
package monitor

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestProviderCountSmallListRetainsProvisionalUnobservedBaseline(t *testing.T) {
	now := time.Date(2026, 9, 23, 2, 0, 0, 0, time.UTC)
	payload := providerCountFixture(t, now, map[string]float64{"0": 25, "1-2": 10, "3-9": 15}, "false")
	alerts, err := NewProviderCountSignal().Run(context.Background(), providerCountSyntheticSettings(t, now, payload))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "provider-count-small-list")
	if len(alerts) != 1 || alert.Severity != SeverityWarn || alert.Frame != "any/location/us/quality" || alert.Sustain != 1 {
		t.Fatal("provisional qualification changed the existing small-list warning identity or severity")
	}
	for _, want := range []string{"completed_requests=50", "zero=25", "zero_or_1_2=35", "range=5m", "qualification=provisional", "baseline_state=unobserved"} {
		if !strings.Contains(alert.Observed, want) {
			t.Errorf("provisional observation lost %q", want)
		}
	}
	for _, want := range []string{"Provisional five-minute", "does not prove a regression", "15 minutes", "materially lower cohort baseline", "not implemented", "pre-incident baseline", "Clearing the provisional five-minute WARN alone"} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Errorf("baseline authority qualifier lost %q", want)
		}
	}
	if strings.Contains(alert.Symptom, "materially degraded") {
		t.Fatal("fixed-window observation still asserts a qualified regression")
	}
}

func TestProviderCountSmallListQualificationPreservesHealthyControl(t *testing.T) {
	now := time.Date(2026, 9, 23, 2, 0, 0, 0, time.UTC)
	payload := providerCountFixture(t, now, map[string]float64{"3-9": 100}, "false")
	alerts, err := NewProviderCountSignal().Run(context.Background(), providerCountSyntheticSettings(t, now, payload))
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 0 {
		t.Fatal("an ordinary in-band discovery cohort acquired a provisional warning")
	}
}

func TestProviderCountSmallListQualificationPreservesAbsolutePage(t *testing.T) {
	now := time.Date(2026, 9, 23, 2, 0, 0, 0, time.UTC)
	payload := providerCountFixture(t, now, map[string]float64{"0": 16, "3-9": 4}, "false")
	alerts, err := NewProviderCountSignal().Run(context.Background(), providerCountSyntheticSettings(t, now, payload))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "provider-count-effective-empty")
	if len(alerts) != 1 || alert.Severity != SeverityPage || !strings.Contains(alert.Observed, "range=5m") || strings.Contains(alert.Observed, "baseline_state=") || strings.Contains(alert.Symptom, "Provisional") {
		t.Fatal("missing learned baseline weakened the independent absolute empty-list page")
	}
}

func TestProviderCountSmallListQualificationKeepsDirectIntentSeparate(t *testing.T) {
	now := time.Date(2026, 9, 23, 2, 0, 0, 0, time.UTC)
	payload := providerCountDirectControlFixture(t, now, "direct", map[string]float64{"0": 100})
	alerts, err := NewProviderCountSignal().Run(context.Background(), providerCountSyntheticSettings(t, now, payload))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "provider-count-direct-unclassified")
	if len(alerts) != 1 || alert.Severity != SeverityWarn || strings.Contains(alert.Observed, "baseline_state=") || !strings.Contains(alert.Context, "Intent remains unknown, not healthy") {
		t.Fatal("baseline qualifier merged the separate missing direct-request-intent boundary")
	}
}
