// HTTP500 controls keep the native producer envelope and exercise the real
// local runner, reconciliation and one-shot reducers without a backend read.
package monitor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Match the retained native logs->main traceback shape with synthetic paths,
// arguments and response data. No production body or process identity is used.
func warpctlHttp500Panic(body string) string {
	return "panic: Loki query error (500): " + body + "\n\ngoroutine 1 [running]:\nmain.logs(0xc000000000)\n\t/synthetic/warpctl/main.go:1703 +0x78e\nmain.main()\n\t/synthetic/warpctl/main.go:277 +0x6b9\n"
}

// A private PATH contains only a fixture Warpctl. Its shell uses builtins,
// emits fixed fixture bytes and exits; no real CLI or network command can run.
func warpctlHttp500NativeRunner(t *testing.T, stdout, stderr string, status int) *runner {
	t.Helper()
	dir := t.TempDir()
	quote := func(text string) string { return "'" + strings.ReplaceAll(text, "'", "'\\''") + "'" }
	script := "#!/bin/sh\nprintf '%s' " + quote(stdout) + "\nprintf '%s' " + quote(stderr) + " >&2\nexit " + strconv.Itoa(status) + "\n"
	if err := os.WriteFile(filepath.Join(dir, "warpctl"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	return newRunner(&monitorConfig{commandTimeout: time.Minute})
}

// Causal RED: a real joined child emits the native terminal HTTP500 shape,
// but the original reducer discards that status as generic command failure.
func TestWarpctlLogHttp500NativeFailureKeepsStatus(t *testing.T) {
	const privateBody = "rpc error: code = Unknown desc = mkdir /synthetic-private-cache/fixture-native-shape-padding-123/index_fixture: no space left on device"
	stderr := warpctlHttp500Panic(privateBody)
	if len(stderr) != 306 {
		t.Fatal("synthetic fixture lost the retained 306-byte native panic shape")
	}
	runner := warpctlHttp500NativeRunner(t, "fixture-private-partial-record\n", stderr, 2)
	out, queryErr := runner.warpctl(context.Background(), "logs", "synthetic", "fixture-service", "--since=1m", "--query=fixture-private-selector")
	var exitErr *exec.ExitError
	if !errors.As(queryErr, &exitErr) || !exitErr.ProcessState.Exited() || exitErr.ExitCode() != 2 {
		t.Fatal("fixture did not produce a joined native exit2")
	}
	if got := classifyObservationError(queryErr); got != "observation-log-query-http-500" {
		t.Fatalf("native terminal HTTP500 lost fixed status: got %s", got)
	}
	if queryErr.Error() != "warpctl logs failed: error_class=observation-log-query-http-500 exit_status=2" {
		t.Fatal("HTTP500 error did not retain only the fixed status and native exit")
	}
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	tailer := newLogTailer("fixture-service", nil)
	tailer.clock = func() time.Time { return now }
	tailer.startedAt = now.Add(-2 * time.Minute)
	tailer.recordReconcile(now.Add(-2*time.Minute), fmt.Errorf("fixture-private-partition: %w", queryErr))
	_, _, lastSuccess, lastError := tailer.reconcileSnapshot()
	finding := tailerReconcileFinding("fixture-service", now, tailer.startedAt, lastSuccess, lastError)
	alert := alertFromFinding(syntheticSettings(&syntheticSource{}), "1.5", "log-errors", "Log errors", finding)
	if alert.SignalID != "monitor/visibility" || alert.Class != "tailer-reconcile" || alert.Target != "logs/fixture-service" || alert.Frame != "" || alert.Severity != SeverityWarn || alert.Sustain != 2 {
		t.Fatal("HTTP500 classification changed reconciliation identity or urgency")
	}
	if !strings.Contains(alert.Observed, "error_class=observation-log-query-http-500 exit_status=2") {
		t.Fatal("reconciliation discarded the native HTTP500 status")
	}
	requireAlertOmits(t, alert, privateBody, "fixture-private", "panic:", "/synthetic/", "response_shape=")
	alerts, runErr := runOneShotLogOutput(t, context.Background(), out, queryErr, nil)
	requireOneShotLogVisibility(t, alerts, runErr, queryErr, "observation-log-query-http-500", privateBody, "fixture-private", "panic:", "/synthetic/", "response_shape=")
	tailer.recordReconcile(now.Add(-time.Minute), nil)
	_, _, lastSuccess, lastError = tailer.reconcileSnapshot()
	if !tailerReconcileFinding("fixture-service", now, tailer.startedAt, lastSuccess, lastError).healthy {
		t.Fatal("a subsequent completed query did not restore the original visibility identity")
	}
}

// Only a bounded, unique terminal envelope from the owned native producer can
// carry the new class. Neither body text nor an earlier retry is authority.
func TestWarpctlLogHttp500RequiresOwnedTerminalEnvelope(t *testing.T) {
	childErr := syntheticProcessExit(t, 2)
	otherExit := syntheticProcessExit(t, 1)
	valid := warpctlHttp500Panic("fixture-private-body context deadline exceeded json: too many outstanding requests")
	for _, test := range []struct {
		name   string
		stderr string
		err    error
		class  string
	}{
		{name: "qualified-body-cannot-spoof-another-cause", stderr: valid, err: childErr, class: "observation-log-query-http-500"},
		{name: "header-only", stderr: "panic: Loki query error (500): fixture-private-body\n", err: childErr, class: observationErrorClassCommandFailed},
		{name: "fake-owner", stderr: strings.Replace(valid, "main.logs(", "main.unrelated(", 1), err: childErr, class: observationErrorClassCommandFailed},
		{name: "fake-source", stderr: strings.ReplaceAll(valid, "/warpctl/main.go:", "/unrelated/main.go:"), err: childErr, class: observationErrorClassCommandFailed},
		{name: "partial-trace", stderr: valid[:strings.Index(valid, "main.main()")], err: childErr, class: observationErrorClassCommandFailed},
		{name: "extra-trailer", stderr: valid + "fixture-private-trailer\n", err: childErr, class: observationErrorClassCommandFailed},
		{name: "multiple-headers", stderr: "panic: fixture-private-first\n" + valid, err: childErr, class: observationErrorClassCommandFailed},
		{name: "empty-first-header", stderr: "panic: \n" + valid, err: childErr, class: observationErrorClassCommandFailed},
		{name: "body-injected-trace", stderr: warpctlHttp500Panic("fixture-private-body\n\ngoroutine 1 [running]:\nfake"), err: childErr, class: observationErrorClassCommandFailed},
		{name: "oversized", stderr: valid + strings.Repeat("x", warpctlLogDiagnosticLimit), err: childErr, class: observationErrorClassCommandFailed},
		{name: "retry-only", stderr: "2026/01/01 12:00:00 client.go:1: Loki query attempt 1/3 failed (Loki query error (500): fixture-private-body). Retrying in 100ms.\n", err: childErr, class: observationErrorClassCommandFailed},
		{name: "service-stderr-record", stderr: "[fixture-service] " + valid, err: childErr, class: observationErrorClassCommandFailed},
		{name: "status-text-is-not-native", stderr: valid, err: errors.New("exit status 2"), class: observationErrorClassCommandFailed},
		{name: "other-native-exit", stderr: valid, err: otherExit, class: observationErrorClassCommandFailed},
		{name: "canceled", stderr: valid, err: errors.Join(context.Canceled, childErr), class: observationErrorClassCanceled},
		{name: "deadline", stderr: valid, err: errors.Join(context.DeadlineExceeded, childErr), class: observationErrorClassTimeout},
		{name: "empty-stderr", err: childErr, class: observationErrorClassCommandFailed},
	} {
		got := newWarpctlLogQueryError(test.err, test.stderr)
		if got.class != test.class || !errors.Is(got, test.err) {
			t.Errorf("%s: class=%s, want %s with unchanged native cause", test.name, got.class, test.class)
		}
		for _, forbidden := range []string{"fixture-private", "panic:", "/synthetic/", "context deadline exceeded", "json:", "response_shape="} {
			if strings.Contains(got.Error(), forbidden) {
				t.Errorf("%s: private diagnostic or unrelated cause entered safe error", test.name)
			}
		}
	}
}

// Native exit2 alone cannot establish an HTTP response or panic cause.
func TestWarpctlLogHttp500OrdinaryNativeExitRemainsUnknown(t *testing.T) {
	runner := warpctlHttp500NativeRunner(t, "", "ordinary fixture-private-diagnostic with number 500\n", 2)
	_, err := runner.warpctl(context.Background(), "logs", "synthetic", "fixture-service", "--since=1m")
	if err == nil || classifyObservationError(err) != observationErrorClassCommandFailed || !strings.Contains(err.Error(), "exit_status=2") {
		t.Fatal("ordinary exit2 diagnostic acquired HTTP500 authority")
	}
}

// Stdout records never supply the local diagnostic envelope, even on failure.
func TestWarpctlLogHttp500StdoutCannotSupplyAuthority(t *testing.T) {
	runner := warpctlHttp500NativeRunner(t, warpctlHttp500Panic("fixture-private-source-record"), "", 2)
	_, err := runner.warpctl(context.Background(), "logs", "synthetic", "fixture-service", "--since=1m")
	if err == nil || classifyObservationError(err) != observationErrorClassCommandFailed {
		t.Fatal("stdout record acquired native diagnostic authority")
	}
}

// Successful native completion remains successful despite diagnostic lookalikes.
func TestWarpctlLogHttp500SuccessfulChildRemainsHealthy(t *testing.T) {
	stderr := warpctlHttp500Panic("fixture-private-lookalike")
	runner := warpctlHttp500NativeRunner(t, "healthy fixture record\n", stderr, 0)
	out, err := runner.warpctl(context.Background(), "logs", "synthetic", "fixture-service", "--since=1m")
	if err != nil || !strings.Contains(out, "healthy fixture record") || !strings.Contains(out, stderr) {
		t.Fatal("successful completion or its existing output contract was changed")
	}
}
