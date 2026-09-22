package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestRouterConfigHealthyAndDriftUseBothCaptures(t *testing.T) {
	now := time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC)
	source := newSyntheticRouterSource()
	source.body = "--running--\nsystem { host-name router-test }\n--saved--\nsystem { host-name router-test }"
	settings := syntheticRouterSettings(source, &now)
	alerts, err := NewRouterConfigSignal().Run(context.Background(), settings)
	if err != nil || len(alerts) != 0 {
		t.Fatalf("valid equal captures: alerts=%d error=%v", len(alerts), err)
	}
	source.summary = strings.Replace(syntheticRouterSummary, `"saved":{"complete":true,"changes":0,"deletes":0,"sets":0`, `"saved":{"complete":true,"changes":1,"deletes":1,"sets":0`, 1)
	alerts, err = NewRouterConfigSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "router-config-drift")
	if alert.Frame != "saved" || !strings.Contains(alert.Observed, "changes=1") {
		t.Fatal("saved-only drift lost its layer/count authority")
	}
	requireRouterPrivate(t, alerts)
}

func TestRouterConfigIncompleteCapturesStayUnknown(t *testing.T) {
	now := time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC)
	for index := 0; index < 7; index++ {
		source := newSyntheticRouterSource()
		source.body = "--running--\nsystem { host-name router-test }\n--saved--\nsystem { host-name router-test }"
		switch index {
		case 0:
			source.missingEnd = true
		case 1:
			source.changedBoot = true
		case 2:
			source.err = errors.New("provider hostile synthetic-secret 192.0.2.77")
		case 3:
			source.summary = strings.Replace(syntheticRouterSummary, `"unverified":0`, `"unverified":1`, 1)
		case 4:
			source.summary = strings.Replace(syntheticRouterSummary, `"running":{"complete":true`, `"running":{"complete":false`, 1)
		case 5:
			source.summary = syntheticRouterSummary + "{}"
		case 6:
			source.body = "--running--\n" + strings.Repeat("x", routerConfigByteLimit+1) + "\n--saved--\nsystem { host-name router-test }"
		}
		alerts, err := NewRouterConfigSignal().Run(context.Background(), syntheticRouterSettings(source, &now))
		if err != nil {
			t.Fatalf("control %d: %v", index, err)
		}
		requireAlertClass(t, alerts, "cannot-observe")
		requireRouterPrivate(t, alerts)
	}
}

func TestRouterConfigProtectedDriftIsNotHealthyRefusal(t *testing.T) {
	now := time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC)
	source := newSyntheticRouterSource()
	source.body = "--running--\nsystem { host-name router-test }\n--saved--\nsystem { host-name router-test }"
	source.summary = strings.Replace(syntheticRouterSummary, `"running":{"complete":true,"changes":0,"deletes":0,"sets":0,"unverified":0,"protected_delete":false`, `"running":{"complete":true,"changes":1,"deletes":1,"sets":0,"unverified":0,"protected_delete":true`, 1)
	alerts, err := NewRouterConfigSignal().Run(context.Background(), syntheticRouterSettings(source, &now))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "router-config-drift")
	if !strings.Contains(alert.Observed, "protected_delete=true") {
		t.Fatal("protected drift flag was lost")
	}
	requireRouterPrivate(t, alerts)
}

func TestRouterConfigUnconfiguredIsNotConverged(t *testing.T) {
	settings := syntheticSettings(&syntheticSource{})
	alerts, err := NewRouterConfigSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	requireAlertClass(t, alerts, "router-observation-unconfigured")
}

func TestRouterConfigKnownDriftSurvivesConcealedValues(t *testing.T) {
	now := time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC)
	source := newSyntheticRouterSource()
	source.body = "--running--\nsystem { host-name router-test }\n--saved--\nsystem { host-name router-test }"
	source.summary = strings.Replace(syntheticRouterSummary, `"running":{"complete":true,"changes":0,"deletes":0,"sets":0,"unverified":0,"protected_delete":false,"reason":"compared"}`, `"running":{"complete":false,"changes":1,"deletes":0,"sets":1,"unverified":1,"protected_delete":false,"reason":"concealed-values"}`, 1)
	alerts, err := NewRouterConfigSignal().Run(context.Background(), syntheticRouterSettings(source, &now))
	if err != nil {
		t.Fatal(err)
	}
	requireAlertClass(t, alerts, "router-config-drift")
	requireAlertClass(t, alerts, "cannot-observe")
	if len(alerts) != 2 {
		t.Fatal("known drift and concealed uncertainty were not independently retained")
	}
	source.summary = strings.Replace(source.summary, "concealed-values", "input-shape-incomplete", 1)
	alerts, err = NewRouterConfigSignal().Run(context.Background(), syntheticRouterSettings(source, &now))
	if err != nil || len(alerts) != 1 || alerts[0].Class != "cannot-observe" {
		t.Fatal("partial input counts were accepted as concrete drift")
	}
}

