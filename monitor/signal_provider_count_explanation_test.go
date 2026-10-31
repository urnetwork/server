// Empty answers the request or its narrow target explains are not platform
// empties. The 2026-10-25 to 10-31 Main page was one caller-country cohort
// (any/location/au) of about 21 requests in five minutes, four a minute per
// rank mode, whose zeros were city cache_empty (the score export holds no
// admitted provider for the city) or city filtered_destinations (the
// request's own exclusions removed every admitted provider). Every value
// below is synthetic.
package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// One explanation row per target kind and reason, tagged as the probe's query
// tags the empty-outcome part.
type providerCountExplanation struct {
	targetKind string
	reason     string
	value      float64
}

func providerCountExplanationFixture(t testing.TB, at time.Time, locationKind string, country string, explanations ...providerCountExplanation) string {
	t.Helper()
	result := []map[string]any{}
	for _, explanation := range explanations {
		result = append(result, map[string]any{
			"metric": map[string]string{
				"monitor_part": "empty", "ip_family": "any", "location_kind": locationKind, "caller_country": country,
				"rank_mode": "quality", "force_minimum": "false", "target_kind": explanation.targetKind, "reason": explanation.reason,
			},
			"value": []any{float64(at.Unix()), fmt.Sprintf("%.3f", explanation.value)},
		})
	}
	payload, err := json.Marshal(map[string]any{"status": "success", "data": map[string]any{"resultType": "vector", "result": result}})
	if err != nil {
		t.Fatal(err)
	}
	return string(payload)
}

// An ordinary location cohort for one caller country with its explanations.
func providerCountExplainedPayload(t testing.TB, now time.Time, country string, bands map[string]float64, explanations ...providerCountExplanation) string {
	t.Helper()
	outcomes := strings.ReplaceAll(providerCountFixture(t, now, bands, "false"), `"caller_country":"us"`, `"caller_country":"`+country+`"`)
	return providerCountJoinedControlFixtures(t, outcomes, providerCountExplanationFixture(t, now, "location", country, explanations...))
}

func providerCountExplainedFindings(t *testing.T, now time.Time, payload string) []finding {
	t.Helper()
	env, err := newProbeEnv(providerCountSyntheticSettings(t, now, payload).withDefaults())
	if err != nil {
		t.Fatal(err)
	}
	findings, err := (&providerCountProbe{}).check(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	return findings
}

func providerCountFindingClasses(findings []finding) []string {
	classes := []string{}
	for _, f := range findings {
		if !f.clearHold && !f.healthy {
			classes = append(classes, f.class)
		}
	}
	return classes
}

// The incident's shape: twenty zeros in twenty-one completed requests, every
// zero explained by the city target or the request's own exclusions. Neither
// the PAGE nor an intent-unknown small-list WARN may claim it. Its twenty-one
// requests are judgeable evidence of its own state, so on a first execution it
// holds nothing; it cannot vouch for another cohort's incident either
// (signal_provider_count_cohort_recovery_test.go).
func TestProviderCountExplainedNarrowTargetCohortDoesNotPage(t *testing.T) {
	now := time.Date(2026, 10, 31, 3, 30, 0, 0, time.UTC)
	for _, explanations := range [][]providerCountExplanation{
		{{"city", "cache_empty", 20}},
		{{"city", "filtered_destinations", 20}},
		{{"city", "cache_empty", 11}, {"city", "filtered_destinations", 9}},
		{{"region", "cache_empty", 15}, {"country", "filtered_destinations", 4}, {"country", "intentional_zero", 1}},
	} {
		findings := providerCountExplainedFindings(t, now, providerCountExplainedPayload(t, now, "au", map[string]float64{"0": 20, "10+": 1}, explanations...))
		if classes := providerCountFindingClasses(findings); len(classes) != 0 {
			t.Fatalf("explained zeros %+v raised %v", explanations, classes)
		}
		for _, f := range findings {
			if f.clearHold {
				t.Fatalf("explained cohort %+v that reported nothing held %s", explanations, f.class)
			}
		}
	}
}

// Zeros the explanations do not account for still page: missing cache
// metadata, a country, group or best-available target the export wrote
// empty, filters other than a window refill's, and client-ID exclusions,
// which can be the client's own removals of providers that failed it.
func TestProviderCountUnexplainedZerosStillPage(t *testing.T) {
	now := time.Date(2026, 10, 31, 3, 30, 0, 0, time.UTC)
	for _, explanation := range []providerCountExplanation{
		{"city", "cache_missing", 20},
		{"city", "cache_page_gap", 20},
		{"city", "eligible_not_selected", 20},
		{"city", "filtered_hard", 20},
		{"city", "filtered_mixed", 20},
		{"city", "filtered_client_ids", 20},
		{"city", "filtered_explicit_mixed", 20},
		{"city", "filtered_explicit", 20},
		{"country", "cache_empty", 20},
		{"location_unknown", "cache_empty", 20},
		{"mixed", "cache_empty", 20},
	} {
		findings := providerCountExplainedFindings(t, now, providerCountExplainedPayload(t, now, "au", map[string]float64{"0": 20, "10+": 1}, explanation))
		if classes := providerCountFindingClasses(findings); len(classes) != 1 || classes[0] != "provider-count-effective-empty" {
			t.Fatalf("unexplained %s/%s zeros: classes %v, want the effective-empty PAGE", explanation.targetKind, explanation.reason, classes)
		}
	}
	for _, kind := range []struct{ locationKind, targetKind string }{{"group", "group"}, {"best-available", "best_available"}} {
		outcomes := strings.ReplaceAll(providerCountFixture(t, now, map[string]float64{"0": 20}, "false"), `"location_kind":"location"`, `"location_kind":"`+kind.locationKind+`"`)
		payload := providerCountJoinedControlFixtures(t, outcomes, providerCountExplanationFixture(t, now, kind.locationKind, "us", providerCountExplanation{kind.targetKind, "cache_empty", 20}))
		if classes := providerCountFindingClasses(providerCountExplainedFindings(t, now, payload)); len(classes) != 1 || classes[0] != "provider-count-effective-empty" {
			t.Fatalf("empty %s target: classes %v, want the effective-empty PAGE", kind.targetKind, classes)
		}
	}
}

// Refill answers are not availability observations: they leave the
// denominator, so they cannot dilute the platform zeros of the same cohort
// below the PAGE ratio.
func TestProviderCountRefillAnswersLeaveTheAvailabilityDenominator(t *testing.T) {
	now := time.Date(2026, 10, 31, 3, 30, 0, 0, time.UTC)
	payload := providerCountExplainedPayload(t, now, "us", map[string]float64{"0": 100, "10+": 5},
		providerCountExplanation{"country", "filtered_destinations", 65}, providerCountExplanation{"country", "cache_missing", 35})
	alerts, err := NewProviderCountSignal().Run(context.Background(), providerCountSyntheticSettings(t, now, payload))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "provider-count-effective-empty")
	if alert.Severity != SeverityPage {
		t.Fatalf("severity=%s, want page", alert.Severity)
	}
	for _, want := range []string{"completed_requests=105", "zero=100", "zero_explained_by_request=65", "zero_explained_by_target_supply=0", "zero_unexplained=35", "considered_requests=40", "zero_explanations=joined"} {
		if !strings.Contains(alert.Observed, want) {
			t.Errorf("observed lost %q: %s", want, alert.Observed)
		}
	}
}

