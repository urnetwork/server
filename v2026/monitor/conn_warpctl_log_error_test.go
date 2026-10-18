// Local child fixtures preserve the real runner, alert, and reconciliation path
// without executing Warpctl or contacting an observation backend.
package monitor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

func TestWarpctlLogQueryFailureClassesArePrivate(t *testing.T) {
	const privateText = "fixture-secret https://private.example/query?token=fixture-token 192.0.2.77"
	childErr := errors.New("exit status 2: " + privateText)
	for _, test := range []struct {
		name   string
		stderr string
		class  string
	}{
		{name: "http429", stderr: "panic: Loki query error (429): " + privateText, class: "observation-log-query-http-429"},
		{name: "http502", stderr: "panic: Loki query error (502): " + privateText, class: "observation-log-query-http-502"},
		{name: "http503", stderr: "panic: Loki query error (503): " + privateText, class: "observation-log-query-http-503"},
		{name: "http504", stderr: "panic: Loki query error (504): " + privateText, class: "observation-log-query-http-504"},
		{name: "transport-timeout", stderr: "panic: Get \"https://private.example/query\": context deadline exceeded (Client.Timeout exceeded while awaiting headers) " + privateText, class: observationErrorClassTimeout},
		{name: "yaml-schema", stderr: "panic: yaml: unmarshal errors:\n  line 1: cannot unmarshal " + privateText, class: "observation-log-query-config-schema"},
		{name: "json-schema", stderr: "panic: json: cannot unmarshal string into Go struct field " + privateText, class: "observation-log-query-config-schema"},
		{name: "json-syntax", stderr: "panic: invalid character '<' looking for beginning of value " + privateText, class: "observation-log-query-config-schema"},
		{name: "unknown", stderr: "panic: " + privateText, class: observationErrorClassCommandFailed},
		{name: "remote-stderr-record", stderr: "[fixture-service][E] panic: Loki query error (502): " + privateText, class: observationErrorClassCommandFailed},
		{name: "retry-without-terminal-evidence", stderr: "2026/01/01 12:00:00 client.go:1: Loki query attempt 1/3 failed (Loki query error (502): " + privateText + "). Retrying in 1s.", class: observationErrorClassCommandFailed},
		{name: "earlier-retry-is-not-terminal-cause", stderr: "2026/01/01 12:00:00 client.go:1: Loki query attempt 1/3 failed (Loki query error (502): retry). Retrying in 1s.\npanic: yaml: unmarshal errors: " + privateText, class: "observation-log-query-config-schema"},
		{name: "unknown-http-body-cannot-spoof-timeout", stderr: "panic: Loki query error (500): context deadline exceeded " + privateText, class: observationErrorClassCommandFailed},
		{name: "multiple-panic-headers-are-ambiguous", stderr: "panic: Loki query error (502): response\npanic: yaml: unmarshal errors", class: observationErrorClassCommandFailed},
		{name: "stdout-is-not-local-diagnostic", class: observationErrorClassCommandFailed},
		{name: "quoted-url-is-not-timeout-evidence", stderr: "panic: Get \"https://private.example/context deadline exceeded\": EOF", class: observationErrorClassCommandFailed},
		{name: "oversized-diagnostics", stderr: "panic: Loki query error (502): " + strings.Repeat("x", warpctlLogDiagnosticLimit), class: observationErrorClassCommandFailed},
	} {
		runner := newRunner(&monitorConfig{commandTimeout: time.Minute})
		const output = "[fixture-service][E] panic: Loki query error (504): private remote record"
		runner.runLocal = func(context.Context, string, ...string) (string, string, error) {
			return output, test.stderr, childErr
		}
		got, err := runner.warpctl(context.Background(), "logs", "synthetic", "fixture-service", "--since=1m", "--query="+privateText)
		if got != output || err == nil || !errors.Is(err, childErr) {
			t.Fatalf("%s lost command output or native error ownership", test.name)
		}
		if gotClass := classifyObservationError(err); gotClass != test.class {
			t.Errorf("%s class=%s, want %s", test.name, gotClass, test.class)
		}
		for _, privateValue := range []string{privateText, "private.example", "192.0.2.77", "fixture-token", "--query", "panic:"} {
			if strings.Contains(err.Error(), privateValue) {
				t.Errorf("%s copied private child or argument text into the error", test.name)
			}
		}
		alerts, runErr := runOneShotLogOutput(t, context.Background(), got, err, nil)
		requireOneShotLogVisibility(t, alerts, runErr, childErr, test.class, privateText, "private remote record", "private.example", "192.0.2.77", "--query", "panic:")
	}
}

