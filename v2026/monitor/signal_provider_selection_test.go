// Synthetic request-boundary, source-authority and privacy controls for §2.9c.
package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// One private process and one fixed cohort, with explicit timestamp witnesses.
func selectionTestRows(reason string, count float64) []map[string]any {
	now := pickerTestNow()
	if reason == "returned" {
		reason = "returned_10_plus"
	}
	rows := []map[string]any{}
	add := func(field string, value float64, outcome bool) {
		labels := map[string]string{"env": "synthetic", "job": "api", "host": "api-synthetic", "block": "blue", "instance": "private-selection-instance", "monitor_selection": field}
		if outcome {
			labels["target_kind"], labels["request_class"], labels["ip_family"] = "country", "default_minimum", "any"
			labels["rank_mode"], labels["outcome"], labels["reason"] = "quality", "zero", reason
			if strings.HasPrefix(reason, "returned") {
				labels["outcome"] = "nonempty"
			}
		}
		rows = append(rows, map[string]any{"metric": labels, "value": []any{now.Unix(), fmt.Sprint(value)}})
	}
	for _, bound := range []string{"now", "prior"} {
		stamp := now.Add(-5 * time.Second)
		if bound == "prior" {
			stamp = stamp.Add(-5 * time.Minute)
		}
		add(bound+"_start", float64(now.Add(-time.Hour).Unix()), false)
		add(bound+"_schema", 2, false)
		add(bound+"_start_time", float64(stamp.Unix()), false)
		add(bound+"_schema_time", float64(stamp.Unix()), false)
	}
	add("count", count, true)
	add("count_time", float64(now.Add(-5*time.Second).Unix()), true)
	add("resets", 0, true)
	add("samples", 20, true)
	return rows
}

// Old producers remain usable for the original zero invariant, but cannot
// certify result shape; an unknown or changing schema supplies neither.
func TestProviderSelectionSignalMixedSchemaKeepsShapeUnknown(t *testing.T) {
	rows := selectionTestRows("returned_small_filtered_destinations", 80)
	oldRows := selectionTestRows("eligible_not_selected", 3)
	for _, row := range oldRows {
		labels := row["metric"].(map[string]string)
		labels["block"] = "green"
		if labels["monitor_selection"] == "now_schema" || labels["monitor_selection"] == "prior_schema" {
			row["value"].([]any)[1] = "1"
		}
	}
	rows = append(rows, oldRows...)
	settings := selectionTestSettings(t, pickerTestPayload(t, rows))
	settings.LogServiceBlocks["api"] = []string{"blue", "green"}
	alerts, err := NewProviderSelectionSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	requireAlertClass(t, alerts, "provider-selection-empty-despite-eligible")
	unknown := requireAlertClass(t, alerts, "provider-selection-unavailable")
	if unknown.Frame != "response-shape" || !strings.Contains(unknown.Markdown(), "paired_processes=2 shape_paired_processes=1 expected_slots=2") {
		t.Fatal("mixed producer versions lost old invariant authority or certified complete response shape")
	}
}

// A schema switch inside a process window cannot masquerade as a complete
// generation. Arbitrary versions and cross-version reason shapes fail closed.
func TestProviderSelectionSignalSchemaVersionBoundaries(t *testing.T) {
	for _, defect := range []string{"changed", "future", "legacy-new-reason", "new-legacy-reason"} {
		rows := selectionTestRows("returned_small_sample", 80)
		for _, row := range rows {
			labels := row["metric"].(map[string]string)
			field := labels["monitor_selection"]
			switch {
			case defect == "changed" && field == "prior_schema":
				row["value"].([]any)[1] = "1"
			case defect == "future" && (field == "now_schema" || field == "prior_schema"):
				row["value"].([]any)[1] = "3"
			case defect == "legacy-new-reason" && (field == "now_schema" || field == "prior_schema"):
				row["value"].([]any)[1] = "1"
			case defect == "new-legacy-reason" && labels["reason"] != "":
				labels["reason"] = "returned"
			}
		}
		alerts, err := NewProviderSelectionSignal().Run(context.Background(), selectionTestSettings(t, pickerTestPayload(t, rows)))
		if err != nil || len(alerts) != 1 {
			t.Fatalf("%s did not retain schema uncertainty: alerts=%d error=%v", defect, len(alerts), err)
		}
		requireAlertClass(t, alerts, "provider-selection-unavailable")
	}
}

