// Loopback Mimir queries opt into byte-bounded native SSH intake. Other SSH
// probes and the separately bounded PG sampler retain their own contracts.
package monitor

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"strings"
	"time"
)

// Local observation limits do not change backend capacity or query cadence.
const (
	mimirShellResponseMaxBytes = 4 * 1024 * 1024
	mimirShellStderrMaxBytes   = 64 * 1024
)

// Presence marks only the selected loopback query adapter.
type mimirShellContextKey struct{}

// Only a fixed stream name crosses the observation boundary.
type mimirShellOutputError struct{ stream string }

// No remote bytes, command text or credentials appear in this error.
func (self *mimirShellOutputError) Error() string {
	return "Mimir observation output limit exceeded: " + self.stream
}

// Bounded diagnostics stay private while the SSH adapter retains its exact
// connect-timeout classifier. Unwrapping preserves the actual native status.
type mimirShellNativeError struct {
	err    error
	stderr string
}

// The underlying exec status contains no command output.
func (self *mimirShellNativeError) Error() string { return self.err.Error() }

// Native status classification remains available without rendering stderr.
func (self *mimirShellNativeError) Unwrap() error { return self.err }

// Each exec pipe has one writer; results are read only after Wait joins them.
type mimirShellWriter struct {
	buffer           bytes.Buffer
	limit            int
	exceeded         bool
	cancel           context.CancelFunc
	receiptMarker    string
	receiptScan      int
	receiptLineStart int
}

// Keep at most the accepted prefix in memory, cancel on the first extra byte.
func (self *mimirShellWriter) Write(value []byte) (int, error) {
	if self.exceeded {
		return 0, errors.New("Mimir observation output limit exceeded")
	}
	remaining := self.limit - self.buffer.Len()
	if len(value) > remaining {
		_, _ = self.buffer.Write(value[:remaining])
		self.exceeded = true
		self.cancel()
		return remaining, errors.New("Mimir observation output limit exceeded")
	}
	count, err := self.buffer.Write(value)
	self.observeReceipt()
	return count, err
}

// Inspect each completed diagnostic line once. A receipt cancels this child
// promptly; a later script success or stall cannot erase the refusal. Final
// framing qualification remains after Wait, including duplicate rejection.
func (self *mimirShellWriter) observeReceipt() {
	if self.receiptMarker == "" {
		return
	}
	value := self.buffer.Bytes()
	identity := mimirShellBudgetPrefix
	for self.receiptScan < len(value) {
		if value[self.receiptScan] == '\n' {
			if strings.Contains(string(value[self.receiptLineStart:self.receiptScan]), identity) {
				self.cancel()
			}
			self.receiptLineStart = self.receiptScan + 1
		}
		self.receiptScan++
	}
}

