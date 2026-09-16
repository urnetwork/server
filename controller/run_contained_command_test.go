package controller

import (
	"bufio"
	"bytes"
	"context"
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
)

const containedCommandHelperName = "contained-command-helper"

// Runs a leader or child mode only inside a copy of this test binary. The
// child reports readiness after ignoring TERM, so the parent never depends on
// scheduler timing to create the process-group boundary.
func runContainedCommandHelper(t *testing.T) bool {
	t.Helper()
	for index, argument := range os.Args {
		if argument != containedCommandHelperName {
			continue
		}
		if len(os.Args) != index+3 {
			t.Fatalf("helper arguments = %#v", os.Args[index:])
		}
		mode, socketPath := os.Args[index+1], os.Args[index+2]
		switch mode {
		case "leader":
			signals := make(chan os.Signal, 1)
			signal.Notify(signals, syscall.SIGTERM)
			defer signal.Stop(signals)
			child := exec.Command(os.Args[0], "-test.run=^TestRunContainedCommandCancellationTerminatesDescendant$", "--", containedCommandHelperName, "child", socketPath)
			child.Stdout = os.Stdout
			child.Stderr = os.Stderr
			if err := child.Start(); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(9)
			}
			<-signals
			os.Exit(0)
		case "child":
			connection, err := net.Dial("unix", socketPath)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(10)
			}
			fmt.Fprintln(connection, os.Getpid())
			if _, err := bufio.NewReader(connection).ReadString('\n'); err != nil {
				_ = connection.Close()
				os.Exit(11)
			}
			signal.Ignore(syscall.SIGTERM)
			fmt.Fprintln(connection, "armed")
			_ = connection.Close()
			select {}
		default:
			t.Fatalf("unexpected helper mode %q", mode)
		}
	}
	return false
}

// A canceled evaluator must terminate every member of its private process
// group, even when the command leader cooperatively exits before a child that
// deliberately ignores TERM. The Unix listener is the explicit readiness
// barrier; the liveness check observes the exact recorded child PID.
func TestRunContainedCommandCancellationTerminatesDescendant(t *testing.T) {
	if runContainedCommandHelper(t) {
		return
	}
	testRunContainedCommandCancellation(t, false)
}

// Buffered output creates copier pipes, which the surviving child inherits.
// The command must bound their lifetime as well as terminate the process group.
func TestRunContainedCommandCancellationBoundsInheritedPipes(t *testing.T) {
	testRunContainedCommandCancellation(t, true)
}

// A readiness handshake proves the descendant ignores TERM before canceling.
func testRunContainedCommandCancellation(t *testing.T, bufferedOutput bool) {
	t.Helper()
	listener, err := net.ListenUnix("unix", &net.UnixAddr{
		Net:  "unix",
		Name: filepath.Join(t.TempDir(), "helper.sock"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := listener.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
		t.Fatal(err)
	}

	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer devNull.Close()
	var stdout, stderr io.Writer = devNull, devNull
	if bufferedOutput {
		stdout, stderr = new(bytes.Buffer), new(bytes.Buffer)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	commandDirectory := t.TempDir()
	type commandResult struct {
		exitCode int
		err      error
	}
	resultChannel := make(chan commandResult, 1)
	go func() {
		exitCode, commandErr := runContainedCommand(
			ctx,
			commandDirectory,
			os.Args[0],
			[]string{"-test.run=^TestRunContainedCommandCancellationTerminatesDescendant$", "--", containedCommandHelperName, "leader", listener.Addr().String()},
			stdout,
			stderr,
		)
		resultChannel <- commandResult{exitCode: exitCode, err: commandErr}
	}()

	connection, err := listener.AcceptUnix()
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	childPidText, err := bufio.NewReader(connection).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	childPid, err := strconv.Atoi(strings.TrimSpace(childPidText))
	if err != nil || childPid <= 0 {
		t.Fatalf("child PID %q: %v", childPidText, err)
	}
	defer func() { _ = syscall.Kill(childPid, syscall.SIGKILL) }()
	if _, err := fmt.Fprintln(connection, "arm"); err != nil {
		t.Fatal(err)
	}
	armed, err := bufio.NewReader(connection).ReadString('\n')
	if err != nil || strings.TrimSpace(armed) != "armed" {
		t.Fatalf("child arm acknowledgement = %q, %v", armed, err)
	}

	cancel()
	var result commandResult
	select {
	case result = <-resultChannel:
	case <-time.After(4 * processTermGrace):
		t.Fatal("contained command did not return after cancellation")
	}
	if !errors.Is(result.err, context.Canceled) || result.exitCode != 0 {
		t.Fatalf("canceled command result = (%d, %v), want (0, context canceled)", result.exitCode, result.err)
	}
	stat, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(childPid), "stat"))
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(string(stat[strings.LastIndexByte(string(stat), ')')+1:]))
	if len(fields) == 0 || fields[0] != "Z" && fields[0] != "X" {
		t.Fatalf("canceled command left TERM-ignoring descendant PID %d running: %s", childPid, stat)
	}
}

// Ordinary exit codes remain submission outcomes, not containment failures.
func TestRunContainedCommandPreservesExitCode(t *testing.T) {
	for _, expectedCode := range []int{0, 7, 255} {
		exitCode, err := runContainedCommand(t.Context(), t.TempDir(), "/bin/sh",
			[]string{"-c", "exit " + strconv.Itoa(expectedCode)}, io.Discard, io.Discard)
		if err != nil || exitCode != expectedCode {
			t.Errorf("exit %d: got (%d, %v)", expectedCode, exitCode, err)
		}
	}
}

// A caller that is already canceled must not launch any evaluator work.
func TestRunContainedCommandDoesNotStartAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	exitCode, err := runContainedCommand(ctx, t.TempDir(), "/missing-synthetic-command", nil, io.Discard, io.Discard)
	if exitCode != -1 || !errors.Is(err, context.Canceled) {
		t.Fatalf("already canceled result = (%d, %v)", exitCode, err)
	}
}