// Small-list explanations are observations, not supply verdicts or permission
// to suppress the independent user-visible count signal.
func TestProviderSelectionSignalSmallResultReasonsStayBounded(t *testing.T) {
	for _, reason := range []string{
		"returned_3_9", "returned_10_plus", "returned_small_direct", "returned_small_mixed_direct", "returned_small_requested", "returned_small_cache_unknown", "returned_small_eligible", "returned_small_sample",
		"returned_small_filtered_hard", "returned_small_filtered_network", "returned_small_filtered_family", "returned_small_filtered_explicit", "returned_small_filtered_client_ids", "returned_small_filtered_destinations", "returned_small_filtered_explicit_mixed", "returned_small_filtered_mixed",
	} {
		rows := selectionTestRows(reason, 80)
		settings := selectionTestSettings(t, pickerTestPayload(t, rows))
		env, err := newProbeEnv(settings)
		if err != nil {
			t.Fatal(err)
		}
		evidence := parseProviderSelection(pickerTestPayload(t, rows), "synthetic", pickerTestNow(), pickerScope(env))
		if !evidence.complete || !evidence.shapeComplete || len(evidence.counts) != 1 {
			t.Fatalf("%s lost its complete fixed-vocabulary attribution", reason)
		}
		alerts, err := NewProviderSelectionSignal().Run(context.Background(), settings)
		if err != nil || len(alerts) != 0 {
			t.Fatalf("%s became an unsupported scarcity finding: alerts=%d error=%v", reason, len(alerts), err)
		}
	}
}

// A real Signal adapter and bounded native request, with no network access.
func selectionTestSettings(t testing.TB, raw string) SignalSettings {
	t.Helper()
	source := &providerHttpsTestSource{syntheticSource: &syntheticSource{}, do: func(request *http.Request) (*http.Response, error) {
		providerHttpsTestRequest(t, request, true)
		return providerHttpsTestResponse(raw), nil
	}}
	settings := syntheticSettings(source)
	settings.Environment, settings.Now = "synthetic", pickerTestNow
	settings.Hosts = []HostSettings{{Name: "api-synthetic", Roles: []string{"services"}}}
	settings.LogServices = []string{"api"}
	settings.LogServiceHosts = map[string][]string{"api": {"api-synthetic"}}
	settings.LogServiceBlocks = map[string][]string{"api": {"blue"}}
	providerHttpsTestCredentials(t, &settings)
	return settings
}

// The known invariant does not require assuming a global supply baseline.
func TestProviderSelectionSignalEligibleZeroPagesAndRedacts(t *testing.T) {
	settings := selectionTestSettings(t, pickerTestPayload(t, selectionTestRows("eligible_not_selected", 3)))
	alerts, err := NewProviderSelectionSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "provider-selection-empty-despite-eligible")
	if alert.Severity != SeverityPage || alert.Frame != "country/any/quality/default_minimum" {
		t.Fatalf("unexpected invariant severity/frame: %s %s", alert.Severity, alert.Frame)
	}
	for _, text := range []string{"requests=3", "scope_complete=true", "not the global online pool", "no customer, provider, network", "SIGNALS.md §2.9c"} {
		if !strings.Contains(alert.Markdown(), text) {
			t.Errorf("missing invariant qualification %q", text)
		}
	}
	for _, private := range []string{"private-selection-instance", "synthetic-private.example"} {
		if strings.Contains(alert.Markdown(), private) {
			t.Fatal("private source identity entered rendered alert")
		}
	}
}

// The online rank mode is fixed vocabulary: its invariant pages under its own
// frame, while an unlisted rank still fails closed.
func TestProviderSelectionSignalAcceptsOnlineRankMode(t *testing.T) {
	for _, rankMode := range []string{"online", "synthetic-rank"} {
		rows := selectionTestRows("eligible_not_selected", 3)
		for _, row := range rows {
			if labels := row["metric"].(map[string]string); labels["rank_mode"] != "" {
				labels["rank_mode"] = rankMode
			}
		}
		alerts, err := NewProviderSelectionSignal().Run(context.Background(), selectionTestSettings(t, pickerTestPayload(t, rows)))
		if err != nil {
			t.Fatal(err)
		}
		if rankMode == "online" {
			if alert := requireAlertClass(t, alerts, "provider-selection-empty-despite-eligible"); alert.Frame != "country/any/online/default_minimum" || len(alerts) != 1 {
				t.Fatalf("online cohort lost complete attribution: frame=%s alerts=%d", alert.Frame, len(alerts))
			}
		} else if len(alerts) != 1 || requireAlertClass(t, alerts, "provider-selection-unavailable").Frame != "" {
			t.Fatalf("unlisted rank mode was accepted: alerts=%d", len(alerts))
		}
	}
}

