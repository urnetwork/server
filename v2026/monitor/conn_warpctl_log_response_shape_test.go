package monitor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

const query429NginxBody = "<html>\n<head><title>429 Too Many Requests</title></head>\n<body>\n<center><h1>429 Too Many Requests</h1></center>\n<hr><center>nginx</center>\n</body>\n</html>"

func query429Panic(body string) string {
	return "panic: Loki query error (429): " + body + "\n\ngoroutine 1 [running]:\nmain.logs(0xc000000000)\n\t/synthetic/warpctl/main.go:1703 +0x1\nmain.main()\n\t/synthetic/warpctl/main.go:120 +0x1\n"
}

// The natural bounded-query path already owns the response in private stderr.
// A fixed shape must survive that path and reconciliation without a new query.
func TestWarpctlLog429ResponseShapeCausal(t *testing.T) {
	for _, test := range []struct{ name, body, shape string }{
		{name: "loki", body: "too many outstanding requests", shape: "loki-outstanding-requests"},
		{name: "nginx", body: query429NginxBody, shape: "nginx-default-429"},
	} {
		childErr := syntheticProcessExit(t, 2)
		runner := newRunner(&monitorConfig{commandTimeout: time.Minute})
		runner.runLocal = func(context.Context, string, ...string) (string, string, error) {
			return "", query429Panic(test.body), childErr
		}
		_, err := runner.warpctl(context.Background(), "logs", "synthetic", "fixture-service", "--since=1m")
		if err == nil || !errors.Is(err, childErr) || classifyObservationError(err) != observationErrorClassLogHttp429 {
			t.Fatal("terminal native HTTP429 ownership changed")
		}
		if !strings.Contains(err.Error(), "response_shape="+test.shape) {
			t.Fatalf("%s: terminal source-qualified HTTP429 lost its fixed response shape", test.name)
		}
		if !strings.Contains(err.Error(), "exit_status=2") {
			t.Fatal("response hint discarded the actual native exit")
		}
		now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
		tailer := newLogTailer("fixture-service", nil)
		tailer.clock = func() time.Time { return now }
		tailer.startedAt = now.Add(-time.Minute)
		tailer.recordReconcile(now.Add(-2*time.Minute), fmt.Errorf("private-selector: %w", err))
		_, _, lastSuccess, lastError := tailer.reconcileSnapshot()
		finding := tailerReconcileFinding("fixture-service", now, tailer.startedAt, lastSuccess, lastError)
		alert := alertFromFinding(syntheticSettings(&syntheticSource{}), "1.5", "log-errors", "Log errors", finding)
		if alert.SignalID != "monitor/visibility" || alert.Class != "tailer-reconcile" || alert.Target != "logs/fixture-service" || alert.Frame != "" || alert.Severity != SeverityWarn || alert.Sustain != 2 {
			t.Fatal("response hint changed the visibility identity")
		}
		if !strings.Contains(alert.Observed, "response_shape="+test.shape) {
			t.Fatal("reconciliation discarded the fixed response hint")
		}
		requireAlertOmits(t, alert, "private-selector", test.body, "panic:", "/synthetic/")
		tailer.recordReconcile(now.Add(-time.Minute), nil)
		_, _, lastSuccess, lastError = tailer.reconcileSnapshot()
		if !tailerReconcileFinding("fixture-service", now, tailer.startedAt, lastSuccess, lastError).healthy {
			t.Fatal("subsequent healthy reconciliation did not clear the original identity")
		}
	}
}

func TestWarpctlLog429ResponseShapeHealthyAndUnrelatedPayloads(t *testing.T) {
	const servicePayload = "service message: too many outstanding requests"
	runner := newRunner(&monitorConfig{commandTimeout: time.Minute})
	runner.runLocal = func(context.Context, string, ...string) (string, string, error) {
		return servicePayload + query429NginxBody, query429Panic("too many outstanding requests"), nil
	}
	out, err := runner.warpctl(context.Background(), "logs", "synthetic", "fixture-service", "--since=1m")
	if err != nil || out != servicePayload+query429NginxBody {
		t.Fatal("healthy service payload or stderr lookalike became a query failure")
	}
	childErr := syntheticProcessExit(t, 2)
	runner.runLocal = func(context.Context, string, ...string) (string, string, error) {
		return servicePayload, query429Panic("too many outstanding requests"), childErr
	}
	for _, command := range []struct {
		name string
		args []string
	}{
		{name: "other-reader", args: []string{"logs", "synthetic", "fixture-service"}},
		{name: "warpctl", args: []string{"logs", "synthetic", "fixture-service", "-f"}},
	} {
		_, err := runner.local(context.Background(), command.name, command.args...)
		if err == nil || strings.Contains(err.Error(), "response_shape=") {
			t.Fatal("another artifact or standing stream acquired a bounded-query hint")
		}
	}
}
