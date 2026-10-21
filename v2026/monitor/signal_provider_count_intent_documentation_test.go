// Request intent is not recoverable from broad response labels. Keep the
// observation visible without promoting an omitted request cap to scarcity.
package monitor

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func TestMonitorDocumentationProviderCountRequestIntent(t *testing.T) {
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
		"`force_minimum=false` does not exclude `ForceCount=true`",
		"non-direct `location/quality` request can intentionally cap discovery selection",
		"`Count=0`, `Count=1`, or `Count=2`",
		"without ForceCount, the selector uses at least 20",
		"Explicit ClientId additions, if present, remain separate from that discovery cap",
		"outcome metric lacks ForceCount and requested/effective count",
		"remains provisional without a request-intent denominator",
		"Preserve the WARN",
		"do not infer scarcity or suppress it merely because a cap is possible",
		"current Connect Go request type does not expose ForceCount",
		"`caller_country=us` identifies the caller, not the requested/provider country",
		"`ip_family=any` combines the empty and v4-capable request filters",
		"post-filter candidate pool",
		"cannot alone reconstruct the exact outcome cohort or raw cache population",
	} {
		if !strings.Contains(documentation, required) {
			t.Errorf("SIGNALS.md lost request-intent boundary %q", required)
		}
	}
}

// Omitted ForceCount/Count evidence cannot erase the existing threshold
// crossing, invent a qualified baseline, or change its ordinary cohort.
func TestProviderCountUnknownRequestIntentPreservesWarning(t *testing.T) {
	now := time.Date(2026, 9, 23, 6, 45, 0, 0, time.UTC)
	payload := providerCountFixture(t, now, map[string]float64{
		"0": 237, "1-2": 104, "10+": 209,
	}, "false")
	alerts, err := NewProviderCountSignal().Run(context.Background(), providerCountSyntheticSettings(t, now, payload))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "provider-count-small-list")
	if len(alerts) != 1 || alert.Severity != SeverityWarn || alert.Frame != "any/location/us/quality" {
		t.Fatal("unknown request caps suppressed or reclassified the observed small-list warning")
	}
	for _, required := range []string{
		"completed_requests=550", "zero=237", "zero_or_1_2=341",
		"range=5m", "qualification=provisional", "baseline_state=unobserved",
	} {
		if !strings.Contains(alert.Observed, required) {
			t.Errorf("request-intent uncertainty changed bounded observation %q", required)
		}
	}
	if !strings.Contains(alert.Symptom, "Provisional") {
		t.Fatal("request-intent uncertainty became an asserted scarcity regression")
	}
}
