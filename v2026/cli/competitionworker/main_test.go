// Signal regressions use explicit process readiness and descriptor-lifetime
// barriers. No production settings, database or evaluator host is contacted.
package main

import (
	"bufio"
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
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026/controller"
)

const workerProbeHelperArgument = "competition-worker-probe-helper"

// Only copies of this test binary enter a helper mode. The real self-check
// command creates its own process group and a descendant that ignores TERM.
func runWorkerProbeHelper(t *testing.T) bool {
	t.Helper()
	for index, argument := range os.Args {
		if argument != workerProbeHelperArgument {
			continue
		}
		if len(os.Args) != index+3 {
			t.Fatalf("invalid probe helper arguments: %v", os.Args[index:])
		}
		mode, target := os.Args[index+1], os.Args[index+2]
		switch mode {
		case "worker":
			content, err := os.ReadFile(target)
			if err != nil {
				t.Fatal(err)
			}
			digest := sha256.Sum256(content)
			settings := &controller.Settings{
				ArtifactRoot: filepath.Dir(target), SelfCheckCommand: target,
				SelfCheckCommandSha256: hex.EncodeToString(digest[:]),
			}
			err = runWorkerProbe(func(ctx context.Context) error {
				_, err := (controller.CommandEvaluator{}).SelfCheck(ctx, settings)
				return err
			})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("signal did not cancel and join the real self-check: %v", err)
			}
		case "leader":
			signals := make(chan os.Signal, 1)
			signal.Notify(signals, syscall.SIGTERM)
			defer signal.Stop(signals)
			child := exec.Command(os.Args[0], "-test.run=^TestCompetitionWorkerProbeSignalJoinsSelfCheck$", "--", workerProbeHelperArgument, "child", target)
			if err := child.Start(); err != nil {
				t.Fatal(err)
			}
			<-signals
			// The controller, not this cooperative leader, must kill and join
			// the descendant after it refuses the initial group TERM.
			os.Exit(0)
		case "child":
			connection, err := net.Dial("unix", target)
			if err != nil {
				t.Fatal(err)
			}
			defer connection.Close()
			signal.Ignore(syscall.SIGTERM)
			if _, err := fmt.Fprintf(connection, "%d %d armed\n", os.Getpid(), syscall.Getpgrp()); err != nil {
				t.Fatal(err)
			}
			// The held descriptor proves this exact descendant has terminated;
			// the test never guesses from a short delay or an observed PID alone.
			_, _ = io.Copy(io.Discard, connection)
		default:
			t.Fatalf("unknown probe helper mode %q", mode)
		}
		return true
	}
	return false
}

// Source readiness and --check share the actual wrapper tested here. Signals
// reach only the worker group; cleanup must separately reach the self-check.
func TestCompetitionWorkerProbeSignalJoinsSelfCheck(t *testing.T) {
	if runWorkerProbeHelper(t) {
		return
	}
	for _, terminationSignal := range []syscall.Signal{syscall.SIGTERM, syscall.SIGINT, syscall.SIGQUIT} {
		testWorkerProbeSignal(t, terminationSignal)
	}
}

// Socket paths are short even under a long caller TMPDIR. The broad deadlines
// only bound failures; the readiness handshake determines signal ordering.
func testWorkerProbeSignal(t *testing.T, terminationSignal syscall.Signal) {
	t.Helper()
	directory, err := os.MkdirTemp("/tmp", "ur-wp-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(directory)
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Net: "unix", Name: filepath.Join(directory, "probe.sock")})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := listener.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
		t.Fatal(err)
	}
	executablePath, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	quote := func(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }
	commandPath := filepath.Join(directory, "self-check")
	content := "#!/bin/sh\nexec " + quote(executablePath) + " -test.run=^TestCompetitionWorkerProbeSignalJoinsSelfCheck$ -- " + workerProbeHelperArgument + " leader " + quote(listener.Addr().String()) + "\n"
	if err := os.WriteFile(commandPath, []byte(content), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	worker := exec.CommandContext(ctx, executablePath, "-test.run=^TestCompetitionWorkerProbeSignalJoinsSelfCheck$", "--", workerProbeHelperArgument, "worker", commandPath)
	worker.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var output strings.Builder
	worker.Stdout, worker.Stderr = &output, &output
	if err := worker.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if worker.ProcessState == nil {
			_ = syscall.Kill(-worker.Process.Pid, syscall.SIGKILL)
			_ = worker.Wait()
		}
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
	readiness, err := reader.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(readiness)
	if len(fields) != 3 || fields[2] != "armed" {
		t.Fatalf("descendant readiness = %q", readiness)
	}
	childPid, childErr := strconv.Atoi(fields[0])
	processGroupId, groupErr := strconv.Atoi(fields[1])
	if childErr != nil || groupErr != nil || childPid <= 0 || processGroupId <= 0 || processGroupId == worker.Process.Pid {
		t.Fatalf("self-check did not establish a separate live descendant group: %q", readiness)
	}
	defer func() { _ = syscall.Kill(-processGroupId, syscall.SIGKILL) }()
	if err := syscall.Kill(-worker.Process.Pid, terminationSignal); err != nil {
		t.Fatal(err)
	}
	if err := worker.Wait(); err != nil {
		t.Fatalf("signal %v killed the worker before self-check cleanup: %v\n%s", terminationSignal, err, output.String())
	}
	if _, err := reader.ReadByte(); !errors.Is(err, io.EOF) {
		t.Fatalf("signal %v returned before descendant %d released its socket: %v", terminationSignal, childPid, err)
	}
}

// Every probe has the existing five-minute readiness bound, preserves its
// result, and releases its signal and deadline context after synchronous work.
func TestCompetitionWorkerProbePreservesResultAndDeadline(t *testing.T) {
	syntheticErr := errors.New("synthetic probe refusal")
	for _, expectedErr := range []error{nil, syntheticErr} {
		var probeCtx context.Context
		err := runWorkerProbe(func(ctx context.Context) error {
			probeCtx = ctx
			deadline, ok := ctx.Deadline()
			if !ok || time.Until(deadline) <= 0 || time.Until(deadline) > 5*time.Minute {
				t.Fatal("worker probe lost its bounded readiness deadline")
			}
			return expectedErr
		})
		if !errors.Is(err, expectedErr) {
			t.Fatalf("probe error = %v, want %v", err, expectedErr)
		}
		if !errors.Is(probeCtx.Err(), context.Canceled) {
			t.Fatal("finished probe retained its context")
		}
	}
}