// A positive-count missing page is a cache consistency warning, not scarcity.
func TestProviderSelectionSignalCacheGapBoundary(t *testing.T) {
	for _, count := range []float64{19, 20} {
		alerts, err := NewProviderSelectionSignal().Run(context.Background(), selectionTestSettings(t, pickerTestPayload(t, selectionTestRows("cache_page_gap", count))))
		if err != nil {
			t.Fatal(err)
		}
		if count < 20 {
			if len(alerts) != 0 {
				t.Fatal("below-bound cache gap alerted")
			}
		} else if alert := requireAlertClass(t, alerts, "provider-selection-cache-page-gap"); alert.Severity != SeverityWarn || !strings.Contains(alert.Markdown(), "publication/expiry race") {
			t.Fatal("cache gap lost noncausal warning qualifier")
		}
	}
}

// Legitimate restrictive/intentional zeroes and positive lists remain visible
// as metrics but cannot be promoted into a new generic scarcity page.
func TestProviderSelectionSignalHealthyAndRestrictedControls(t *testing.T) {
	for _, reason := range []string{"returned", "intentional_zero", "no_specs", "direct_excluded", "cache_empty", "filtered_hard", "filtered_network", "filtered_family", "filtered_explicit", "filtered_mixed"} {
		alerts, err := NewProviderSelectionSignal().Run(context.Background(), selectionTestSettings(t, pickerTestPayload(t, selectionTestRows(reason, 500))))
		if err != nil || len(alerts) != 0 {
			t.Fatalf("%s manufactured scarcity or visibility error: alerts=%d err=%v", reason, len(alerts), err)
		}
	}
}

// Every source defect fails closed without echoing arbitrary metric text.
func TestProviderSelectionSignalUnavailableControls(t *testing.T) {
	for _, defect := range []string{"missing", "old-schema", "stale", "restart", "reset", "one-sample", "duplicate", "unknown-label"} {
		rows := selectionTestRows("eligible_not_selected", 3)
		if defect == "missing" {
			rows = nil
		}
		for _, row := range rows {
			labels := row["metric"].(map[string]string)
			field := labels["monitor_selection"]
			switch {
			case defect == "old-schema" && field == "prior_schema":
				row["value"].([]any)[1] = "0"
			case defect == "stale" && field == "count_time":
				row["value"].([]any)[1] = fmt.Sprint(pickerTestNow().Add(-91 * time.Second).Unix())
			case defect == "restart" && field == "now_start":
				row["value"].([]any)[1] = fmt.Sprint(pickerTestNow().Add(-time.Minute).Unix())
			case defect == "reset" && field == "resets":
				row["value"].([]any)[1] = "1"
			case defect == "one-sample" && field == "samples":
				row["value"].([]any)[1] = "1"
			case defect == "unknown-label" && field == "count":
				labels["target_kind"] = "country|region"
				labels["reason"] = "synthetic-private.example"
			}
		}
		if defect == "duplicate" {
			rows = append(rows, rows[0])
		}
		alerts, err := NewProviderSelectionSignal().Run(context.Background(), selectionTestSettings(t, pickerTestPayload(t, rows)))
		if err != nil {
			t.Fatal(err)
		}
		alert := requireAlertClass(t, alerts, "provider-selection-unavailable")
		if len(alerts) != 1 || strings.Contains(alert.Markdown(), "synthetic-private.example") {
			t.Fatalf("%s did not fail closed privately", defect)
		}
	}
}

// An incomplete fleet denominator must not erase a proven observed-subset
// failure, or let that subset certify the unobserved generation.
func TestProviderSelectionSignalPartialRetainsKnownFailure(t *testing.T) {
	settings := selectionTestSettings(t, pickerTestPayload(t, selectionTestRows("eligible_not_selected", 3)))
	settings.LogServiceBlocks["api"] = []string{"blue", "green"}
	alerts, err := NewProviderSelectionSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	requireAlertClass(t, alerts, "provider-selection-unavailable")
	alert := requireAlertClass(t, alerts, "provider-selection-empty-despite-eligible")
	if !strings.Contains(alert.Markdown(), "scope_complete=false") {
		t.Fatal("partial scope was described as complete")
	}
}