// A narrow-target supply gap at product scale is a WARN of its own, never a
// PAGE; a single client's refill loop stays below its floor.
func TestProviderCountNarrowTargetSupplyGapWarnsAtTheWarnFloor(t *testing.T) {
	now := time.Date(2026, 10, 31, 3, 30, 0, 0, time.UTC)
	alerts, err := NewProviderCountSignal().Run(context.Background(), providerCountSyntheticSettings(t, now,
		providerCountExplainedPayload(t, now, "us", map[string]float64{"0": 60, "10+": 4}, providerCountExplanation{"city", "cache_empty", 60})))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "provider-count-target-supply-empty")
	if len(alerts) != 1 || alert.Severity != SeverityWarn || alert.Frame != "any/location/us/quality" {
		t.Fatalf("narrow supply gap: %+v", alerts)
	}
	for _, want := range []string{"completed_requests=64", "zero_explained_by_target_supply=60", "zero_unexplained=0"} {
		if !strings.Contains(alert.Observed, want) {
			t.Errorf("observed lost %q: %s", want, alert.Observed)
		}
	}
	for _, want := range []string{"cache_empty", "no admitted provider", "not a selection or cache failure", "SIGNALS.md §2.9a"} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Errorf("markdown lost %q", want)
		}
	}
	alerts, err = NewProviderCountSignal().Run(context.Background(), providerCountSyntheticSettings(t, now,
		providerCountExplainedPayload(t, now, "us", map[string]float64{"0": 49}, providerCountExplanation{"city", "cache_empty", 49})))
	if err != nil || len(alerts) != 0 {
		t.Fatalf("a supply gap below the WARN floor alerted: alerts=%+v err=%v", alerts, err)
	}
}

