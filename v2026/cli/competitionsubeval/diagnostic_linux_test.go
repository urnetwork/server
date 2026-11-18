//go:build linux

// Real native pipes, subprocess exits and explicit cleanup acknowledgements
// exercise the entrypoint; no installed config, manager or evaluator is used.
package main

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// A broken fd2 must not kill the process after its operation cleaned up.
func TestSubevaluationDiagnosticBrokenPipePreservesOriginalErrorAndExit(t *testing.T) {
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	read.Close()
	defer write.Close()
	cleanRead, cleanWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanRead.Close()
	defer cleanWrite.Close()
	command := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestSubevaluationDiagnosticProcess$", "--", "broken-pipe")
	command.Stderr, command.Stdout = write, io.Discard
	command.ExtraFiles = []*os.File{cleanWrite}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	cleanWrite.Close()
	var acknowledgment [1]byte
	if _, err := io.ReadFull(cleanRead, acknowledgment[:]); err != nil || acknowledgment[0] != 1 {
		_ = command.Process.Kill()
		_ = command.Wait()
		t.Fatal("operation cleanup did not precede its failed diagnostic")
	}
	err = command.Wait()
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatal("original operation failure did not produce a native exit")
	}
	status, ok := exit.Sys().(syscall.WaitStatus)
	if !ok || !status.Exited() || status.ExitStatus() != 1 {
		t.Fatal("broken diagnostic changed an ordinary operation error into signal termination")
	}
}

// This private subprocess uses the actual production signal/reporter owner.
// Its ordinary top-level invocation is a no-op PASS, not a skipped test.
func TestSubevaluationDiagnosticProcess(t *testing.T) {
	if len(os.Args) < 3 || os.Args[len(os.Args)-2] != "--" || os.Args[len(os.Args)-1] != "broken-pipe" {
		return
	}
	clean := os.NewFile(3, "synthetic-operation-cleaned")
	original := errors.New("synthetic operation failure")
	err := runSubevaluationCommand(nil, os.Stdin, os.Stdout, os.Stderr,
		func(context.Context, []string, io.Reader, io.Writer, io.Writer) error {
			defer func() { _, _ = clean.Write([]byte{1}); _ = clean.Close() }()
			return original
		}, defaultSubevaluationDiagnosticSettings())
	if !errors.Is(err, original) {
		os.Exit(91)
	}
	os.Exit(1)
}

// A full pipe is proven by EAGAIN before the finite diagnostic begins. Its
// actual native deadline, not a sleep/negative timeout, terminates the write.
func TestSubevaluationDiagnosticFullPipeHasNativeDeadlineAndSiblingIsolation(t *testing.T) {
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer read.Close()
	defer write.Close()
	connection, err := write.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var fillErr error
	if err := connection.Control(func(fd uintptr) {
		if fillErr = syscall.SetNonblock(int(fd), true); fillErr != nil {
			return
		}
		content := make([]byte, 4096)
		for written := 0; written <= 1024*1024; {
			n, err := syscall.Write(int(fd), content)
			if n > 0 {
				written += n
			}
			if errors.Is(err, syscall.EINTR) {
				continue
			}
			if errors.Is(err, syscall.EAGAIN) {
				return
			}
			if err != nil || n <= 0 {
				fillErr = errors.New("synthetic pipe fill failed")
				return
			}
		}
		fillErr = errors.New("synthetic pipe exceeded its finite fill bound")
	}); err != nil || fillErr != nil {
		t.Fatal("native full-pipe boundary unavailable")
	}
	settings := defaultSubevaluationDiagnosticSettings()
	settings.WriteTimeout = 50 * time.Millisecond
	blocked := newSubevaluationDiagnostic(write, settings)
	defer blocked.Close()
	done := make(chan error, 1)
	go func() { _, err := blocked.Write([]byte("synthetic diagnostic")); done <- err }()
	otherRead, otherWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer otherRead.Close()
	defer otherWrite.Close()
	healthy := newSubevaluationDiagnostic(otherWrite, defaultSubevaluationDiagnosticSettings())
	defer healthy.Close()
	if n, err := healthy.Write([]byte("ok")); err != nil || n != 2 {
		t.Fatal("blocked diagnostic contaminated a separate owner")
	}
	var payload [2]byte
	if _, err := io.ReadFull(otherRead, payload[:]); err != nil || string(payload[:]) != "ok" {
		t.Fatal("healthy sibling did not complete while another sink was full")
	}
	if err := <-done; !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatal("full diagnostic did not return its native deadline failure")
	}
	if n, err := blocked.Write([]byte("no new allowance")); err == nil || n != 0 {
		t.Fatal("failed diagnostic minted another write allowance")
	}
}