// Cancellation ends at the transport boundary rather than emitting health.
func TestProviderSelectionSignalCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := NewProviderSelectionSignal().Run(ctx, selectionTestSettings(t, pickerTestPayload(t, nil)))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation returned %v", err)
	}
}

// Quiet lazy counters otherwise accumulate for a process lifetime and exhaust
// the bounded source response. Filter every outcome witness, never authority.
func TestProviderSelectionQueryDropsQuietOutcomesKeepsAuthority(t *testing.T) {
	scope := providerPickerScope{hosts: []string{"api-synthetic"}, blocks: []string{"blue"}}
	query := providerSelectionQuery("synthetic", scope)
	seen := map[string]bool{}
	for _, part := range strings.Split(query, " or ") {
		for _, field := range []string{"count", "count_time", "resets", "samples", "now_start", "now_start_time", "prior_start", "prior_start_time", "now_schema", "now_schema_time", "prior_schema", "prior_schema_time"} {
			if !strings.Contains(part, `,"monitor_selection","`+field+`",`) {
				continue
			}
			seen[field] = true
			wantPositive := field == "count" || field == "count_time" || field == "resets" || field == "samples"
			if gotPositive := strings.Contains(part, "[5m]) > 0"); gotPositive != wantPositive {
				t.Errorf("%s positive-cohort filter=%t, want %t", field, gotPositive, wantPositive)
			}
		}
	}
	if len(seen) != 12 {
		t.Fatalf("query carries %d witness fields, want 12", len(seen))
	}
	// A fully quiet process still has its complete authority window. It does
	// not create a visibility warning or certify target-level supply.
	rows := selectionTestRows("returned", 0)[:8]
	alerts, err := NewProviderSelectionSignal().Run(context.Background(), selectionTestSettings(t, pickerTestPayload(t, rows)))
	if err != nil || len(alerts) != 0 {
		t.Fatalf("quiet complete authority became unavailable: alerts=%d err=%v", len(alerts), err)
	}
}

// Build a distinct synthetic process without borrowing another generation's witnesses.
func selectionGenerationTestRows(instance string, startAge time.Duration, bound, reason string, count float64) []map[string]any {
	rows := []map[string]any{}
	for _, row := range selectionTestRows(reason, count) {
		labels := row["metric"].(map[string]string)
		field := labels["monitor_selection"]
		if bound != "" && !strings.HasPrefix(field, bound+"_") {
			continue
		}
		labels["instance"] = instance
		if field == "now_start" || field == "prior_start" {
			row["value"].([]any)[1] = fmt.Sprint(pickerTestNow().Add(-startAge).Unix())
		}
		rows = append(rows, row)
	}
	return rows
}

// An old-enough now-only sibling cannot disappear behind a complete predecessor.
// Exercise both the parser and the public alert/observation consumer projection.
func TestProviderSelectionSignalReturnedGenerationPreventsCompleteCoverage(t *testing.T) {
	for _, count := range []float64{0, 3} {
		rows := selectionTestRows("eligible_not_selected", count)
		if count == 0 {
			rows = rows[:8]
		}
		rows = append(rows, selectionGenerationTestRows("private-replacement", 10*time.Minute, "now", "returned", 0)...)
		raw := pickerTestPayload(t, rows)
		settings := selectionTestSettings(t, raw)
		env, err := newProbeEnv(settings)
		if err != nil {
			t.Fatal(err)
		}
		evidence := parseProviderSelection(raw, "synthetic", pickerTestNow(), pickerScope(env))
		if evidence.complete || evidence.shapeComplete || evidence.paired != 1 || evidence.shapePaired != 1 {
			t.Fatalf("count=%.0f: returned incomplete generation disappeared from coverage", count)
		}
		var record ProviderSelectionObservation
		calls := 0
		settings.ProviderSelectionObserver = func(value ProviderSelectionObservation) error { record = value; calls++; return nil }
		alerts, err := NewProviderSelectionSignal().Run(context.Background(), settings)
		if err != nil || calls != 1 || record.SourceCoverageComplete || record.ShapeCoverageComplete || record.Reason == "complete" || record.PairedProcesses != 1 || record.ShapePairedProcesses != 1 || record.ExpectedSlots != 1 {
			t.Fatalf("count=%.0f: public consumer certified incomplete returned generation: calls=%d err=%v", count, calls, err)
		}
		visibility := requireAlertClass(t, alerts, "provider-selection-unavailable")
		if !isVisibilityAlert(visibility) || visibility.Severity != SeverityWarn || visibility.Sustain != 1 || visibility.Target != "api-fleet" || visibility.Frame != "" {
			t.Fatal("generation coverage changed visibility identity or severity")
		}
		if count == 3 {
			positive := requireAlertClass(t, alerts, "provider-selection-empty-despite-eligible")
			if positive.Severity != SeverityPage || positive.Sustain != 2 || !strings.Contains(positive.Markdown(), "scope_complete=false") || len(record.Outcomes) != 1 || record.Outcomes[0].Requests != 3 {
				t.Fatal("generation ambiguity erased independently proven subset pressure")
			}
		} else if len(alerts) != 1 || len(record.Outcomes) != 0 {
			t.Fatal("quiet qualified predecessor manufactured counts or pressure")
		}
		var output strings.Builder
		if err := record.WriteJsonl(&output); err != nil || strings.Contains(output.String(), "private-") || strings.Contains(visibility.Markdown(), "private-") {
			t.Fatal("private process identity escaped generation coverage projection")
		}
	}
}