// Fixed producer states keep known structural counts independent of whether
// management-uplink protection can be identified from the complete capture.
func syntheticRouterComparisonSource(t *testing.T, layer string, comparison routerComparison) *syntheticRouterSource {
	t.Helper()
	source := newSyntheticRouterSource()
	source.body = "--running--\nsystem { host-name router-test description synthetic-secret }\n--saved--\nsystem { host-name router-test description synthetic-secret }"
	var summary routerSummary
	if err := json.Unmarshal([]byte(syntheticRouterSummary), &summary); err != nil {
		t.Fatal("synthetic router summary could not be decoded")
	}
	switch layer {
	case "running":
		summary.Running = comparison
	case "saved":
		summary.Saved = comparison
	default:
		t.Fatal("synthetic comparison layer is invalid")
	}
	summary.Topology.Reason = "derived"
	summary.Complete = summary.Running.Complete && summary.Saved.Complete && summary.Topology.Complete
	summary.Reason = "complete"
	if !summary.Complete {
		summary.Reason = "incomplete"
	}
	encoded, err := json.Marshal(summary)
	if err != nil {
		t.Fatal("synthetic router summary could not be encoded")
	}
	source.summary = string(encoded)
	return source
}

// The Warp helper computes this leaf replacement before discovering missing
// uplink protection: one delete plus one set remains known drift, not equality.
func TestRouterConfigProtectionUnavailableKeepsKnownDrift(t *testing.T) {
	now := time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC)
	for _, layer := range []string{"running", "saved"} {
		source := syntheticRouterComparisonSource(t, layer, routerComparison{
			Changes: 2, Deletes: 1, Sets: 1, ProtectedDelete: true, Reason: "protection-unavailable",
		})
		alerts, err := NewRouterConfigSignal().Run(context.Background(), syntheticRouterSettings(source, &now))
		if err != nil {
			t.Fatalf("%s: public observation failed: %v", layer, err)
		}
		requireRouterPrivate(t, alerts)
		drift := requireAlertClass(t, alerts, "router-config-drift")
		unknown := requireAlertClass(t, alerts, "cannot-observe")
		if len(alerts) != 2 || drift.Frame != layer || unknown.Frame != layer || drift.Target != syntheticRouterName+"/router-config" || unknown.Target != drift.Target {
			t.Fatalf("%s: known drift and protection uncertainty lost their independent same-layer authority", layer)
		}
		if drift.SignalID != "router/config" || drift.Severity != SeverityWarn || drift.Sustain != 2 || !strings.Contains(drift.Observed, "changes=2 deletes=1 sets=1 unverified=0 protected_delete=true") {
			t.Fatalf("%s: structural drift identity, counts or sustain changed", layer)
		}
		if !strings.Contains(unknown.Observed, "observation_state=comparison-unverified") {
			t.Fatalf("%s: unknown management protection was dropped", layer)
		}
	}
}

// No structural change does not establish safe protection or full equality.
func TestRouterConfigProtectionUnavailableZeroCountsStayUnknown(t *testing.T) {
	now := time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC)
	for _, layer := range []string{"running", "saved"} {
		source := syntheticRouterComparisonSource(t, layer, routerComparison{Reason: "protection-unavailable"})
		alerts, err := NewRouterConfigSignal().Run(context.Background(), syntheticRouterSettings(source, &now))
		if err != nil || len(alerts) != 1 || alerts[0].Class != "cannot-observe" || alerts[0].Frame != layer {
			t.Fatalf("%s: zero drift with unknown protection became healthy or fabricated drift", layer)
		}
		requireRouterPrivate(t, alerts)
	}
}

// Only producer states establishing a completed structural diff authorize
// counts; an invalid or partial input cannot supply positive drift evidence.
func TestRouterConfigInvalidComparisonCountsStayUnknown(t *testing.T) {
	now := time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC)
	for _, reason := range []string{"input-shape-incomplete", "input-invalid", "comparison-unavailable"} {
		source := syntheticRouterComparisonSource(t, "running", routerComparison{
			Changes: 2, Deletes: 1, Sets: 1, ProtectedDelete: true, Reason: reason,
		})
		alerts, err := NewRouterConfigSignal().Run(context.Background(), syntheticRouterSettings(source, &now))
		if err != nil || len(alerts) != 1 || alerts[0].Class != "cannot-observe" || alerts[0].Frame != "running" {
			t.Fatalf("%s: nonauthoritative counts became concrete drift", reason)
		}
		requireRouterPrivate(t, alerts)
	}
}

// Complete visible equality remains independently healthy for both layers.
func TestRouterConfigCompleteEqualityControl(t *testing.T) {
	now := time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC)
	source := syntheticRouterComparisonSource(t, "running", routerComparison{Complete: true, Reason: "compared"})
	alerts, err := NewRouterConfigSignal().Run(context.Background(), syntheticRouterSettings(source, &now))
	if err != nil || len(alerts) != 0 || source.hostCalls != 1 || source.localCalls != 2 {
		t.Fatal("complete visible equality lost its bounded capture/comparison contract")
	}
}