// A failed overlap keeps its established identity and live findings; diagnostic
// wrappers must not reintroduce private block selectors around the safe cause.
func TestWarpctlLogQueryReconciliationKeepsSafeFailureClass(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	runner := newRunner(&monitorConfig{commandTimeout: time.Minute})
	runner.runLocal = func(context.Context, string, ...string) (string, string, error) {
		return "partial remote data", "panic: Loki query error (503): fixture-private-response", errors.New("exit status 2")
	}
	_, queryErr := runner.warpctl(context.Background(), "logs", "synthetic", "fixture-service", "--since=1m")
	tailer := newLogTailer("fixture-service", nil)
	tailer.clock = func() time.Time { return now }
	tailer.startedAt = now.Add(-time.Minute)
	tailer.recordReconcile(now.Add(-2*time.Minute), fmt.Errorf("block fixture-private-selector continuation: %w", queryErr))
	_, _, lastSuccess, lastError := tailer.reconcileSnapshot()
	finding := tailerReconcileFinding("fixture-service", now, tailer.startedAt, lastSuccess, lastError)
	alert := alertFromFinding(syntheticSettings(&syntheticSource{}), "1.5", "log-errors", "Log errors", finding)
	if alert.SignalID != "monitor/visibility" || alert.Class != "tailer-reconcile" ||
		alert.Target != "logs/fixture-service" || alert.Frame != "" || alert.Sustain != 2 || alert.Severity != SeverityWarn {
		t.Fatal("query classification changed the standing visibility identity")
	}
	if !strings.Contains(alert.Observed, "error_class=observation-log-query-http-503") {
		t.Error("standing reconciliation lost the fixed query-failure cause")
	}
	requireAlertOmits(t, alert, "fixture-private-response", "fixture-private-selector", "partial remote data", "panic:")
	tailer.recordReconcile(now.Add(-time.Minute), nil)
	_, _, lastSuccess, lastError = tailer.reconcileSnapshot()
	if restored := tailerReconcileFinding("fixture-service", now, tailer.startedAt, lastSuccess, lastError); !restored.healthy {
		t.Fatal("a subsequent complete query did not clear the same visibility identity")
	}
}

// Successful diagnostics, including retry failures, cannot manufacture failure.
func TestWarpctlLogQuerySuccessPreservesOutput(t *testing.T) {
	runner := newRunner(&monitorConfig{commandTimeout: time.Minute})
	const output = "ordinary remote record\n"
	runner.runLocal = func(context.Context, string, ...string) (string, string, error) {
		return output, "panic: Loki query error (429): synthetic hostile diagnostic", nil
	}
	got, err := runner.warpctl(context.Background(), "logs", "synthetic", "fixture-service", "--since=1m")
	if err != nil || got != output {
		t.Fatal("successful bounded query was changed by its diagnostics")
	}
}

// A real local child deadline is authoritative even with misleading stderr.
func TestWarpctlLogQueryCommandDeadlineIsPrivate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runner := newRunner(&monitorConfig{commandTimeout: time.Minute})
		runner.runLocal = func(ctx context.Context, _ string, _ ...string) (string, string, error) {
			<-ctx.Done()
			return "partial output", "panic: Loki query error (502): private child", errors.New("synthetic killed child")
		}
		out, err := runner.warpctl(context.Background(), "logs", "synthetic", "fixture-service", "--since=1m")
		if out != "partial output" || !errors.Is(err, context.DeadlineExceeded) || classifyObservationError(err) != observationErrorClassTimeout {
			t.Fatal("bounded child deadline lost partial output or timeout identity")
		}
		if strings.Contains(err.Error(), "private child") || strings.Contains(err.Error(), "502") {
			t.Fatal("earlier child response overrode the authoritative timeout")
		}
	})
}

