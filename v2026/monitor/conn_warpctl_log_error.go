// Bounded local log queries keep child diagnostics separate from remote logs.
package monitor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
)

const warpctlLogDiagnosticLimit = 64 * 1024

const (
	observationErrorClassLogHttp429      = "observation-log-query-http-429"
	observationErrorClassLogHttp502      = "observation-log-query-http-502"
	observationErrorClassLogHttp503      = "observation-log-query-http-503"
	observationErrorClassLogHttp504      = "observation-log-query-http-504"
	observationErrorClassLogConfigSchema = "observation-log-query-config-schema"
)

// Store only a fixed class and the native execution cause, never stderr or argv.
// Unwrap preserves cancellation and exit-status checks without exposing them in
// alerts that render Error. HTTP classes describe the query, not a remote app.
type warpctlLogQueryError struct {
	class string
	err   error
}

func (self *warpctlLogQueryError) Error() string {
	message := "warpctl logs failed: error_class=" + self.class
	var exitErr *exec.ExitError
	if errors.As(self.err, &exitErr) {
		message += fmt.Sprintf(" exit_status=%d", exitErr.ExitCode())
	}
	return message
}

func (self *warpctlLogQueryError) Unwrap() error { return self.err }

// The authoritative lifecycle wins over any earlier child retry diagnostic.
func newWarpctlLogQueryError(err error, stderr string) *warpctlLogQueryError {
	class := observationErrorClassCommandFailed
	switch {
	case errors.Is(err, context.Canceled):
		class = observationErrorClassCanceled
	case errors.Is(err, context.DeadlineExceeded):
		class = observationErrorClassTimeout
	case len(stderr) <= warpctlLogDiagnosticLimit:
		class = warpctlLogDiagnosticClass(stderr)
	}
	return &warpctlLogQueryError{class: class, err: err}
}

// Warpctl terminates Search failures with a Go panic on stderr. Earlier retry
// lines and stdout records cannot prove the terminal cause. Multiple headers or
// an unknown/oversized shape remain generic command failure, not fabricated zero.
func warpctlLogDiagnosticClass(stderr string) string {
	var panicText string
	for _, line := range strings.Split(stderr, "\n") {
		text, ok := strings.CutPrefix(line, "panic: ")
		if !ok {
			continue
		}
		if panicText != "" {
			return observationErrorClassCommandFailed
		}
		panicText = strings.TrimSuffix(text, "\r")
	}
	for _, status := range []struct{ prefix, class string }{
		{prefix: "Loki query error (429):", class: observationErrorClassLogHttp429},
		{prefix: "Loki query error (502):", class: observationErrorClassLogHttp502},
		{prefix: "Loki query error (503):", class: observationErrorClassLogHttp503},
		{prefix: "Loki query error (504):", class: observationErrorClassLogHttp504},
	} {
		if strings.HasPrefix(panicText, status.prefix) {
			return status.class
		}
	}
	if strings.HasPrefix(panicText, "Loki query error (") {
		// Response bodies may quote arbitrary panic or timeout signatures.
		return observationErrorClassCommandFailed
	}
	for _, prefix := range []string{"yaml:", "json:", "invalid character ", "unexpected end of JSON input"} {
		if strings.HasPrefix(panicText, prefix) {
			return observationErrorClassLogConfigSchema
		}
	}
	if panicText == "context deadline exceeded" {
		return observationErrorClassTimeout
	}
	if panicText == "context canceled" {
		return observationErrorClassCanceled
	}
	if strings.HasPrefix(panicText, "Get \"") {
		// Discard the URL before checking net/http's transport diagnostic.
		if _, reason, ok := strings.Cut(panicText, "\": "); ok {
			for _, marker := range []string{"context deadline exceeded", "Client.Timeout exceeded", "i/o timeout", "TLS handshake timeout", "net/http: timeout awaiting response headers"} {
				if strings.Contains(reason, marker) {
					return observationErrorClassTimeout
				}
			}
		}
	}
	return observationErrorClassCommandFailed
}

type localCommandRunner func(context.Context, string, ...string) (output string, stderr string, err error)

// Capture combined output without allowing two exec copy workers to race. The
// diagnostic prefix is bounded; the existing command-output contract is intact.
type localCommandCapture struct {
	stateLock   sync.Mutex
	output      bytes.Buffer
	diagnostics bytes.Buffer
}

func (self *localCommandCapture) Write(data []byte) (int, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.output.Write(data)
}

// Only this writer receives the child's stderr, never remote stdout records.
type localCommandStderrWriter struct{ capture *localCommandCapture }

func (self localCommandStderrWriter) Write(data []byte) (int, error) {
	self.capture.stateLock.Lock()
	defer self.capture.stateLock.Unlock()
	remaining := max(0, warpctlLogDiagnosticLimit+1-self.capture.diagnostics.Len())
	self.capture.diagnostics.Write(data[:min(len(data), remaining)])
	return self.capture.output.Write(data)
}

// The standing stream has a different stderr owner and is not a bounded query.
func isBoundedWarpctlLogQuery(name string, args []string) bool {
	if name != "warpctl" || len(args) == 0 || args[0] != "logs" {
		return false
	}
	for _, arg := range args[1:] {
		if arg == "-f" || arg == "--follow" {
			return false
		}
	}
	return true
}

// Other commands retain the original single combined pipe. Log queries also
// retain a bounded stderr prefix for classification after a failed execution.
func runLocalCommand(ctx context.Context, name string, args ...string) (string, string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	capture := &localCommandCapture{}
	cmd.Stdout = capture
	cmd.Stderr = capture
	if isBoundedWarpctlLogQuery(name, args) {
		cmd.Stderr = localCommandStderrWriter{capture: capture}
	}
	err := cmd.Run()
	return capture.output.String(), capture.diagnostics.String(), err
}
