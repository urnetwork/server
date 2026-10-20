// Trusted-process output tests use private synthetic children. Large logs must
// not close pipes, forge machine receipts or bypass cancellation cleanup.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/urnetwork/server/controller"
)

const sourceSupervisorOutputHelper = "synthetic-supervisor-output-helper"

// Only an explicitly selected copy of this test binary emits synthetic logs;
// the parent's fixed worker selector remains part of the exercised protocol.
func TestSourceSupervisorOutputHelperProcess(t *testing.T) {
	start := -1
	for index, argument := range os.Args {
		if argument == sourceSupervisorOutputHelper {
			start = index + 1
			break
		}
	}
	if start < 0 {
		return
	}
	if len(os.Args) < start+2 {
		t.Fatal("missing synthetic helper arguments")
	}
	mode, target := os.Args[start], os.Args[start+1]
	if (mode == "worker" || mode == "cancel") && (len(os.Args) != start+3 || os.Args[start+2] != "--supervisor-request") {
		t.Fatal("launcher omitted its fixed worker selector")
	}
	if _, err := io.Copy(io.Discard, os.Stdin); err != nil {
		t.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer stop()
	chunk := bytes.Repeat([]byte("synthetic-private-log\n"), 2048)
	for range 64 {
		if _, err := os.Stderr.Write(chunk); err != nil {
			t.Fatal(err)
		}
		if mode == "worker" || mode == "oversize" || mode == "cancel" {
			if _, err := os.Stdout.Write(chunk); err != nil {
				t.Fatal(err)
			}
		}
	}
	if mode == "cancel" {
		connection, err := net.Dial("unix", target)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fmt.Fprintln(connection, "synthetic-logs-drained-and-signal-armed"); err != nil {
			t.Fatal(err)
		}
		<-ctx.Done()
		_ = connection.Close()
		os.Exit(0)
	}
	if mode == "machine" {
		if _, err := fmt.Fprint(os.Stdout, `{"synthetic":"receipt"}`); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(target, []byte("synthetic-child-completed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if mode == "failure" {
		os.Exit(23)
	}
	os.Exit(0)
}

// The small pinned script only execs this test binary; it is not the workload,
// a generated production command or an authority-bearing deployment artifact.
func sourceSupervisorOutputProgram(t testing.TB, mode, target string) (string, string) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	quote := func(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }
	content := []byte("#!/bin/sh\nexec " + quote(executable) + " -test.run=^TestSourceSupervisorOutputHelperProcess$ -- " + sourceSupervisorOutputHelper + " " + quote(mode) + " " + quote(target) + " \"$@\"\n")
	path := filepath.Join(t.TempDir(), "synthetic-trusted-command")
	if err := os.WriteFile(path, content, 0o500); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(content)
	return path, hex.EncodeToString(digest[:])
}

// A real child exceeds both old pipe limits, then proves it reached its normal
// end. The pre-fix writer closes stderr and the child terminates with SIGPIPE.
func TestSourceSupervisorWorkerLogsDoNotTerminateChild(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "completed")
	path, digest := sourceSupervisorOutputProgram(t, "worker", marker)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if err := runSourceSupervisorWorkerCommand(ctx, path, digest, controller.CompetitionSupervisorWorkerRequest{Operation: "run_one_epoch"}); err != nil {
		t.Fatal(err)
	}
	if content, err := os.ReadFile(marker); err != nil || string(content) != "synthetic-child-completed\n" {
		t.Fatalf("verbose child did not complete: %q %v", content, err)
	}
}

// A broker's exact machine stdout survives arbitrary diagnostic volume; log
// bytes are never appended to the returned document or its public error.
func TestSourceSupervisorMachineOutputDrainsDiagnostics(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "completed")
	path, digest := sourceSupervisorOutputProgram(t, "machine", marker)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	content, err := runSourceSupervisorProgram(ctx, path, digest, struct{}{}, nil)
	if err != nil || string(content) != `{"synthetic":"receipt"}` {
		t.Fatalf("diagnostics corrupted machine stdout: %q %v", content, err)
	}
}

// Draining is not acceptance: truncated machine output is refused only after
// the process is joined, never parsed as a complete authority-bearing receipt.
func TestSourceSupervisorRejectsOversizedMachineOutputAfterJoin(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "completed")
	path, digest := sourceSupervisorOutputProgram(t, "oversize", marker)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	content, err := runSourceSupervisorProgram(ctx, path, digest, struct{}{}, nil)
	if err == nil || content != nil || !strings.Contains(err.Error(), "output exceeds its bound") {
		t.Fatalf("oversized stdout became a receipt or killed the child: bytes=%d error=%v", len(content), err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("output refusal returned before child completion: %v", err)
	}
}

// A bounded diagnostic prefix cannot replace the real child status with a
// transport signal, nor can sensitive log content escape through the error.
func TestSourceSupervisorPreservesFailureAfterLargeDiagnostics(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "completed")
	path, digest := sourceSupervisorOutputProgram(t, "failure", marker)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	err := runSourceSupervisorCommand(ctx, path, digest, struct{}{})
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) || exitError.ExitCode() != 23 || !strings.Contains(err.Error(), "stderr_truncated=true") || strings.Contains(err.Error(), "synthetic-private-log") {
		t.Fatalf("real failure was masked or leaked diagnostics: %v", err)
	}
}

// Output keeps fixed memory while consuming every byte, including writes
// crossing the bound and subsequent writes after truncation has occurred.
func TestSourceSupervisorOutputRetainsBoundedPrefixAndDrains(t *testing.T) {
	for _, limit := range []int{0, 4, 8} {
		output := &sourceSupervisorOutput{limit: limit}
		for _, value := range []string{"abcd", "efgh", "ijkl"} {
			if written, err := output.Write([]byte(value)); err != nil || written != len(value) {
				t.Fatalf("limit %d stopped draining: written=%d error=%v", limit, written, err)
			}
		}
		if string(output.content) != "abcdefghijkl"[:limit] || !output.exceeded {
			t.Fatalf("limit %d retained wrong prefix: %q exceeded=%t", limit, output.content, output.exceeded)
		}
	}
}

// An explicit socket handshake occurs after log overflow and signal setup.
// Cancellation must still reach the child and join its descriptor cleanup.
func TestSourceSupervisorCancellationJoinsVerboseChild(t *testing.T) {
	directory, err := os.MkdirTemp("/tmp", "ur-supervisor-output-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(directory)
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Net: "unix", Name: filepath.Join(directory, "ready.sock")})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := listener.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
		t.Fatal(err)
	}
	path, digest := sourceSupervisorOutputProgram(t, "cancel", listener.Addr().String())
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	joined := make(chan error, 1)
	go func() {
		joined <- runSourceSupervisorWorkerCommand(ctx, path, digest, controller.CompetitionSupervisorWorkerRequest{Operation: "run_one_epoch"})
	}()
	connection, err := listener.AcceptUnix()
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if err := connection.SetDeadline(time.Now().Add(40 * time.Second)); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(connection)
	if ready, err := reader.ReadString('\n'); err != nil || ready != "synthetic-logs-drained-and-signal-armed\n" {
		t.Fatalf("child readiness = %q, %v", ready, err)
	}
	cancel()
	if err := <-joined; !errors.Is(err, context.Canceled) {
		t.Fatalf("verbose child cancellation was lost: %v", err)
	}
	if _, err := reader.ReadByte(); !errors.Is(err, io.EOF) {
		t.Fatalf("launcher returned before child descriptor cleanup: %v", err)
	}
}