func TestWarpctlLogQueryParentCancellationIsPrivate(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runner := newRunner(&monitorConfig{commandTimeout: time.Minute})
	runner.runLocal = func(context.Context, string, ...string) (string, string, error) {
		cancel()
		return "partial output", "panic: Loki query error (503): private child", errors.New("synthetic killed child")
	}
	_, err := runner.warpctl(ctx, "logs", "synthetic", "fixture-service", "--since=1m")
	if !errors.Is(err, context.Canceled) || classifyObservationError(err) != observationErrorClassCanceled || strings.Contains(err.Error(), "503") {
		t.Fatal("parent cancellation became a query or service failure")
	}
	runner.runLocal = func(context.Context, string, ...string) (string, string, error) {
		t.Fatal("pre-canceled query invoked a child")
		return "", "", nil
	}
	_, err = runner.warpctl(ctx, "logs", "synthetic", "fixture-service", "--since=1m")
	if !errors.Is(err, context.Canceled) {
		t.Fatal("pre-canceled query lost its lifecycle cause")
	}
}

func TestWarpctlLogQueryNativeStatusIsPreserved(t *testing.T) {
	childErr := syntheticProcessExit(t, 2)
	runner := newRunner(&monitorConfig{commandTimeout: time.Minute})
	runner.runLocal = func(context.Context, string, ...string) (string, string, error) {
		return "", "panic: Loki query error (504): private response", childErr
	}
	_, err := runner.warpctl(context.Background(), "logs", "synthetic", "fixture-service", "--since=1m")
	if !errors.Is(err, childErr) || err.Error() != "warpctl logs failed: error_class=observation-log-query-http-504 exit_status=2" {
		t.Fatal("native status or fixed query class was lost")
	}
}

// Separate exec copy workers must not corrupt the combined-output contract or
// admit stdout into the bounded local-diagnostic prefix.
func TestWarpctlLogQueryConcurrentCaptureSeparatesDiagnostics(t *testing.T) {
	capture := &localCommandCapture{}
	stderr := localCommandStderrWriter{capture: capture}
	var workers sync.WaitGroup
	workers.Add(2)
	go func() {
		defer workers.Done()
		for range 1000 {
			capture.Write([]byte("O"))
		}
	}()
	go func() {
		defer workers.Done()
		for range 1000 {
			stderr.Write([]byte("E"))
		}
	}()
	workers.Wait()
	if strings.Count(capture.output.String(), "O") != 1000 || strings.Count(capture.output.String(), "E") != 1000 || capture.diagnostics.String() != strings.Repeat("E", 1000) {
		t.Fatal("concurrent capture lost output or mixed remote stdout into stderr")
	}
	stderr.Write([]byte(strings.Repeat("E", warpctlLogDiagnosticLimit)))
	if capture.diagnostics.Len() != warpctlLogDiagnosticLimit+1 || capture.output.Len() != 2000+warpctlLogDiagnosticLimit {
		t.Fatal("diagnostic bound truncated the existing combined-output contract")
	}
}

func TestWarpctlLogQueryDoesNotClassifyOtherLocalCommands(t *testing.T) {
	childErr := errors.New("synthetic command failure")
	runner := newRunner(&monitorConfig{commandTimeout: time.Minute})
	runner.runLocal = func(context.Context, string, ...string) (string, string, error) {
		return "existing output", "panic: Loki query error (502): unrelated diagnostic", childErr
	}
	for _, args := range [][]string{{"ls", "services", "synthetic"}, {"logs", "synthetic", "fixture-service", "-f"}} {
		out, err := runner.warpctl(context.Background(), args...)
		var queryErr *warpctlLogQueryError
		if out != "existing output" || !errors.Is(err, childErr) || errors.As(err, &queryErr) {
			t.Fatal("bounded-query classifier changed another command's contract")
		}
	}
}
