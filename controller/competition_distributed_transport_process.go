// Request-local pipe ownership bounds both local and SSH transport cancellation.
// Canceling transport never signals the detached native execution or a process group.
package controller

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// Closed facts are public-safe; Unwrap retains the original joined errors.
// Neither diagnostics nor an underlying error's text enters this description.
type CompetitionAgentTransportError struct {
	stage            string
	firstErr         error
	contextBefore    error
	contextAfter     error
	ReadFailed       bool
	WriteFailed      bool
	DiagnosticFailed bool
	WaitFailed       bool
	ChildCloseFailed bool
	ChildExitKnown   bool
	ChildExitCode    int
	ChildExitSignal  int
	responsePhase    string
	cause            error
}

// The vocabulary stays closed even for a malformed or zero-value error.
func (self *CompetitionAgentTransportError) Stage() string {
	switch self.stage {
	case "pipe", "start", "close", "read", "write", "diagnostic", "wait", "context":
		return self.stage
	default:
		return "unknown"
	}
}

// A phase is closed metadata, never a response, diagnostic or private error.
func (self *CompetitionAgentTransportError) ResponsePhase() string {
	switch self.responsePhase {
	case "frame", "authentication", "files", "trailer", "complete":
		return self.responsePhase
	default:
		return "unknown"
	}
}

// Classification never formats the error or reads child diagnostic bytes.
func competitionAgentTransportErrorClass(err error) string {
	switch {
	case err == nil:
		return "none"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline"
	case errors.Is(err, io.EOF):
		return "eof"
	case errors.Is(err, io.ErrUnexpectedEOF):
		return "unexpected_eof"
	case errors.Is(err, os.ErrClosed), errors.Is(err, io.ErrClosedPipe):
		return "closed_pipe"
	default:
		return "other"
	}
}

// Before/after context facts distinguish caller expiry from cleanup-induced
// cancellation. The remaining facts are only booleans and a numeric child exit.
func (self *CompetitionAgentTransportError) Error() string {
	return fmt.Sprintf("executor process transport stage=%s cause=%s context_before=%s context_after=%s read=%t write=%t diagnostic=%t wait=%t child_close=%t child_exit_known=%t child_exit=%d child_signal=%d response_phase=%s",
		self.Stage(), competitionAgentTransportErrorClass(self.firstErr), competitionAgentTransportErrorClass(self.contextBefore), competitionAgentTransportErrorClass(self.contextAfter),
		self.ReadFailed, self.WriteFailed, self.DiagnosticFailed, self.WaitFailed, self.ChildCloseFailed, self.ChildExitKnown, self.ChildExitCode, self.ChildExitSignal, self.ResponsePhase())
}

// Preserve errors.Is/As and every existing cancellation/integrity cause.
func (self *CompetitionAgentTransportError) Unwrap() error { return self.cause }

// All three streams are explicit files, not os/exec-owned copying pipes. Wait
// can therefore reap the exact child immediately without closing unread frames.
// Inherited streams cannot keep local I/O blocked after request cancellation;
// the descendant and detached native evaluation may continue independently.
func exchangeCompetitionAgentCommand(ctx context.Context, cancel context.CancelFunc, command string, arguments []string, maximumDiagnosticBytes int, writeRequest func(io.Writer) error, readResponse func(io.Reader) error) error {
	return exchangeCompetitionAgentCommandWithWaitObserver(ctx, cancel, command, arguments, maximumDiagnosticBytes, writeRequest, readResponse, nil)
}

// The nil production observer has no I/O, timing or authority effect. Native
// tests may acknowledge actual Wait completion before consuming buffered bytes;
// the observer must return synchronously and is never exposed by a public role.
func exchangeCompetitionAgentCommandWithWaitObserver(ctx context.Context, cancel context.CancelFunc, command string, arguments []string, maximumDiagnosticBytes int, writeRequest func(io.Writer) error, readResponse func(io.Reader) error, observeWait func()) error {
	return exchangeCompetitionAgentCommandWithJoin(ctx, cancel, command, arguments, maximumDiagnosticBytes,
		defaultCompetitionDistributedCoordinatorSettings().ControlJoinTimeout, writeRequest, readResponse, observeWait)
}

// Cancellation first notifies only the held transport child, preserving its
// independent native cleanup owner. WaitDelay bounds a wedged child's grace;
// no process group or detached evaluator is signaled by this transport.
func exchangeCompetitionAgentCommandWithJoin(ctx context.Context, cancel context.CancelFunc, command string, arguments []string, maximumDiagnosticBytes int, joinTimeout time.Duration, writeRequest func(io.Writer) error, readResponse func(io.Reader) error, observeWait func()) error {
	return exchangeCompetitionAgentCommandWithHooks(ctx, cancel, command, arguments, maximumDiagnosticBytes, joinTimeout,
		writeRequest, readResponse, competitionAgentCommandHooks{waited: observeWait})
}

// Production hooks are nil. Tests may acknowledge actual signal/Wait edges;
// callbacks must return synchronously and are never chosen by a request/config.
type competitionAgentCommandHooks struct {
	waited   func()
	canceled func(error)
}