// Explanations can only lower severity, so one that cannot be trusted is
// discarded whole and the cohort is judged as before; a label value outside
// the fixed vocabulary never reaches the alert.
func TestProviderCountUntrustedExplanationsKeepTheExistingVerdict(t *testing.T) {
	now := time.Date(2026, 10, 31, 3, 30, 0, 0, time.UTC)
	bands := map[string]float64{"0": 20, "10+": 1}
	stale := providerCountExplanationFixture(t, now.Add(-10*time.Minute), "location", "au", providerCountExplanation{"city", "cache_empty", 20})
	for name, explanations := range map[string]string{
		"unknown reason":      providerCountExplanationFixture(t, now, "location", "au", providerCountExplanation{"city", "cache_empty", 10}, providerCountExplanation{"city", "synthetic-private.example", 10}),
		"unknown target kind": providerCountExplanationFixture(t, now, "location", "au", providerCountExplanation{"synthetic-private.example", "cache_empty", 20}),
		"stale sample":        stale,
		"negative value":      providerCountExplanationFixture(t, now, "location", "au", providerCountExplanation{"city", "cache_empty", -20}),
		"duplicate row":       providerCountExplanationFixture(t, now, "location", "au", providerCountExplanation{"city", "cache_empty", 10}, providerCountExplanation{"city", "cache_empty", 10}),
	} {
		outcomes := strings.ReplaceAll(providerCountFixture(t, now, bands, "false"), `"caller_country":"us"`, `"caller_country":"au"`)
		alerts, err := NewProviderCountSignal().Run(context.Background(), providerCountSyntheticSettings(t, now, providerCountJoinedControlFixtures(t, outcomes, explanations)))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		alert := requireAlertClass(t, alerts, "provider-count-effective-empty")
		if alert.Severity != SeverityPage || !strings.Contains(alert.Observed, "zero_explanations=invalid") || !strings.Contains(alert.Observed, "zero_unexplained=20") {
			t.Fatalf("%s: untrusted explanations changed the verdict: %+v", name, alert)
		}
		if strings.Contains(alert.Markdown(), "synthetic-private.example") {
			t.Fatalf("%s: an unbounded label reached the alert", name)
		}
	}
}

// Absent explanations, as from processes that predate the counter, change
// nothing; an explanation cannot exceed the zeros it explains; and a response
// with explanations but no outcome bands is still missing telemetry.
func TestProviderCountExplanationsAbsentOrExcessive(t *testing.T) {
	now := time.Date(2026, 10, 31, 3, 30, 0, 0, time.UTC)
	alerts, err := NewProviderCountSignal().Run(context.Background(), providerCountSyntheticSettings(t, now, providerCountFixture(t, now, map[string]float64{"0": 20, "10+": 1}, "false")))
	if err != nil {
		t.Fatal(err)
	}
	if alert := requireAlertClass(t, alerts, "provider-count-effective-empty"); !strings.Contains(alert.Observed, "zero_explanations=absent") || !strings.Contains(alert.Observed, "zero_unexplained=20") {
		t.Fatalf("absent explanations: %s", alert.Observed)
	}
	findings := providerCountExplainedFindings(t, now, providerCountExplainedPayload(t, now, "au", map[string]float64{"0": 20, "10+": 1}, providerCountExplanation{"city", "cache_empty", 25.5}))
	if classes := providerCountFindingClasses(findings); len(classes) != 0 {
		t.Fatalf("an explanation past its zeros raised %v", classes)
	}
	_, err = NewProviderCountSignal().Run(context.Background(), providerCountSyntheticSettings(t, now, providerCountExplanationFixture(t, now, "location", "au", providerCountExplanation{"city", "cache_empty", 20})))
	if err == nil || !strings.Contains(err.Error(), "outcome metric is absent") {
		t.Fatalf("explanations without outcome bands: err=%v, want missing telemetry", err)
	}
}

// One query reads both parts at one instant, the explanations with the same
// capability selector as the outcome bands.
func TestProviderCountQueryJoinsEmptyOutcomeExplanations(t *testing.T) {
	query := providerCountQuery("synthetic")
	for _, want := range []string{
		`sum by (ip_family,location_kind,caller_country,rank_mode,force_minimum,result_count) (increase(urnetwork_findproviders2_outcomes_total{env="synthetic",capability!~"modern.*|post_quantum"}[5m]))`,
		`sum by (ip_family,location_kind,caller_country,rank_mode,force_minimum,target_kind,target_country,reason) (increase(urnetwork_findproviders2_empty_outcomes_total{env="synthetic",capability!~"modern.*|post_quantum"}[5m]))`,
		`"monitor_part","empty","",""`,
		` or `,
	} {
		if !strings.Contains(query, want) {
			t.Errorf("query lost %q: %s", want, query)
		}
	}
}

func TestMonitorDocumentationProviderCountEmptyExplanations(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("SIGNALS.md")
	if err != nil {
		t.Fatal(err)
	}
	_, section, found := strings.Cut(string(data), "### 2.9a ")
	if !found {
		t.Fatal("SIGNALS.md lost the provider-count section")
	}
	section, _, found = strings.Cut(section, "\n### 2.9b ")
	if !found {
		t.Fatal("SIGNALS.md lost the provider-count section boundary")
	}
	documentation := strings.Join(strings.Fields(section), " ")
	for _, required := range []string{
		"`urnetwork_findproviders2_empty_outcomes_total`",
		"`provider-count-target-supply-empty`",
		"`filtered_destinations` and `intentional_zero`",
		"`filtered_client_ids`, `filtered_explicit_mixed` and `filtered_explicit` still count",
		"`cache_empty` for a `city` or `region` target",
		"leave the denominator",
		"A country, group, best-available, mixed or unknown target the export wrote empty still counts toward the PAGE",
		"discarded whole",
		"four requests a minute per rank mode",
		"`zero_explanations=joined`",
	} {
		if !strings.Contains(documentation, required) {
			t.Errorf("SIGNALS.md lost the empty-explanation contract %q", required)
		}
	}
}
