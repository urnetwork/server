// Synthetic request-boundary, source-authority and privacy controls for §2.9c.
package monitor

import (
	"context"
	"errors"
	"fmt"
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

// A real Signal adapter and bounded source command, with no network access.
func selectionTestSettings(t testing.TB, raw string) SignalSettings {
	t.Helper()
	source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		if host.Name != "api-synthetic" || !strings.Contains(command, "--max-time 15 --max-filesize 4194304") || !strings.Contains(command, "urnetwork_findproviders2_selection_schema_version") || !strings.Contains(command, "offset 5m") || !strings.Contains(command, "timestamp(") || !strings.Contains(command, "resets(") || !strings.Contains(command, `host=~"api-synthetic"`) {
			return "", errors.New("unexpected bounded selection source command")
		}
		return raw, nil
	}}
	settings := syntheticSettings(source)
	settings.Environment, settings.Now = "synthetic", pickerTestNow
	settings.Hosts = []HostSettings{{Name: "api-synthetic", Roles: []string{"services"}}}
	settings.LogServices = []string{"api"}
	settings.LogServiceHosts = map[string][]string{"api": {"api-synthetic"}}
	settings.LogServiceBlocks = map[string][]string{"api": {"blue"}}
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