// The concrete native implementation is shared by the uninstrumented owner
// and deterministic tests; hooks never select another signal or target.
func exchangeCompetitionAgentCommandWithHooks(ctx context.Context, cancel context.CancelFunc, command string, arguments []string, maximumDiagnosticBytes int, joinTimeout time.Duration, writeRequest func(io.Writer) error, readResponse func(io.Reader) error, hooks competitionAgentCommandHooks) error {
	if joinTimeout <= 0 || maximumDiagnosticBytes <= 0 {
		return errors.New("executor transport requires finite cleanup and diagnostic bounds")
	}
	var stateLock sync.Mutex
	var firstStage string
	var firstErr, contextBefore error
	var childCloseErr, readErr, writeErr, diagnosticErr, waitErr error
	var process *exec.Cmd
	// Record before local cancel/close can manufacture a context failure.
	// Concurrent faults retain the first observed stage, not a guessed cause.
	noteFailure := func(stage string, err error) {
		if err == nil {
			return
		}
		stateLock.Lock()
		defer stateLock.Unlock()
		if firstStage == "" {
			contextBefore = ctx.Err()
			if contextBefore != nil {
				stage, err = "context", contextBefore
			}
			firstStage, firstErr = stage, err
		}
	}
	transportError := func(cause error) error {
		if cause == nil {
			return nil
		}
		noteFailure("context", ctx.Err())
		stateLock.Lock()
		defer stateLock.Unlock()
		failure := &CompetitionAgentTransportError{stage: firstStage, firstErr: firstErr, contextBefore: contextBefore, contextAfter: ctx.Err(),
			ReadFailed: readErr != nil, WriteFailed: writeErr != nil, DiagnosticFailed: diagnosticErr != nil, WaitFailed: waitErr != nil, ChildCloseFailed: childCloseErr != nil, cause: cause}
		if process != nil && process.ProcessState != nil {
			failure.ChildExitKnown, failure.ChildExitCode = true, process.ProcessState.ExitCode()
			if status, ok := process.ProcessState.Sys().(interface {
				Signaled() bool
				Signal() syscall.Signal
			}); ok && status.Signaled() {
				failure.ChildExitSignal = int(status.Signal())
			}
		}
		return failure
	}
	stdin, requestInput, err := os.Pipe()
	if err != nil {
		noteFailure("pipe", err)
		return transportError(err)
	}
	responseOutput, stdout, err := os.Pipe()
	if err != nil {
		stdin.Close()
		requestInput.Close()
		noteFailure("pipe", err)
		return transportError(err)
	}
	diagnosticOutput, stderr, err := os.Pipe()
	if err != nil {
		stdin.Close()
		requestInput.Close()
		responseOutput.Close()
		stdout.Close()
		noteFailure("pipe", err)
		return transportError(err)
	}
	var closeOnce sync.Once
	closeIo := func() {
		closeOnce.Do(func() {
			requestInput.Close()
			responseOutput.Close()
			diagnosticOutput.Close()
		})
	}
	defer closeIo()
	process = exec.CommandContext(ctx, command, arguments...)
	process.Cancel = func() error {
		err := process.Process.Signal(syscall.SIGTERM)
		if hooks.canceled != nil {
			hooks.canceled(err)
		}
		return err
	}
	process.WaitDelay = joinTimeout
	process.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C", "LC_ALL=C"}
	process.Stdin, process.Stdout, process.Stderr = stdin, stdout, stderr
	startErr := process.Start()
	// Only the child retains these ends after Start, including on start failure.
	childCloseErr = errors.Join(stdin.Close(), stdout.Close(), stderr.Close())
	if startErr != nil {
		noteFailure("start", startErr)
		return transportError(errors.Join(startErr, childCloseErr))
	}
	if childCloseErr != nil {
		noteFailure("close", childCloseErr)
		cancel()
		closeIo()
	}
	watchStopped, stopWatch := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(watchStopped)
		select {
		case <-ctx.Done():
			closeIo()
		case <-stopWatch:
		}
	}()
	waited := make(chan struct{})
	go func() {
		defer close(waited)
		waitErr = process.Wait()
		noteFailure("wait", waitErr)
		if hooks.waited != nil {
			hooks.waited()
		}
	}()
	written := make(chan struct{})
	go func() {
		defer close(written)
		writeErr = errors.Join(writeRequest(requestInput), requestInput.Close())
		if writeErr != nil {
			noteFailure("write", writeErr)
			cancel()
			closeIo()
		}
	}()
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		// As before, diagnostic overflow is an error, never unbounded buffering.
		// No diagnostic bytes enter a public error or service log.
		_, diagnosticErr = io.Copy(&boundedBuffer{limit: maximumDiagnosticBytes}, diagnosticOutput)
		if diagnosticErr != nil {
			noteFailure("diagnostic", diagnosticErr)
			cancel()
			closeIo()
		}
	}()
	readErr = readResponse(responseOutput)
	if readErr != nil {
		noteFailure("read", readErr)
		cancel()
		closeIo()
	}
	<-written
	<-drained
	<-waited
	close(stopWatch)
	<-watchStopped
	// A closed pipe is not an EOF proof, and even a complete response received
	// after cancellation cannot acquire fresh authority from the dead request.
	return transportError(errors.Join(childCloseErr, readErr, writeErr, diagnosticErr, waitErr, ctx.Err()))
}
