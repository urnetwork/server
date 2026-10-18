package monitor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

// Group-shaped fixtures exercise the incident boundary without inventing a
// private live group, caller, request identity or source-availability proof.
func selectionGroupTestRows(reason string, count float64) []map[string]any {
	rows := selectionTestRows(reason, count)
	for _, row := range rows {
		labels := row["metric"].(map[string]string)
		if labels["target_kind"] != "" {
			labels["target_kind"] = "group"
		}
	}
	return rows
}

// The actual bad boundary is retained below the alert threshold and warns at
// the existing cache diagnostic volume, without claiming absent providers.
func TestProviderSelectionMissingCacheObservation(t *testing.T) {
	for _, count := range []float64{1, 19, 20, 38} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			settings := selectionTestSettings(t, pickerTestPayload(t, selectionGroupTestRows("cache_missing", count)))
			var records []ProviderSelectionObservation
			settings.ProviderSelectionObserver = func(record ProviderSelectionObservation) error {
				records = append(records, record)
				return nil
			}
			alerts, err := NewProviderSelectionSignal().Run(context.Background(), settings)
			if err != nil || len(records) != 1 {
				t.Fatalf("missing metadata observation was dropped: records=%d err=%v", len(records), err)
			}
			record := records[0]
			if !record.SourceCoverageComplete || !record.ShapeCoverageComplete || record.Reason != "complete" || len(record.Outcomes) != 1 || record.Outcomes[0].Reason != "cache_missing" || record.Outcomes[0].Requests != count || record.Outcomes[0].TargetKind != "group" {
				t.Fatalf("wrong group metadata boundary: %+v", record)
			}
			if count < 20 {
				if len(alerts) != 0 {
					t.Fatal("below-bound diagnostic became an alert")
				}
				return
			}
			alert := requireAlertClass(t, alerts, "provider-selection-cache-missing")
			if alert.Severity != SeverityWarn || alert.Sustain != 2 || alert.Frame != "group/any/quality/default_minimum" || !strings.Contains(alert.Markdown(), "does not prove every target") || !strings.Contains(alert.Markdown(), "unrelated healthy groups") {
				t.Fatal("missing metadata warning lost its causal or recovery limits")
			}
		})
	}
}

// Positive responses and quiet processes must remain distinguishable; neither
// creates an absent cache_missing row or certifies an unobserved target.
func TestProviderSelectionObservationNonemptyAndQuietControls(t *testing.T) {
	for _, quiet := range []bool{false, true} {
		rows := selectionGroupTestRows("returned_10_plus", 80)
		if quiet {
			rows = rows[:8]
		}
		settings := selectionTestSettings(t, pickerTestPayload(t, rows))
		var record ProviderSelectionObservation
		calls := 0
		settings.ProviderSelectionObserver = func(value ProviderSelectionObservation) error { record = value; calls++; return nil }
		alerts, err := NewProviderSelectionSignal().Run(context.Background(), settings)
		if err != nil || len(alerts) != 0 || calls != 1 || !record.SourceCoverageComplete {
			t.Fatalf("complete source control failed: quiet=%t alerts=%d calls=%d err=%v", quiet, len(alerts), calls, err)
		}
		if quiet && len(record.Outcomes) != 0 || !quiet && (len(record.Outcomes) != 1 || record.Outcomes[0].Outcome != "nonempty" || record.Outcomes[0].Requests != 80) {
			t.Fatal("quiet labels became zero-count rows or nonempty traffic was dropped")
		}
		if !record.ObservedAt.Equal(pickerTestNow()) || !record.WindowStart.Equal(pickerTestNow().Add(-5*time.Minute)) || record.WindowSeconds != 300 || record.CountsScope != "observed-process-subset" {
			t.Fatal("observation lost its exact evaluation window or count qualifier")
		}
	}
}

// The source cannot turn unknown evidence into a complete empty distribution.
// Valid observed-subset failures remain visible alongside the unknown fleet.
func TestProviderSelectionObservationUnknownAndPartialControls(t *testing.T) {
	for _, defect := range []string{"missing", "stale", "reset", "one-sample", "partial", "malformed", "transport", "inventory"} {
		t.Run(defect, func(t *testing.T) {
			rows := selectionGroupTestRows("cache_missing", 38)
			if defect == "missing" {
				rows = nil
			}
			for _, row := range rows {
				field := row["metric"].(map[string]string)["monitor_selection"]
				switch {
				case defect == "stale" && field == "count_time":
					row["value"].([]any)[1] = fmt.Sprint(pickerTestNow().Add(-91 * time.Second).Unix())
				case defect == "reset" && field == "resets":
					row["value"].([]any)[1] = "1"
				case defect == "one-sample" && field == "samples":
					row["value"].([]any)[1] = "1"
				}
			}
			raw := pickerTestPayload(t, rows)
			if defect == "malformed" {
				raw = "private-invalid-source"
			}
			settings := selectionTestSettings(t, raw)
			if defect == "partial" {
				settings.LogServiceBlocks["api"] = []string{"blue", "green"}
			}
			if defect == "transport" {
				settings.Source = &syntheticSource{hostFn: func(HostSettings, string) (string, error) { return "", errors.New("private-transport-detail") }}
			}
			if defect == "inventory" {
				settings.LogServiceBlocks = nil
			}
			var record ProviderSelectionObservation
			calls := 0
			settings.ProviderSelectionObserver = func(value ProviderSelectionObservation) error { record = value; calls++; return nil }
			alerts, err := NewProviderSelectionSignal().Run(context.Background(), settings)
			if err != nil || calls != 1 || record.SourceCoverageComplete || record.ShapeCoverageComplete || record.Reason == "complete" {
				t.Fatalf("unknown became complete or was dropped: calls=%d err=%v record=%+v", calls, err, record)
			}
			requireAlertClass(t, alerts, "provider-selection-unavailable")
			if defect == "partial" {
				requireAlertClass(t, alerts, "provider-selection-cache-missing")
				if len(record.Outcomes) != 1 || record.Outcomes[0].Requests != 38 || record.PairedProcesses != 1 || record.ExpectedSlots != 2 {
					t.Fatal("known partial failure was lost")
				}
			} else if len(record.Outcomes) != 0 {
				t.Fatal("rejected source produced request counts")
			}
			var output bytes.Buffer
			if err := record.WriteJsonl(&output); err != nil || strings.Contains(output.String(), "private-") {
				t.Fatal("observation leaked source text or failed serialization")
			}
		})
	}
}

