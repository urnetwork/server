// Exercise failed bounded reads through the public one-shot monitor path.
package monitor

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// Keep discovery and transport synthetic while preserving the real adapter.
func runOneShotLogOutput(t *testing.T, ctx context.Context, output string, sourceErr error, cancel context.CancelFunc) (Alerts, error) {
	t.Helper()
	calls := 0
	source := &syntheticSource{localFn: func(name string, args ...string) (string, error) {
		calls++
		want := []string{"logs", "synthetic", "fixture-service", "--since=1m", "--limit=10000"}
		if name != "warpctl" || !reflect.DeepEqual(args, want) {
			t.Fatal("bounded log test attempted an unexpected source operation")
		}
		if cancel != nil {
			cancel()
		}
		return output, sourceErr
	}}
	settings := syntheticSettings(source)
	settings.LogServices = []string{"fixture-service"}
	alerts, err := NewWithSignals(settings, NewLogErrorsSignal()).Run(ctx)
	if calls != 1 {
		t.Fatal("bounded log test did not perform exactly one selected read")
	}
	return alerts, err
}

// An observation failure must never turn helper bytes into service findings.
func requireOneShotLogVisibility(t *testing.T, alerts Alerts, err, sourceErr error, errorClass string, privateValues ...string) {
	t.Helper()
	if err == nil || !errors.Is(err, sourceErr) {
		t.Error("failed bounded read did not preserve its command error")
	}
	if len(alerts) != 1 {
		t.Fatal("failed bounded read did not produce exactly one visibility alert")
	}
	alert := alerts[0]
	if alert.SignalID != "monitor/visibility" || alert.Class != "cannot-observe" ||
		alert.Severity != SeverityWarn || alert.SignalNumber != "1.5" ||
		alert.SignalKey != "log-errors" || alert.Target != "logs/error-classes" || alert.Sustain != 2 {
		t.Error("failed bounded read changed visibility identity or became a service failure")
	}
	if !strings.Contains(alert.Markdown(), "error_class="+errorClass) {
		t.Error("failed bounded read lost its fixed observation error class")
	}
	requireAlertOmits(t, alert, privateValues...)
}

// Local failure diagnostics can satisfy the remote panic threshold if parsed.
func TestLogErrorsOneShotRejectsFailedPanicDiagnostics(t *testing.T) {
	const privateDetail = "synthetic-private-helper-detail"
	output := strings.Repeat("panic: "+privateDetail+"\n", 5)
	sourceErr := errors.New("exit status 2: " + privateDetail)
	alerts, err := runOneShotLogOutput(t, context.Background(), output, sourceErr, nil)
	requireOneShotLogVisibility(t, alerts, err, sourceErr, observationErrorClassCommandFailed, privateDetail, "panic:")
}

// Non-error-shaped bytes and a partial remote prefix are not complete windows.
func TestLogErrorsOneShotRejectsFailedPartialOutput(t *testing.T) {
	const privateDetail = "synthetic-private-partial-detail"
	for _, output := range []string{
		"helper started " + privateDetail + "\n",
		"[fixture-service][I] ordinary remote record\n" + privateDetail,
	} {
		sourceErr := errors.New("exit status 2: " + privateDetail)
		alerts, err := runOneShotLogOutput(t, context.Background(), output, sourceErr, nil)
		requireOneShotLogVisibility(t, alerts, err, sourceErr, observationErrorClassCommandFailed, privateDetail)
	}
}

// The existing empty-failure branch retains its same public identity and error.
func TestLogErrorsOneShotPreservesEmptyFailure(t *testing.T) {
	const privateDetail = "synthetic-private-empty-failure"
	sourceErr := errors.New("exit status 2: " + privateDetail)
	alerts, err := runOneShotLogOutput(t, context.Background(), "", sourceErr, nil)
	requireOneShotLogVisibility(t, alerts, err, sourceErr, observationErrorClassCommandFailed, privateDetail)
}

// A completed empty range remains a legitimate zero-alert diagnostic control.
func TestLogErrorsOneShotAcceptsSuccessfulEmptyWindow(t *testing.T) {
	alerts, err := runOneShotLogOutput(t, context.Background(), "", nil, nil)
	if err != nil || len(alerts) != 0 {
		t.Fatal("successful empty bounded log window became an observation failure")
	}
}

// The fix must not suppress genuine service failures from a successful read.
func TestLogErrorsOneShotPreservesSuccessfulRemotePanic(t *testing.T) {
	output := strings.Repeat("[fixture-service][E] panic: synthetic remote failure\n", 5)
	alerts, err := runOneShotLogOutput(t, context.Background(), output, nil, nil)
	if err != nil || len(alerts) != 1 {
		t.Fatal("successful remote panic window lost its one service alert")
	}
	alert := alerts[0]
	if alert.SignalID != "logs/panic" || alert.Class != "panic" || alert.Target != "fixture-service" ||
		alert.Severity != SeverityPage || alert.Sustain != 1 || !strings.Contains(alert.Observed, "rate=5/min") {
		t.Fatal("successful remote panic window changed its threshold, severity, or identity")
	}
}

// One-shot cancellation propagates as an error; RunLoop separately owns drain suppression.
func TestLogErrorsOneShotPropagatesCancellationWithOutput(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	const privateDetail = "synthetic-private-canceled-helper"
	alerts, err := runOneShotLogOutput(t, ctx, strings.Repeat("panic: "+privateDetail+"\n", 5), context.Canceled, cancel)
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatal("synthetic source did not cancel the one-shot context")
	}
	requireOneShotLogVisibility(t, alerts, err, context.Canceled, observationErrorClassCanceled, privateDetail, "panic:")
}