// Pair eligibility is unchanged; uncertainty about a second identity never
// grants it counters or lets a valid predecessor certify unique slot ownership.
func TestProviderSelectionSignalUnpairedGenerationAuthorityControls(t *testing.T) {
	for _, defect := range []string{"young", "prior-only", "stale-now", "missing-start", "missing-schema", "reset", "schema-switch", "zero-schema", "unsupported-schema"} {
		other := selectionGenerationTestRows("private-unpaired", 10*time.Minute, "", "eligible_not_selected", 100)
		filtered := []map[string]any{}
		for _, row := range other {
			field := row["metric"].(map[string]string)["monitor_selection"]
			if defect == "prior-only" && !strings.HasPrefix(field, "prior_") || defect == "missing-start" && field == "now_start" || defect == "missing-schema" && field == "now_schema" {
				continue
			}
			switch {
			case defect == "young" && (field == "now_start" || field == "prior_start"):
				row["value"].([]any)[1] = fmt.Sprint(pickerTestNow().Add(-time.Minute).Unix())
			case defect == "stale-now" && field == "now_start_time":
				row["value"].([]any)[1] = fmt.Sprint(pickerTestNow().Add(-91 * time.Second).Unix())
			case defect == "reset" && field == "resets":
				row["value"].([]any)[1] = "1"
			case defect == "schema-switch" && field == "prior_schema":
				row["value"].([]any)[1] = "1"
			case defect == "zero-schema" && (field == "now_schema" || field == "prior_schema"):
				row["value"].([]any)[1] = "0"
			case defect == "unsupported-schema" && (field == "now_schema" || field == "prior_schema"):
				row["value"].([]any)[1] = "3"
			}
			filtered = append(filtered, row)
		}
		rows := append(selectionTestRows("eligible_not_selected", 3), filtered...)
		settings := selectionTestSettings(t, pickerTestPayload(t, rows))
		var record ProviderSelectionObservation
		settings.ProviderSelectionObserver = func(value ProviderSelectionObservation) error { record = value; return nil }
		alerts, err := NewProviderSelectionSignal().Run(context.Background(), settings)
		if err != nil || record.SourceCoverageComplete || record.ShapeCoverageComplete || record.PairedProcesses != 1 || len(record.Outcomes) != 1 || record.Outcomes[0].Requests != 3 {
			t.Fatalf("%s: unpaired generation gained authority or erased valid subset", defect)
		}
		requireAlertClass(t, alerts, "provider-selection-unavailable")
		if !strings.Contains(requireAlertClass(t, alerts, "provider-selection-empty-despite-eligible").Markdown(), "scope_complete=false") {
			t.Fatalf("%s: valid predecessor claimed complete scope", defect)
		}
	}
}

