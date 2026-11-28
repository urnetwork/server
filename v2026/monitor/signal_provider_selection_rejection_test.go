// Synthetic Mimir responses exercise the fixed rejection diagnostics of §2.9c.
package monitor

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// Each reply rejects the whole vector, including an otherwise valid positive cohort.
// No private sample, envelope annotation, label value or parser error may leave it.
func TestProviderSelectionSourceRejection(t *testing.T) {
	const private = "private-source-canary"
	for _, tc := range []struct{ variant, boundary string }{
		{variant: "json", boundary: "json-decode"},
		{variant: "status", boundary: "response-status"},
		{variant: "type", boundary: "result-type"},
		{variant: "warnings", boundary: "response-annotations"},
		{variant: "infos", boundary: "response-annotations"},
		{variant: "rows", boundary: "row-limit"},
		{variant: "sample", boundary: "sample-decode"},
		{variant: "time", boundary: "evaluation-time"},
		{variant: "value", boundary: "sample-value"},
		{variant: "scope", boundary: "process-scope"},
		{variant: "instance", boundary: "process-identity"},
		{variant: "cohort", boundary: "cohort-labels"},
		{variant: "reason", boundary: "outcome-reason"},
		{variant: "field", boundary: "series-field"},
		{variant: "duplicate", boundary: "duplicate-cell"},
	} {
		rows := selectionTestRows("drawable_not_selected", 3)
		last := selectionTestRows("returned", 1)[8]
		labels := last["metric"].(map[string]string)
		labels["instance"] = "private-second-instance"
		rows = append(rows, last)
		data := map[string]any{"resultType": "vector", "result": rows}
		envelope := map[string]any{"status": "success", "data": data}
		switch tc.variant {
		case "status":
			envelope["status"], envelope["error"] = "error", private
		case "type":
			data["resultType"] = private
		case "warnings", "infos":
			envelope[tc.variant] = []string{private}
		case "rows":
			data["result"] = make([]struct{}, 8193)
		case "sample":
			last["value"].([]any)[1] = private
		case "time":
			last["value"].([]any)[0] = pickerTestNow().Unix() - 1
		case "value":
			last["value"].([]any)[1] = "+Inf"
		case "scope":
			labels["host"] = private
		case "instance":
			delete(labels, "instance")
		case "cohort":
			labels["request_class"] = private
		case "reason":
			labels["reason"] = private
		case "field":
			labels["monitor_selection"] = private
		case "duplicate":
			data["result"] = append(rows, last)
		}
		body, err := json.Marshal(envelope)
		if err != nil {
			t.Fatal(err)
		}
		if tc.variant == "json" {
			body = []byte(private)
		}
		settings := selectionTestSettings(t, string(body))
		var record ProviderSelectionObservation
		var observation, alertJson strings.Builder
		settings.ProviderSelectionObserver = func(value ProviderSelectionObservation) error {
			record = value
			return record.WriteJsonl(&observation)
		}
		alerts, err := NewProviderSelectionSignal().Run(context.Background(), settings)
		if err != nil || len(alerts) != 1 {
			t.Fatalf("%s: wanted one visibility warning, got %d, error %v", tc.variant, len(alerts), err)
		}
		alert := alerts[0]
		if alert.Identity() != "mimir/provider-selection|provider-selection-unavailable|api-fleet|" || alert.Severity != SeverityWarn || alert.Sustain != 1 || !strings.Contains(alert.Observed, " reason=invalid-source-response ") {
			t.Errorf("%s changed visibility identity or reason: %s", tc.variant, alert.Observed)
		}
		if record.Reason != "invalid-source-response" || record.ExpectedSlots != 1 || record.PairedProcesses != 0 || record.ShapePairedProcesses != 0 || record.DrawPairedProcesses != 0 || record.SourceCoverageComplete || record.ShapeCoverageComplete || record.DrawCoverageComplete || len(record.Outcomes) != 0 {
			t.Errorf("%s retained source authority after rejection", tc.variant)
		}
		var wire struct {
			Rejection string `json:"source_rejection"`
		}
		if err := json.Unmarshal([]byte(observation.String()), &wire); err != nil {
			t.Fatal(err)
		}
		if wire.Rejection != tc.boundary || !strings.Contains(" "+alert.Observed+" ", " source_rejection="+tc.boundary+" ") {
			t.Errorf("%s: want source_rejection=%s in observation and warning, got %q and %q", tc.variant, tc.boundary, wire.Rejection, alert.Observed)
		}
		if err := alerts.WriteJSONL(&alertJson); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(observation.String()+alertJson.String()+alerts.Markdown(), "private-") {
			t.Errorf("%s leaked a private source value", tc.variant)
		}
	}
}

// A byte overflow normally stops at the HTTP reader, before the parser.
// Accepted quiet and unpaired vectors also have no parser rejection.
func TestProviderSelectionSourceRejectionBoundaries(t *testing.T) {
	rows := selectionTestRows("returned", 1)
	for _, tc := range []struct{ name, body, reason string }{
		{name: "quiet", body: pickerTestPayload(t, rows[:8]), reason: "complete"},
		{name: "partial", body: pickerTestPayload(t, rows[:1]), reason: "incomplete-process-window"},
		{name: "transport", body: strings.Repeat(" ", providerSelectionMaxBytes+1), reason: "bounded-source-unavailable"},
	} {
		settings := selectionTestSettings(t, tc.body)
		var observation strings.Builder
		settings.ProviderSelectionObserver = func(record ProviderSelectionObservation) error {
			if record.Reason != tc.reason {
				t.Errorf("%s: got reason %s, want %s", tc.name, record.Reason, tc.reason)
			}
			return record.WriteJsonl(&observation)
		}
		alerts, err := NewProviderSelectionSignal().Run(context.Background(), settings)
		if err != nil || observation.Len() == 0 || strings.Contains(observation.String()+alerts.Markdown(), "source_rejection") {
			t.Errorf("%s: parser rejection requires a rejected vector", tc.name)
		}
		if tc.name == "transport" {
			alert := requireAlertClass(t, alerts, "provider-selection-unavailable")
			if !strings.Contains(alert.Observed, "mimir_query_phase=response-bound") {
				t.Error("transport byte guard lost its own refusal phase")
			}
			env, err := newProbeEnv(settings)
			if err != nil {
				t.Fatal(err)
			}
			findings := providerSelectionFindings(parseProviderSelection(tc.body, "synthetic", pickerTestNow(), pickerScope(env)))
			if len(findings) != 1 || !strings.Contains(findings[0].observed, " source_rejection=byte-limit") {
				t.Error("parser byte guard lost its distinct refusal")
			}
		}
	}
}