// No private fd or shared nonblocking change exists before the first print.
func TestSubevaluationDiagnosticIsLazyAndClosesOnlyItsDuplicate(t *testing.T) {
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer read.Close()
	defer write.Close()
	diagnostic := newSubevaluationDiagnostic(write, defaultSubevaluationDiagnosticSettings())
	if diagnostic.started || diagnostic.owned != nil {
		t.Fatal("construction changed the inherited stream owner")
	}
	if n, err := diagnostic.Write(nil); err != nil || n != 0 || diagnostic.owned != nil {
		t.Fatal("empty diagnostic captured an inherited descriptor")
	}
	if _, err := diagnostic.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	ownedFd := -1
	if diagnostic.owned != nil {
		if connection, err := diagnostic.owned.SyscallConn(); err == nil {
			_ = connection.Control(func(fd uintptr) { ownedFd = int(fd) })
		}
	}
	if ownedFd < 3 {
		t.Fatal("diagnostic retained stdout/stderr signal semantics")
	}
	if err := diagnostic.Close(); err != nil {
		t.Fatal(err)
	}
	if err := diagnostic.Close(); err != nil {
		t.Fatal("diagnostic close is not idempotent")
	}
	if _, err := write.Write([]byte("y")); err != nil {
		t.Fatal("diagnostic closed the caller's descriptor")
	}
	var payload [2]byte
	if _, err := io.ReadFull(read, payload[:]); err != nil || string(payload[:]) != "xy" {
		t.Fatal("private duplicate changed diagnostic bytes")
	}
}

// The finite byte allowance belongs to the command, never each fmt call.
func TestSubevaluationDiagnosticCannotRenewItsTotalByteBudget(t *testing.T) {
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer read.Close()
	defer write.Close()
	settings := defaultSubevaluationDiagnosticSettings()
	settings.MaximumBytes = 3
	diagnostic := newSubevaluationDiagnostic(write, settings)
	defer diagnostic.Close()
	if n, err := diagnostic.Write([]byte("abcdef")); n != 3 || !errors.Is(err, io.ErrShortWrite) {
		t.Fatal("diagnostic exceeded its total byte bound")
	}
	if n, err := diagnostic.Write([]byte("new print")); n != 0 || !errors.Is(err, io.ErrShortWrite) {
		t.Fatal("another print renewed the diagnostic byte budget")
	}
	var payload [3]byte
	if _, err := io.ReadFull(read, payload[:]); err != nil || string(payload[:]) != "abc" {
		t.Fatal("bounded diagnostic changed its accepted prefix")
	}
}

// Nonpollable file I/O cannot promise a native deadline, so no write is issued.
func TestSubevaluationDiagnosticRefusesNonpollableSinkAndInvalidBounds(t *testing.T) {
	file, err := os.Create(filepath.Join(t.TempDir(), "synthetic-diagnostic-file"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	diagnostic := newSubevaluationDiagnostic(file, defaultSubevaluationDiagnosticSettings())
	defer diagnostic.Close()
	if n, err := diagnostic.Write([]byte("not permitted")); n != 0 || err == nil {
		t.Fatal("nonpollable diagnostic acquired an unbounded write")
	}
	if info, err := file.Stat(); err != nil || info.Size() != 0 {
		t.Fatal("refused nonpollable sink was modified")
	}
	for _, settings := range []subevaluationDiagnosticSettings{{MaximumBytes: 1}, {WriteTimeout: time.Second}} {
		invalid := newSubevaluationDiagnostic(file, settings)
		if n, err := invalid.Write([]byte("no bound")); n != 0 || err == nil || invalid.owned != nil {
			t.Fatal("invalid diagnostic bounds captured a descriptor")
		}
		invalid.Close()
	}
}

// The diagnostic reporter is not a transport adapter: both management placement
// and native child stdio must retain the exact inherited physical file object.
func TestSubevaluationCommandPreservesPhysicalDiagnosticDescriptor(t *testing.T) {
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer read.Close()
	defer write.Close()
	called := false
	err = runSubevaluationCommand(nil, nil, io.Discard, write,
		func(_ context.Context, _ []string, _ io.Reader, _ io.Writer, diagnostic io.Writer) error {
			called = true
			physical, ok := diagnostic.(*os.File)
			if !ok || physical != write {
				t.Fatal("reporter replaced management's inherited physical descriptor")
			}
			return nil
		}, defaultSubevaluationDiagnosticSettings())
	if err != nil || !called {
		t.Fatal("physical transport never reached its command owner")
	}
}