// The child keeps the existing SSH owner deadline. Overflow discards both
// streams and wins over the cancellation used solely to stop that child.
func runMimirShellCommand(parent context.Context, name string, args []string, stdin string, limit int, marker string) (string, string, error) {
	if err := parent.Err(); err != nil {
		return "", "", err
	}
	if limit <= 0 {
		return "", "", &mimirShellOutputError{stream: "policy"}
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	stdout := &mimirShellWriter{limit: limit, cancel: cancel}
	stderr := &mimirShellWriter{limit: mimirShellStderrMaxBytes, cancel: cancel, receiptMarker: marker}
	command := exec.CommandContext(ctx, name, args...)
	command.WaitDelay = time.Second
	if stdin != "" {
		command.Stdin = bytes.NewBufferString(stdin)
	}
	command.Stdout, command.Stderr = stdout, stderr
	boundary := observationBoundaryNativeStart
	err := command.Start()
	if err == nil {
		err = command.Wait()
		boundary = observationBoundaryNativeWaitUnknown
		if command.ProcessState != nil {
			boundary = observationBoundaryNativeWait
		}
	}
	if parent.Err() != nil {
		return "", "", &observationBoundaryError{err: parent.Err(), boundary: boundary}
	}
	if stdout.exceeded {
		return "", "", &observationBoundaryError{err: &mimirShellOutputError{stream: "stdout"}, boundary: boundary}
	}
	if stderr.exceeded {
		return "", "", &observationBoundaryError{err: &mimirShellOutputError{stream: "stderr"}, boundary: boundary}
	}
	if seen, qualified := mimirShellBudgetReceipt(stderr.buffer.String(), marker); seen {
		stream := "receipt"
		if qualified {
			stream = "remote-body"
		}
		return "", "", &observationBoundaryError{err: &mimirShellOutputError{stream: stream}, boundary: boundary}
	}
	if err != nil {
		return "", "", &observationBoundaryError{err: &mimirShellNativeError{err: err, stderr: stderr.buffer.String()}, boundary: boundary}
	}
	return stdout.buffer.String(), "", nil
}

// Preserve gateway order and ordinary failure fallback. An oversized response
// is not replayed through siblings, and a synthetic source cannot bypass the
// accepted-output limit. Synthetic sources own their preallocated return data.
func mimirShellFirstServiceGateway(ctx context.Context, runner probeRunner, hosts []*host, preferred *host, command string, limit int) (string, *host, error) {
	return mimirShellGateway(ctx, runner, hosts, preferred, command, limit, false, nil)
}

// Existing strict owner parsers retain successful-body overflow authority.
// Native intake is always bounded; this does not trust an oversized fixture.
func mimirShellFirstServiceGatewayParsed(ctx context.Context, runner probeRunner, hosts []*host, preferred *host, command string, limit int) (string, *host, error) {
	return mimirShellGateway(ctx, runner, hosts, preferred, command, limit, true, nil)
}

// An owner may retain a safe rejected-envelope cause after retirement. This
// classifier is pure and receives only a within-bound successful transport
// body; no returned sample or finding is accepted from a retired owner.
func mimirShellFirstServiceGatewayWithRejection(ctx context.Context, runner probeRunner, hosts []*host, preferred *host, command string, limit int, classifyRetired func(string) error) (string, *host, error) {
	return mimirShellGateway(ctx, runner, hosts, preferred, command, limit, false, classifyRetired)
}

// Preserve the parent and every observed cause without contacting a sibling
// after cancellation, native overflow or an owned remote refusal.
func mimirShellGateway(ctx context.Context, runner probeRunner, hosts []*host, preferred *host, command string, limit int, ownerParsesBody bool, classifyRetired func(string) error) (string, *host, error) {
	if limit <= 0 {
		return "", nil, &mimirShellOutputError{stream: "policy"}
	}
	ctx = context.WithValue(ctx, mimirShellContextKey{}, limit)
	ordered := make([]*host, 0, len(hosts))
	if preferred != nil {
		ordered = append(ordered, preferred)
	}
	for _, candidate := range hosts {
		if candidate != preferred {
			ordered = append(ordered, candidate)
		}
	}
	failures := make([]error, 0, len(ordered))
	for _, candidate := range ordered {
		output, err := runner.shell(ctx, candidate, command)
		if contextErr := ctx.Err(); contextErr != nil {
			observed := append(append([]error(nil), failures...), err)
			if err == nil && len(output) <= limit && classifyRetired != nil {
				observed = append(observed, classifyRetired(output))
			}
			return "", nil, mimirShellParentError(contextErr, errors.Join(observed...))
		}
		var overflow *mimirShellOutputError
		if errors.As(err, &overflow) {
			return "", nil, err
		}
		if err == nil {
			if !ownerParsesBody && len(output) > limit {
				return "", nil, &mimirShellOutputError{stream: "stdout"}
			}
			return output, candidate, nil
		}
		failures = append(failures, err)
	}
	return "", nil, errors.Join(failures...)
}

// An overflow supplies no usable prefix and must never clear product findings.
func mimirShellOverflowFindings(err error, target string) ([]finding, bool) {
	var overflow *mimirShellOutputError
	if !errors.As(err, &overflow) {
		return nil, false
	}
	visibilityFinding := cannotObserveFinding(target, err)
	visibilityFinding.observed += " source_phase=output-bound stream=" + overflow.stream
	return []finding{visibilityFinding}, true
}

// Direct query reads preserve their original shell transport and guards.
func mimirShell(ctx context.Context, runner probeRunner, target *host, command string, limit int) (string, error) {
	if limit <= 0 {
		return "", &mimirShellOutputError{stream: "policy"}
	}
	output, err := runner.shell(context.WithValue(ctx, mimirShellContextKey{}, limit), target, command)
	return mimirShellAccept(ctx, output, err, limit)
}

// The API release reader retains its stdin and explicit command deadline.
func mimirShellStdin(ctx context.Context, runner probeRunner, target *host, command, stdin string, timeout time.Duration, limit int) (string, error) {
	if limit <= 0 {
		return "", &mimirShellOutputError{stream: "policy"}
	}
	output, err := runner.sshTimeout(context.WithValue(ctx, mimirShellContextKey{}, limit), target, command, stdin, timeout)
	return mimirShellAccept(ctx, output, err, limit)
}

// An injected source cannot use a usable prefix after exceeding its policy.
func mimirShellAccept(ctx context.Context, output string, err error, limit int) (string, error) {
	if contextErr := ctx.Err(); contextErr != nil {
		return "", mimirShellParentError(contextErr, err)
	}
	if err != nil {
		return "", err
	}
	if limit > 0 && len(output) > limit {
		return "", &mimirShellOutputError{stream: "stdout"}
	}
	return output, nil
}

// Error text stays lifecycle-only while errors.Is retains the source cause.
func mimirShellParentError(contextErr, sourceErr error) error {
	if sourceErr == nil {
		return contextErr
	}
	return &signalContextError{contextErr: contextErr, sourceErr: sourceErr}
}

// Only callers with an existing first-step byte guard use this seam. Native
// stdout/stderr still cannot exceed the same configured limit successfully.
func mimirShellParsed(ctx context.Context, runner probeRunner, target *host, command string, limit int) (string, error) {
	if limit <= 0 {
		return "", &mimirShellOutputError{stream: "policy"}
	}
	output, err := runner.shell(context.WithValue(ctx, mimirShellContextKey{}, limit), target, command)
	return mimirShellAccept(ctx, output, err, 0)
}

// The release reader keeps its stdin/deadline and existing strict body parser.
func mimirShellStdinParsed(ctx context.Context, runner probeRunner, target *host, command, stdin string, timeout time.Duration, limit int) (string, error) {
	if limit <= 0 {
		return "", &mimirShellOutputError{stream: "policy"}
	}
	output, err := runner.sshTimeout(context.WithValue(ctx, mimirShellContextKey{}, limit), target, command, stdin, timeout)
	return mimirShellAccept(ctx, output, err, 0)
}
