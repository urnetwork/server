package monitor

import (
	"context"
	"strings"
	"testing"
)

// A processor diagnostic precedes the controller's typed-error check and
// guarded reset. Missing evaluator evidence cannot certify zero attempts.
func TestPayoutInvalidDestinationProviderOnlyKeepsAttemptCountUnknown(t *testing.T) {
	const response = `Bad status: 400 Bad Request {"code":155219,"message":"Invalid destination address."}`
	const prefix = `[synthetic-host][taskworker][synthetic-block][cid:fixture][I]`
	first := prefix + `[2026-01-01T12:00:00Z][circle_client_controller.go:225][circlec]error sending payment: ` + response
	second := prefix + `[2026-01-01T12:00:01Z][circle_client_controller.go:225][circlec]error sending payment: ` + response
	source := &syntheticSource{localFn: func(_ string, args ...string) (string, error) {
		if len(args) > 1 && args[0] == "ls" {
			return "repo names synthetic-taskworker", nil
		}
		return first + "\n" + second + "\n", nil
	}}
	alerts, err := NewLogErrorsSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "payout-invalid-destination")
	if alert.Severity != SeverityWarn || alert.SignalID != "logs/payout-invalid-destination" || alert.Target != "taskworker" || alert.Frame != "" {
		t.Error("provider-only rejection changed its warning identity")
	}
	markdown := alert.Markdown()
	for _, want := range []string{
		"rate=2/min",
		"diagnostic_lines=2",
		"invalid_destination_events=unknown",
		"canonical_source=absent",
		"logical event count: unknown; no task evaluator line was present",
		"does not prove typed rejection, current chain compatibility, or guarded-reset outcome",
		"HTTP 400 with parsed code 155219 or the exact normalized invalid-address message",
	} {
		if !strings.Contains(markdown, want) {
			t.Errorf("provider-only destination warning missing %q", want)
		}
	}
	requireAlertOmits(t, alert,
		"invalid_destination_events=0", "logical event count: 0",
		"because the configured wallet address is invalid for its declared chain")
	if requireAlertClassCount(alerts, "payout-invalid-destination-reset-failed") != 0 {
		t.Error("a pre-reset processor diagnostic manufactured a reset failure")
	}
}

// A complete synthetic healthy window does not manufacture a warning merely
// because the canonical event source is absent.
func TestPayoutInvalidDestinationHealthyWindowRemainsQuiet(t *testing.T) {
	source := &syntheticSource{localFn: func(_ string, args ...string) (string, error) {
		if len(args) > 1 && args[0] == "ls" {
			return "repo names synthetic-taskworker", nil
		}
		return "", nil
	}}
	alerts, err := NewLogErrorsSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	if requireAlertClassCount(alerts, "payout-invalid-destination") != 0 || requireAlertClassCount(alerts, "payout-invalid-destination-reset-failed") != 0 {
		t.Error("healthy window manufactured a destination or reset-failure warning")
	}
}