// A single valid old-enough replacement can qualify, but two distinct complete
// same-slot generations remain ambiguous rather than selecting an arbitrary newest.
func TestProviderSelectionSignalCompleteGenerationUniquenessControls(t *testing.T) {
	for _, both := range []bool{false, true} {
		rows := selectionGenerationTestRows("private-complete-replacement", 10*time.Minute, "", "returned", 0)[:8]
		if both {
			rows = append(rows, selectionTestRows("returned", 0)[:8]...)
		}
		settings := selectionTestSettings(t, pickerTestPayload(t, rows))
		var record ProviderSelectionObservation
		calls := 0
		settings.ProviderSelectionObserver = func(value ProviderSelectionObservation) error { record = value; calls++; return nil }
		alerts, err := NewProviderSelectionSignal().Run(context.Background(), settings)
		wantPaired := 1
		if both {
			wantPaired = 2
		}
		if err != nil || calls != 1 || record.SourceCoverageComplete == both || record.ShapeCoverageComplete == both || record.PairedProcesses != wantPaired || record.ShapePairedProcesses != wantPaired || len(record.Outcomes) != 0 {
			t.Fatalf("both=%t: complete generation uniqueness changed", both)
		}
		if both {
			requireAlertClass(t, alerts, "provider-selection-unavailable")
		} else if len(alerts) != 0 {
			t.Fatal("single complete quiet replacement became unavailable")
		}
	}
}

// Completeness is recomputed from each response, without carrying quiet or
// unavailable authority across repeated executions of the public consumer.
func TestProviderSelectionSignalGenerationCoverageProjectionTransitions(t *testing.T) {
	for _, partial := range []bool{false, true, true, false} {
		rows := selectionTestRows("returned", 0)[:8]
		if partial {
			rows = append(rows, selectionGenerationTestRows("private-partial", 10*time.Minute, "now", "returned", 0)...)
		}
		settings := selectionTestSettings(t, pickerTestPayload(t, rows))
		calls := 0
		settings.ProviderSelectionObserver = func(record ProviderSelectionObservation) error {
			calls++
			if record.SourceCoverageComplete == partial || record.ShapeCoverageComplete == partial || len(record.Outcomes) != 0 {
				t.Fatalf("partial=%t: consumer projected stale completeness", partial)
			}
			return nil
		}
		alerts, err := NewProviderSelectionSignal().Run(context.Background(), settings)
		if err != nil || calls != 1 {
			t.Fatal("generation transition lost public observation")
		}
		if partial {
			requireAlertClass(t, alerts, "provider-selection-unavailable")
		} else if len(alerts) != 0 {
			t.Fatal("complete transition retained prior ambiguity")
		}
	}
}

// Slot inventory is not interchangeable, and rejected identities invalidate
// the source rather than leaking labels or borrowing another slot's authority.
func TestProviderSelectionSignalGenerationScopeControls(t *testing.T) {
	for _, variant := range []string{"different-slot", "unowned", "duplicate-row", "rolling"} {
		rows := selectionTestRows("returned", 0)[:8]
		other := selectionGenerationTestRows("private-other", 10*time.Minute, "now", "returned", 0)
		if variant == "rolling" {
			rows = rows[4:8]
		}
		for _, row := range other {
			labels := row["metric"].(map[string]string)
			if variant == "different-slot" {
				labels["block"] = "green"
			}
			if variant == "unowned" {
				labels["host"] = "unowned-synthetic.example"
			}
		}
		rows = append(rows, other...)
		if variant == "duplicate-row" {
			rows = append(rows, rows[0])
		}
		settings := selectionTestSettings(t, pickerTestPayload(t, rows))
		if variant == "different-slot" {
			settings.LogServiceBlocks["api"] = []string{"blue", "green"}
		}
		var record ProviderSelectionObservation
		calls := 0
		settings.ProviderSelectionObserver = func(value ProviderSelectionObservation) error { record = value; calls++; return nil }
		alerts, err := NewProviderSelectionSignal().Run(context.Background(), settings)
		if err != nil || calls != 1 || record.SourceCoverageComplete || record.ShapeCoverageComplete || len(record.Outcomes) != 0 {
			t.Fatalf("%s: invalid scope/generation became complete", variant)
		}
		if variant == "different-slot" && (record.PairedProcesses != 1 || record.ExpectedSlots != 2) {
			t.Fatal("different expected slot borrowed predecessor authority")
		}
		if variant == "rolling" && record.PairedProcesses != 0 {
			t.Fatal("rolling generations borrowed each other's endpoint witnesses")
		}
		if (variant == "unowned" || variant == "duplicate-row") && record.Reason != "invalid-source-response" {
			t.Fatal("rejected identity/duplicate row changed fixed source error")
		}
		requireAlertClass(t, alerts, "provider-selection-unavailable")
	}
}

