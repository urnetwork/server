// Request-local pipe ownership bounds both local and SSH transport cancellation.
// Canceling transport never signals the detached native execution or a process group.
package controller

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"sync"
)

// All three streams are explicit files, not os/exec-owned copying pipes. Wait
// can therefore reap the exact child immediately without closing unread frames.
// Inherited streams cannot keep local I/O blocked after request cancellation;
// the descendant and detached native evaluation may continue independently.
func exchangeCompetitionAgentCommand(ctx context.Context, cancel context.CancelFunc, command string, arguments []string, maximumDiagnosticBytes int, writeRequest func(io.Writer) error, readResponse func(io.Reader) error) error {
	stdin, requestInput, err := os.Pipe()
	if err != nil {
		return err
	}
	responseOutput, stdout, err := os.Pipe()
	if err != nil {
		stdin.Close()
		requestInput.Close()
		return err
	}
	diagnosticOutput, stderr, err := os.Pipe()
	if err != nil {
		stdin.Close()
		requestInput.Close()
		responseOutput.Close()
		stdout.Close()
		return err
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
	process := exec.CommandContext(ctx, command, arguments...)
	process.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C", "LC_ALL=C"}
	process.Stdin, process.Stdout, process.Stderr = stdin, stdout, stderr
	startErr := process.Start()
	// Only the child retains these ends after Start, including on start failure.
	childCloseErr := errors.Join(stdin.Close(), stdout.Close(), stderr.Close())
	if startErr != nil {
		return errors.Join(startErr, childCloseErr)
	}
	if childCloseErr != nil {
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
	var waitErr error
	go func() {
		defer close(waited)
		waitErr = process.Wait()
	}()
	written := make(chan struct{})
	var writeErr error
	go func() {
		defer close(written)
		writeErr = errors.Join(writeRequest(requestInput), requestInput.Close())
		if writeErr != nil {
			cancel()
			closeIo()
		}
	}()
	drained := make(chan struct{})
	var diagnosticErr error
	go func() {
		defer close(drained)
		// As before, diagnostic overflow is an error, never unbounded buffering.
		// No diagnostic bytes enter a public error or service log.
		_, diagnosticErr = io.Copy(&boundedBuffer{limit: maximumDiagnosticBytes}, diagnosticOutput)
		if diagnosticErr != nil {
			cancel()
			closeIo()
		}
	}()
	readErr := readResponse(responseOutput)
	if readErr != nil {
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
	return errors.Join(childCloseErr, readErr, writeErr, diagnosticErr, waitErr, ctx.Err())
}
