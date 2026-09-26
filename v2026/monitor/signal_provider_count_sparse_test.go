// Traffic-bearing small responses below the ordinary WARN denominator retain
// an intent-unknown observation; threshold movement is not recovery.
package monitor

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func TestProviderCountSparseSmallListIsUnknownNotHealthy(t *testing.T) {
	now := time.Date(2026, 9, 23, 7, 30, 0, 0, time.UTC)
	payload := providerCountFixture(t, now, map[string]float64{"0": 18.90, "1-2": 11.54}, "false")
	settings := providerCountSyntheticSettings(t, now, payload)
	env, err := newProbeEnv(settings.withDefaults())
	if err != nil {
		t.Fatal(err)
	}
	findings, err := (providerCountProbe{}).check(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 || findings[0].healthy || findings[0].class != "provider-count-small-list-unclassified" {
		t.Fatalf("traffic-bearing all-small cohort became silent/healthy instead of intent unknown: %+v", findings)
	}
	alert := alertFromFinding(settings, "2.9a", "provider-count", "provider count", findings[0])
	if alert.Severity != SeverityWarn || alert.PageSustain != 0 || alert.Sustain != 1 || alert.Frame != "any/location/us/quality" {
		t.Fatal("unknown small-list intent became a PAGE or changed its fixed cohort")
	}
	for _, required := range []string{
		"qualification=unclassified", "sample_state=below_warn_min",
		"request_intent=unknown", "baseline_state=unobserved", "completed_requests_for_threshold=",
		"completed_requests=30", "zero=19", "zero_or_1_2=30", "range=5m",
	} {
		if !strings.Contains(alert.Observed, required) {
			t.Errorf("unclassified observation lost %q", required)
		}
	}
	for _, required := range []string{
		"does not establish supply scarcity, provider failure or recovery",
		"ForceMinimum=false does not exclude an intentionally capped request",
		"Caller country is not target/provider country",
		"threshold movement is not recovery", "not a unique-caller denominator",
	} {
		if !strings.Contains(alert.Markdown(), required) {
			t.Errorf("unclassified evidence qualifier lost %q", required)
		}
	}
	if strings.Contains(alert.Symptom, "effectively empty") || strings.Contains(alert.Symptom, "materially degraded") {
		t.Fatal("a low-volume observation asserts a scarcity diagnosis")
	}
}

func TestProviderCountSparsePageExitRetainsDistinctUnknownIdentity(t *testing.T) {
	now := time.Date(2026, 9, 23, 7, 30, 0, 0, time.UTC)
	before, err := NewProviderCountSignal().Run(context.Background(), providerCountSyntheticSettings(t, now,
		providerCountFixture(t, now, map[string]float64{"0": 20, "1-2": 3}, "false")))
	if err != nil {
		t.Fatal(err)
	}
	page := requireAlertClass(t, before, "provider-count-effective-empty")
	after, err := NewProviderCountSignal().Run(context.Background(), providerCountSyntheticSettings(t, now,
		providerCountFixture(t, now, map[string]float64{"0": 19, "1-2": 11}, "false")))
	if err != nil {
		t.Fatal(err)
	}
	unknown := requireAlertClass(t, after, "provider-count-small-list-unclassified")
	if len(before) != 1 || len(after) != 1 || page.Severity != SeverityPage || unknown.Severity != SeverityWarn ||
		page.Frame != unknown.Frame || page.Class == unknown.Class {
		t.Fatal("PAGE threshold movement was erased or misrepresented as the same alert identity")
	}
}

// The new observation fills only 20–<50. Exact PAGE precedence and the ordinary
// >=50 provisional warning remain; rounded display counts do not select a band.
func TestProviderCountSparseThresholdBoundaries(t *testing.T) {
	now := time.Date(2026, 9, 23, 7, 30, 0, 0, time.UTC)
	for _, test := range []struct {
		name  string
		bands map[string]float64
		class string
	}{
		{name: "lower volume and ratio equality", bands: map[string]float64{"1-2": 10, "3-9": 10}, class: "provider-count-small-list-unclassified"},
		{name: "fractional below upper floor", bands: map[string]float64{"1-2": 49.999}, class: "provider-count-small-list-unclassified"},
		{name: "upper floor retains provisional", bands: map[string]float64{"1-2": 25, "3-9": 25}, class: "provider-count-small-list"},
		{name: "page equality wins", bands: map[string]float64{"0": 16, "1-2": 4}, class: "provider-count-effective-empty"},
	} {
		alerts, err := NewProviderCountSignal().Run(context.Background(), providerCountSyntheticSettings(t, now,
			providerCountFixture(t, now, test.bands, "false")))
		if err != nil {
			t.Fatal(err)
		}
		if len(alerts) != 1 || alerts[0].Class != test.class {
			t.Errorf("%s alerts=%+v, want only %s", test.name, alerts, test.class)
		}
	}
}

// These controls passed before the fix and must remain unchanged. No new
// low-volume rule is applied to direct specs, diagnostic calls, or in-band lists.
func TestProviderCountSparsePreservesHealthyDirectAndVolumeControls(t *testing.T) {
	now := time.Date(2026, 9, 23, 7, 30, 0, 0, time.UTC)
	for _, test := range []struct {
		name    string
		payload string
		class   string
	}{
		{name: "ordinary healthy", payload: providerCountFixture(t, now, map[string]float64{"3-9": 30}, "false")},
		{name: "small ratio below half", payload: providerCountFixture(t, now, map[string]float64{"1-2": 9, "3-9": 11}, "false")},
		{name: "below observation floor", payload: providerCountFixture(t, now, map[string]float64{"1-2": 19.999}, "false")},
		{name: "direct small below threshold", payload: providerCountDirectControlFixture(t, now, "direct", map[string]float64{"1-2": 30})},
		{name: "direct existing zero threshold", payload: providerCountDirectControlFixture(t, now, "direct", map[string]float64{"0": 25, "1-2": 5}), class: "provider-count-direct-unclassified"},
		{name: "ForceMinimum remains excluded", payload: providerCountFixture(t, now, map[string]float64{"0": 19, "1-2": 11}, "true")},
	} {
		alerts, err := NewProviderCountSignal().Run(context.Background(), providerCountSyntheticSettings(t, now, test.payload))
		if err != nil {
			t.Fatal(err)
		}
		if test.class == "" {
			if len(alerts) != 0 {
				t.Errorf("%s acquired an alert: %+v", test.name, alerts)
			}
		} else if len(alerts) != 1 || alerts[0].Class != test.class || alerts[0].Severity != SeverityWarn {
			t.Errorf("%s lost its existing direct-intent warning: %+v", test.name, alerts)
		}
	}
}

func TestProviderCountSparseDoesNotHideIndependentPage(t *testing.T) {
	now := time.Date(2026, 9, 23, 7, 30, 0, 0, time.UTC)
	payload := providerCountJoinedControlFixtures(t,
		providerCountFixture(t, now, map[string]float64{"0": 19, "1-2": 11}, "false"),
		providerCountDirectControlFixture(t, now, "group", map[string]float64{"0": 20}),
	)
	alerts, err := NewProviderCountSignal().Run(context.Background(), providerCountSyntheticSettings(t, now, payload))
	if err != nil {
		t.Fatal(err)
	}
	unknown := requireAlertClass(t, alerts, "provider-count-small-list-unclassified")
	page := requireAlertClass(t, alerts, "provider-count-effective-empty")
	if len(alerts) != 2 || unknown.Severity != SeverityWarn || page.Severity != SeverityPage || unknown.Frame == page.Frame {
		t.Fatal("unknown request intent masked an independent discovery PAGE")
	}
}

func TestMonitorDocumentationProviderCountSparseThresholdGap(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("SIGNALS.md")
	if err != nil {
		t.Fatal(err)
	}
	_, section, found := strings.Cut(string(data), "### 2.9a ")
	if !found {
		t.Fatal("SIGNALS.md lost the provider-count section")
	}
	section, _, found = strings.Cut(section, "\n### 2.10 ")
	if !found {
		t.Fatal("SIGNALS.md lost the provider-count section boundary")
	}
	documentation := strings.Join(strings.Fields(section), " ")
	for _, required := range []string{
		"`provider-count-small-list-unclassified`",
		"20–<50 completed responses", "threshold movement, not recovery",
		"After PAGE precedence", "nonpaging, request-intent-unknown WARN",
		"at least 50% in the zero-or-one-to-two bands",
		"`sample_state=below_warn_min request_intent=unknown baseline_state=unobserved`",
		"never a scarcity diagnosis or a healthy sentinel for this band",
		"PAGE and >=50 provisional WARN thresholds remain unchanged",
		"visibility can warn on legitimate caps or restrictive targets",
		"does not establish a unique-caller or request-intent denominator",
		"not an in-place severity transition",
	} {
		if !strings.Contains(documentation, required) {
			t.Errorf("SIGNALS.md lost the sparse-response evidence contract %q", required)
		}
	}
}
