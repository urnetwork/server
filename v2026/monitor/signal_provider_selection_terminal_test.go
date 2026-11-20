// Pins independent populations during a rolling upgrade of the selector.
package monitor

import (
	"context"
	"strings"
	"testing"
)

func TestProviderSelectionTerminalEligibility(t *testing.T) {
	for _, tc := range []struct {
		schema int
		reason string
		class  string
	}{
		{schema: 3, reason: "eligible_not_selected", class: "provider-selection-empty-after-prefilter"},
		{schema: 4, reason: "drawable_not_selected", class: "provider-selection-empty-despite-eligible"},
		{schema: 4, reason: "no_drawable_candidates"},
		{schema: 3, reason: "drawable_not_selected", class: "provider-selection-unavailable"},
		{schema: 4, reason: "eligible_not_selected", class: "provider-selection-unavailable"},
	} {
		rows := selectionTestRowsAtSchema(tc.schema, tc.reason, 3)
		alerts, err := NewProviderSelectionSignal().Run(context.Background(), selectionTestSettings(t, pickerTestPayload(t, rows)))
		if err != nil {
			t.Fatal(err)
		}
		if tc.class == "" {
			if len(alerts) != 0 {
				t.Errorf("schema %d reason %s raised %d findings", tc.schema, tc.reason, len(alerts))
			}
			continue
		}
		wantCount := 1
		if tc.reason == "eligible_not_selected" && tc.schema == 3 {
			wantCount = 2
			coverage := requireAlertClass(t, alerts, "provider-selection-unavailable")
			if coverage.Frame != "terminal-draw" {
				t.Fatal("legacy producer certified terminal eligibility")
			}
		}
		alert := requireAlertClass(t, alerts, tc.class)
		if len(alerts) != wantCount || !strings.Contains(alert.Markdown(), tc.class) {
			t.Errorf("schema %d reason %s did not produce %s", tc.schema, tc.reason, tc.class)
		}
	}
}

func TestProviderSelectionMixedDrawSchemasDoNotCombine(t *testing.T) {
	newRows := selectionTestRowsAtSchema(4, "drawable_not_selected", 2)
	oldRows := selectionTestRowsAtSchema(3, "eligible_not_selected", 2)
	for _, row := range oldRows {
		row["metric"].(map[string]string)["block"] = "green"
	}
	rows := append(newRows, oldRows...)
	settings := selectionTestSettings(t, pickerTestPayload(t, rows))
	settings.LogServiceBlocks["api"] = []string{"blue", "green"}
	var record ProviderSelectionObservation
	settings.ProviderSelectionObserver = func(value ProviderSelectionObservation) error { record = value; return nil }
	alerts, err := NewProviderSelectionSignal().Run(context.Background(), settings)
	if err != nil || len(alerts) != 1 || alerts[0].Frame != "terminal-draw" || record.DrawCoverageComplete || record.DrawPairedProcesses != 1 || !record.SourceCoverageComplete || len(record.Outcomes) != 2 {
		t.Fatal("mixed schemas combined below-threshold evidence or certified terminal coverage")
	}
}