// Existing request-intent carve-outs suppress only the new warning; their
// actual reason partitions are still retained for diagnosis.
func TestProviderSelectionMissingCacheIntentCarveouts(t *testing.T) {
	for _, requestClass := range []string{"count_zero", "forced_minimum"} {
		rows := selectionGroupTestRows("cache_missing", 500)
		for _, row := range rows {
			labels := row["metric"].(map[string]string)
			if labels["request_class"] != "" {
				labels["request_class"] = requestClass
			}
		}
		settings := selectionTestSettings(t, pickerTestPayload(t, rows))
		calls := 0
		settings.ProviderSelectionObserver = func(record ProviderSelectionObservation) error {
			calls++
			if len(record.Outcomes) != 1 || record.Outcomes[0].RequestClass != requestClass {
				t.Fatal("carved-out observation was discarded")
			}
			return nil
		}
		alerts, err := NewProviderSelectionSignal().Run(context.Background(), settings)
		if err != nil || len(alerts) != 0 || calls != 1 {
			t.Fatal("request-intent carve-out changed the alert threshold or lost observation")
		}
	}
}

// All reason partitions survive serialization in a stable order; private
// process identities and arbitrary source labels never leave the parser.
func TestProviderSelectionObservationRoundTripAndOutputFailure(t *testing.T) {
	rows := selectionGroupTestRows("cache_missing", 38)
	rows = append(rows, selectionGroupTestRows("returned_10_plus", 80)[8:]...)
	rows = append(rows, selectionGroupTestRows("filtered_network", 4)[8:]...)
	settings := selectionTestSettings(t, pickerTestPayload(t, rows))
	var output bytes.Buffer
	settings.ProviderSelectionObserver = func(record ProviderSelectionObservation) error { return record.WriteJsonl(&output) }
	alerts, err := NewProviderSelectionSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	requireAlertClass(t, alerts, "provider-selection-cache-missing")
	if strings.Contains(output.String(), "private-selection-instance") || strings.Contains(output.String(), "api-synthetic") || strings.Contains(output.String(), "blue") {
		t.Fatal("private process identity reached the observation")
	}
	var record ProviderSelectionObservation
	decoder := json.NewDecoder(&output)
	if err := decoder.Decode(&record); err != nil || len(record.Outcomes) != 3 || record.Outcomes[0].Outcome != "nonempty" || record.Outcomes[1].Reason != "cache_missing" || record.Outcomes[2].Reason != "filtered_network" {
		t.Fatal("reason distribution was lost or reordered")
	}
	if err := decoder.Decode(&record); err != io.EOF {
		t.Fatal("one execution emitted multiple records")
	}
	settings.ProviderSelectionObserver = func(ProviderSelectionObservation) error { return errors.New("private-writer-detail") }
	alerts, err = NewProviderSelectionSignal().Run(context.Background(), settings)
	if err == nil || strings.Contains(err.Error(), "private-") {
		t.Fatal("writer failure was hidden or leaked")
	}
	requireAlertClass(t, alerts, "provider-selection-cache-missing")
	if err := record.WriteJsonl(shortNilWriter{}); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("partial observation write passed: %v", err)
	}
}

// Optional local output must not alter the effective generation or run during
// comparison; real credential changes must still be detected.
func TestProviderSelectionObserverIsProcessOnly(t *testing.T) {
	settings, current := settingsFreshnessFixture(t)
	calls := 0
	settings.ProviderSelectionObserver = func(ProviderSelectionObservation) error { calls++; return nil }
	current.ProviderSelectionObserver = func(ProviderSelectionObservation) error { calls++; return nil }
	alerts, err := NewSettingsFreshnessSignal().Run(context.Background(), settings)
	if err != nil || len(alerts) != 0 || calls != 0 {
		t.Fatal("selection observer changed the generation or ran during comparison")
	}
	current.PostgreSQL.Password = "private-new-password"
	alerts, err = NewSettingsFreshnessSignal().Run(context.Background(), settings)
	if err != nil || calls != 0 {
		t.Fatal("real settings change failed comparison")
	}
	requireAlertClass(t, alerts, "settings-generation-stale")
}