// A denied expected slot stays unknown without contacting it or dropping
// a separately qualified positive from the permitted slot.
func TestProviderSelectionSignalGenerationDeniedSlotKeepsUnknown(t *testing.T) {
	raw := pickerTestPayload(t, selectionTestRows("eligible_not_selected", 3))
	settings := selectionTestSettings(t, raw)
	settings.Hosts = append(settings.Hosts, HostSettings{Name: "denied-synthetic.example", Roles: []string{"services"}})
	settings.LogServiceHosts["api"] = append(settings.LogServiceHosts["api"], "denied-synthetic.example")
	settings.ExcludedHosts = []string{"denied-synthetic.example"}
	calls := 0
	settings.Source = &providerHttpsTestSource{syntheticSource: &syntheticSource{hostFn: func(HostSettings, string) (string, error) {
		t.Fatal("generation coverage attempted host contact")
		return "", errors.New("unexpected synthetic host contact")
	}}, do: func(request *http.Request) (*http.Response, error) {
		calls++
		providerHttpsTestRequest(t, request, true)
		if strings.Contains(request.URL.Query().Get("query"), "denied-synthetic") {
			t.Fatal("generation query included excluded host")
		}
		return providerHttpsTestResponse(raw), nil
	}}
	var record ProviderSelectionObservation
	settings.ProviderSelectionObserver = func(value ProviderSelectionObservation) error { record = value; return nil }
	alerts, err := NewProviderSelectionSignal().Run(context.Background(), settings)
	if err != nil || calls != 1 || record.SourceCoverageComplete || record.ShapeCoverageComplete || record.PairedProcesses != 1 || record.ExpectedSlots != 2 || len(record.Outcomes) != 1 || record.Outcomes[0].Requests != 3 {
		t.Fatal("denied slot became complete or erased independent permitted evidence")
	}
	requireAlertClass(t, alerts, "provider-selection-unavailable")
	if !strings.Contains(requireAlertClass(t, alerts, "provider-selection-empty-despite-eligible").Markdown(), "scope_complete=false") {
		t.Fatal("permitted subset certified denied slot")
	}
}

