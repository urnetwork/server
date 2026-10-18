// Synthetic responses exercise the existing local command owner and failure
// projection. No fixture invokes Warpctl or contacts an observation backend.
package monitor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Different newline conventions and ordinary earlier retries leave the same
// complete terminal response, but only the last panic can supply its shape.
func TestWarpctlLog429ResponseShapeCompleteTerminalFraming(t *testing.T) {
	childErr := syntheticProcessExit(t, 2)
	for _, test := range []struct{ name, stderr, shape string }{
		{name: "loki-crlf", stderr: strings.ReplaceAll(query429Panic("too many outstanding requests"), "\n", "\r\n"), shape: "loki-outstanding-requests"},
		{name: "nginx-crlf", stderr: query429Panic(strings.ReplaceAll(query429NginxBody, "\n", "\r\n")), shape: "nginx-default-429"},
		{name: "earlier-retry", stderr: "retry: Loki query error (429): too many outstanding requests\n" + query429Panic(query429NginxBody), shape: "nginx-default-429"},
	} {
		err := newWarpctlLogQueryError(fmt.Errorf("fixture-private-wrapper: %w", childErr), test.stderr)
		if !errors.Is(err, childErr) || err.class != observationErrorClassLogHttp429 || !strings.Contains(err.Error(), "response_shape="+test.shape) {
			t.Errorf("%s lost the owned native status or terminal response shape", test.name)
		}
		if strings.Contains(err.Error(), "fixture-private-wrapper") || strings.Contains(err.Error(), "panic:") {
			t.Errorf("%s projected private diagnostics", test.name)
		}
	}
}

// A partial or ambiguous producer envelope cannot turn familiar body text into
// an attributed response. The original status class remains independent.
func TestWarpctlLog429ResponseShapeRejectsAmbiguousAndLookalikeBodies(t *testing.T) {
	childErr := syntheticProcessExit(t, 2)
	valid := query429Panic("too many outstanding requests")
	for _, test := range []struct{ name, stderr string }{
		{name: "missing-trace", stderr: "panic: Loki query error (429): too many outstanding requests\n"},
		{name: "partial-trace", stderr: valid[:strings.Index(valid, "main.main()")]},
		{name: "missing-terminal-newline", stderr: strings.TrimSuffix(valid, "\n")},
		{name: "trailing-diagnostic", stderr: valid + "fixture-private-trailer\n"},
		{name: "different-owner", stderr: strings.Replace(valid, "main.logs(", "main.query(", 1)},
		{name: "different-artifact", stderr: strings.ReplaceAll(valid, "/warpctl/main.go:", "/other-reader/main.go:")},
		{name: "additional-frame", stderr: strings.Replace(valid, "main.main()", "main.wrapper()\n\t/synthetic/warpctl/main.go:8 +0x1\nmain.main()", 1)},
		{name: "duplicate-terminal", stderr: valid + valid},
		{name: "duplicate-body-boundary", stderr: query429Panic("too many outstanding requests\n\ngoroutine fixture-private-body")},
		{name: "multiple-panic-headers", stderr: "panic: fixture-private-earlier\n" + valid},
		{name: "service-stderr-record", stderr: "[fixture-service] " + valid},
		{name: "retry-only", stderr: "retry: Loki query error (429): too many outstanding requests\n"},
		{name: "nonterminal-known-shape", stderr: "retry: Loki query error (429): too many outstanding requests\n" + query429Panic("fixture-private-final-response")},
		{name: "literal-suffix", stderr: query429Panic("too many outstanding requests fixture-private-suffix")},
		{name: "literal-prefix", stderr: query429Panic("fixture-private-prefix too many outstanding requests")},
		{name: "literal-newline", stderr: query429Panic("too many outstanding requests\n")},
		{name: "literal-control-byte", stderr: query429Panic("too many outstanding requests\x00")},
		{name: "versioned-nginx", stderr: query429Panic(strings.Replace(query429NginxBody, "nginx</center>", "nginx/1.26.3</center>", 1))},
		{name: "nginx-custom-footer", stderr: query429Panic(query429NginxBody + "fixture-private-footer")},
		{name: "nginx-leading-space", stderr: query429Panic(" " + query429NginxBody)},
		{name: "oversized", stderr: strings.Repeat("x", warpctlLogDiagnosticLimit) + "\n" + valid},
	} {
		err := newWarpctlLogQueryError(childErr, test.stderr)
		if !errors.Is(err, childErr) || !strings.Contains(err.Error(), "exit_status=2") {
			t.Errorf("%s lost the native failure", test.name)
		}
		if strings.Contains(err.Error(), "response_shape=loki-") || strings.Contains(err.Error(), "response_shape=nginx-") {
			t.Errorf("%s acquired a known response shape", test.name)
		}
		if len(test.stderr) <= warpctlLogDiagnosticLimit && err.class != warpctlLogDiagnosticClass(test.stderr) {
			t.Errorf("%s changed the original status classification", test.name)
		}
		if err.class == observationErrorClassLogHttp429 && !strings.Contains(err.Error(), "response_shape=unknown") {
			t.Errorf("%s hid unknown response evidence", test.name)
		}
		for _, privateText := range []string{"fixture-private", "too many outstanding requests", "<html>", "panic:", "/synthetic/"} {
			if strings.Contains(err.Error(), privateText) {
				t.Errorf("%s projected private response/trace text", test.name)
			}
		}
	}
}