// Equal process starts do not merge identities. Neither source row ordering
// nor the public serializers may turn ambiguous coverage into a healthy scope.
func TestProviderSelectionSignalGenerationOrderAndPublicJson(t *testing.T) {
	for _, variant := range []string{"healthy", "quiet", "now-only", "missing-prior-start", "restart", "unsupported-schema", "stale-prior", "future-now", "stale-count", "reset", "one-sample", "same-start-complete", "same-start-unpaired", "duplicate-cell"} {
		rows := selectionTestRows("eligible_not_selected", 3)
		complete := variant == "healthy" || variant == "quiet"
		paired, requests, reason := 1, 3.0, "incomplete-process-window"
		if complete {
			reason = "complete"
		}
		if variant == "quiet" {
			rows, requests = rows[:8], 0
		}
		if !complete && variant != "duplicate-cell" {
			startAge, bound := 10*time.Minute, ""
			if variant == "same-start-complete" || variant == "same-start-unpaired" {
				startAge = time.Hour
			}
			if variant == "now-only" || variant == "same-start-unpaired" {
				bound = "now"
			}
			if variant == "same-start-complete" {
				paired, requests = 2, 7
			}
			for _, row := range selectionGenerationTestRows("private-generation-order", startAge, bound, "eligible_not_selected", 4) {
				field := row["metric"].(map[string]string)["monitor_selection"]
				if variant == "missing-prior-start" && field == "prior_start" {
					continue
				}
				switch {
				case variant == "restart" && field == "now_start":
					row["value"].([]any)[1] = fmt.Sprint(pickerTestNow().Add(-6 * time.Minute).Unix())
				case variant == "unsupported-schema" && (field == "now_schema" || field == "prior_schema"):
					row["value"].([]any)[1] = "3"
				case variant == "stale-prior" && field == "prior_schema_time":
					row["value"].([]any)[1] = fmt.Sprint(pickerTestNow().Add(-5*time.Minute - 91*time.Second).Unix())
				case variant == "future-now" && field == "now_start_time":
					row["value"].([]any)[1] = fmt.Sprint(pickerTestNow().Add(31 * time.Second).Unix())
				case variant == "stale-count" && field == "count_time":
					row["value"].([]any)[1] = fmt.Sprint(pickerTestNow().Add(-91 * time.Second).Unix())
				case variant == "reset" && field == "resets":
					row["value"].([]any)[1] = "1"
				case variant == "one-sample" && field == "samples":
					row["value"].([]any)[1] = "1"
				}
				rows = append(rows, row)
			}
		}
		if variant == "duplicate-cell" {
			rows = append(rows, rows[0])
			paired, requests, reason = 0, 0, "invalid-source-response"
		}
		var firstObservation, firstAlertJson, firstMarkdown string
		for _, order := range []string{"original", "reverse", "interleaved"} {
			orderedRows := append([]map[string]any(nil), rows...)
			if order == "reverse" {
				for i, j := 0, len(orderedRows)-1; i < j; i, j = i+1, j-1 {
					orderedRows[i], orderedRows[j] = orderedRows[j], orderedRows[i]
				}
			} else if order == "interleaved" {
				orderedRows = nil
				for offset := 0; offset < 2; offset++ {
					for i := offset; i < len(rows); i += 2 {
						orderedRows = append(orderedRows, rows[i])
					}
				}
			}
			settings := selectionTestSettings(t, pickerTestPayload(t, orderedRows))
			var observation, alertJson strings.Builder
			calls := 0
			settings.ProviderSelectionObserver = func(record ProviderSelectionObservation) error {
				calls++
				return record.WriteJsonl(&observation)
			}
			alerts, err := NewProviderSelectionSignal().Run(context.Background(), settings)
			if err != nil || calls != 1 {
				t.Fatalf("%s/%s: public generation observation failed", variant, order)
			}
			var fields map[string]any
			if json.Unmarshal([]byte(observation.String()), &fields) != nil {
				t.Fatalf("%s/%s: generation observation is not valid JSON", variant, order)
			}
			if fields["source_coverage_complete"] != complete || fields["shape_coverage_complete"] != complete || fields["paired_processes"] != float64(paired) || fields["shape_paired_processes"] != float64(paired) || fields["expected_slots"] != float64(1) || fields["reason"] != reason || fields["counts_scope"] != "observed-process-subset" {
				t.Fatalf("%s/%s: serialized generation authority changed", variant, order)
			}
			outcomes, ok := fields["outcomes"].([]any)
			if !ok || requests == 0 && len(outcomes) != 0 || requests > 0 && len(outcomes) != 1 {
				t.Fatalf("%s/%s: serialized generation outcome presence changed", variant, order)
			}
			wantAlerts := 0
			if !complete {
				wantAlerts++
				visibility := requireAlertClass(t, alerts, "provider-selection-unavailable")
				if !isVisibilityAlert(visibility) || visibility.Severity != SeverityWarn || visibility.Sustain != 1 || visibility.Identity() != "mimir/provider-selection|provider-selection-unavailable|api-fleet|" {
					t.Fatalf("%s/%s: generation ambiguity lost its visibility identity", variant, order)
				}
			}
			if requests > 0 {
				wantAlerts++
				outcome, ok := outcomes[0].(map[string]any)
				if !ok || outcome["requests"] != requests || outcome["reason"] != "eligible_not_selected" {
					t.Fatalf("%s/%s: valid subset was erased or unpaired counts gained authority", variant, order)
				}
				positive := requireAlertClass(t, alerts, "provider-selection-empty-despite-eligible")
				if positive.Severity != SeverityPage || positive.Sustain != 2 || !strings.Contains(positive.Markdown(), fmt.Sprintf("scope_complete=%t", complete)) || !strings.Contains(positive.Markdown(), fmt.Sprintf("requests=%.3f", requests)) {
					t.Fatalf("%s/%s: independent invariant lost its subset qualification", variant, order)
				}
			}
			if len(alerts) != wantAlerts || alerts.WriteJSONL(&alertJson) != nil {
				t.Fatalf("%s/%s: public alert projection changed", variant, order)
			}
			markdown := alerts.Markdown()
			if strings.Contains(observation.String()+alertJson.String()+markdown, "private-") {
				t.Fatalf("%s/%s: private generation identity escaped public output", variant, order)
			}
			if order == "original" {
				firstObservation, firstAlertJson, firstMarkdown = observation.String(), alertJson.String(), markdown
			} else if firstObservation != observation.String() || firstAlertJson != alertJson.String() || firstMarkdown != markdown {
				t.Fatalf("%s/%s: row order changed public generation authority", variant, order)
			}
		}
	}
}