// Native lifecycle and exit evidence are authoritative even when the stderr
// prefix looks exactly like a recognized terminal response.
func TestWarpctlLog429ResponseShapeKeepsLifecycleAndNativeStatus(t *testing.T) {
	childErr := syntheticProcessExit(t, 2)
	for _, test := range []struct {
		name  string
		err   error
		class string
	}{
		{name: "native-255", err: syntheticProcessExit(t, 255), class: observationErrorClassLogHttp429},
		{name: "native-1", err: syntheticProcessExit(t, 1), class: observationErrorClassLogHttp429},
		{name: "nonnative-status-text", err: errors.New("exit status 2: fixture-private-status"), class: observationErrorClassLogHttp429},
		{name: "canceled", err: errors.Join(context.Canceled, childErr), class: observationErrorClassCanceled},
		{name: "deadline", err: errors.Join(context.DeadlineExceeded, childErr), class: observationErrorClassTimeout},
	} {
		err := newWarpctlLogQueryError(test.err, query429Panic("too many outstanding requests"))
		if !errors.Is(err, test.err) || err.class != test.class {
			t.Errorf("%s lost the lifecycle or original status class", test.name)
		}
		var exitErr *exec.ExitError
		if errors.As(test.err, &exitErr) && !strings.Contains(err.Error(), fmt.Sprintf("exit_status=%d", exitErr.ExitCode())) {
			t.Errorf("%s lost the actual native status", test.name)
		}
		if strings.Contains(err.Error(), "response_shape=loki-") || strings.Contains(err.Error(), "response_shape=nginx-") || strings.Contains(err.Error(), "fixture-private") {
			t.Errorf("%s acquired a response hint or private text", test.name)
		}
		if test.class != observationErrorClassLogHttp429 && strings.Contains(err.Error(), "response_shape=") {
			t.Errorf("%s projected an HTTP429 detail after lifecycle superseded it", test.name)
		}
	}
	for _, status := range []string{"502", "503", "504"} {
		err := newWarpctlLogQueryError(childErr, strings.Replace(query429Panic("too many outstanding requests"), "(429)", "("+status+")", 1))
		if strings.Contains(err.Error(), "response_shape=") || !strings.Contains(err.Error(), "http-"+status) {
			t.Errorf("HTTP %s changed its original status or acquired a 429 response hint", status)
		}
	}
}

// The enum boundary does not trust even a locally supplied shape field.
func TestWarpctlLog429ResponseShapeRenderingRemainsClosed(t *testing.T) {
	err := &warpctlLogQueryError{class: observationErrorClassLogHttp429, responseShape: "fixture-private-arbitrary-shape", err: errors.New("fixture-private-cause")}
	if err.Error() != "warpctl logs failed: error_class=observation-log-query-http-429 response_shape=unknown" {
		t.Fatal("response shape rendering admitted an open string")
	}
}

// A real child writes stderr, exits and is joined through the unchanged runner;
// stdout with the opposite body cannot change its terminal response projection.
func TestWarpctlLog429ResponseShapeRealLocalCapture(t *testing.T) {
	dir := t.TempDir()
	stderr := query429Panic(query429NginxBody)
	const stdout = "service fixture: too many outstanding requests\n"
	script := "#!/bin/sh\ncat <<'FIXTURE_STDOUT'\n" + stdout + "FIXTURE_STDOUT\ncat >&2 <<'FIXTURE_STDERR'\n" + stderr + "FIXTURE_STDERR\nexit 2\n"
	if err := os.WriteFile(filepath.Join(dir, "warpctl"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	runner := newRunner(&monitorConfig{commandTimeout: time.Minute})
	out, err := runner.warpctl(context.Background(), "logs", "synthetic", "fixture-service", "--since=1m")
	var exitErr *exec.ExitError
	if err == nil || !errors.As(err, &exitErr) || !exitErr.ProcessState.Exited() || exitErr.ExitCode() != 2 {
		t.Fatal("local child was not joined with its actual native exit")
	}
	if !strings.Contains(out, stdout) || !strings.Contains(out, stderr) || !strings.Contains(err.Error(), "response_shape=nginx-default-429") {
		t.Fatal("real capture changed output ownership or lost the terminal stderr shape")
	}
	if strings.Contains(err.Error(), "too many outstanding requests") || strings.Contains(err.Error(), "<html>") || strings.Contains(err.Error(), "/synthetic/") {
		t.Fatal("real capture exposed private body or traceback")
	}
}

// Retaining the complete apparent panic at the front of an over-limit stderr
// stream cannot bypass the existing capture overflow sentinel.
func TestWarpctlLog429ResponseShapeOverflowIsUnknown(t *testing.T) {
	capture := &localCommandCapture{}
	stderr := localCommandStderrWriter{capture: capture}
	valid := query429Panic("too many outstanding requests")
	stderr.Write([]byte(valid))
	stderr.Write([]byte(strings.Repeat("x", warpctlLogDiagnosticLimit)))
	err := newWarpctlLogQueryError(syntheticProcessExit(t, 2), capture.diagnostics.String())
	if capture.diagnostics.Len() != warpctlLogDiagnosticLimit+1 || err.class != observationErrorClassCommandFailed || strings.Contains(err.Error(), "response_shape=") {
		t.Fatal("bounded diagnostic overflow acquired a known response or status")
	}
}
