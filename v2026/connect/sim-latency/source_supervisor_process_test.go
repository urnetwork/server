// Actual command construction and real synthetic children cover nested cleanup
// ownership. Short barrier tests do not claim a measured native 80-second join.
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
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/docopt/docopt-go"
)

// Only explicit test-child reexecution selects this synthetic control protocol.
func TestSourceSupervisorJoinHelperProcess(t *testing.T) {
	start := -1
	for index, argument := range os.Args {
		if argument == "synthetic-supervisor-join-helper" {
			start = index + 1
			break
		}
	}
	if start < 0 {
		return
	}
	if len(os.Args) < start+2 {
		t.Fatal("missing synthetic join helper arguments")
	}
	mode, address := os.Args[start], os.Args[start+1]
	if mode == "worker" && (len(os.Args) != start+3 || os.Args[start+2] != "--supervisor-request") {
		t.Fatal("worker selector changed")
	}
	if _, err := io.Copy(io.Discard, os.Stdin); err != nil {
		t.Fatal(err)
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM)
	defer signal.Stop(signals)
	connection, err := net.Dial("unix", address)
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.SetDeadline(time.Now().Add(25 * time.Second)); err != nil {
		t.Fatal(err)
	}
	commands := make(chan string)
	readerCtx, cancelReader := context.WithCancel(context.Background())
	readerJoined := make(chan struct{})
	go func() {
		defer close(readerJoined)
		reader := bufio.NewReader(connection)
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				close(commands)
				return
			}
			select {
			case commands <- line:
			case <-readerCtx.Done():
				return
			}
		}
	}()
	closeReader := func() {
		cancelReader()
		_ = connection.Close()
		<-readerJoined
	}
	defer closeReader()
	chunk := bytes.Repeat([]byte("synthetic-bounded-diagnostic\n"), 4096)
	for range 12 {
		if _, err := os.Stdout.Write(chunk); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stderr.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := fmt.Fprintln(connection, "ready"); err != nil {
		t.Fatal(err)
	}
	canceled := false
	for {
		select {
		case <-signals:
			canceled = true
			if _, err := fmt.Fprintln(connection, "term"); err != nil {
				t.Fatal(err)
			}
		case command, ok := <-commands:
			if !ok {
				t.Fatal("cleanup owner abandoned its barrier")
			}
			switch command {
			case "ping\n":
				if _, err := fmt.Fprintln(connection, "pong"); err != nil {
					t.Fatal(err)
				}
			case "release\n":
				if !canceled || mode == "stalled" {
					t.Fatal("invalid cleanup release")
				}
				if _, err := os.Stderr.Write(chunk); err != nil {
					t.Fatal(err)
				}
				if _, err := fmt.Fprintln(connection, "cleanup-joined"); err != nil {
					t.Fatal(err)
				}
				closeReader()
				os.Exit(0)
			default:
				t.Fatal("unknown synthetic command")
			}
		}
	}
}

// A fixed script execs only this test binary, with a private socket barrier.
func sourceSupervisorJoinProgram(t testing.TB, mode, address string) (string, string) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	quote := func(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }
	content := []byte("#!/bin/sh\nexec " + quote(executable) + " -test.run=^TestSourceSupervisorJoinHelperProcess$ -- synthetic-supervisor-join-helper " + quote(mode) + " " + quote(address) + " \"$@\"\n")
	path := filepath.Join(t.TempDir(), "synthetic-join-program")
	if err := os.WriteFile(path, content, 0o500); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(content)
	return path, hex.EncodeToString(digest[:])
}

// The pre-fix actual constructor set 30 seconds for all children, less than the
// worker's legitimate 80-second cleanup. This test fails without that repair.
func TestSourceSupervisorWorkerConstructorPreservesWholeJoin(t *testing.T) {
	settings := defaultSourceSupervisorProcessSettings()
	if settings.WorkerControlJoinTimeout != 80*time.Second || settings.WorkerHandoffTimeout != 10*time.Second {
		t.Fatal("worker owner defaults no longer match the transport role", settings)
	}
	for _, arguments := range [][]string{nil, {"--supervisor-request"}, {"--supervisor-request", "extra"}, {"--supervisor-request=other"}} {
		command, err := sourceSupervisorProcessCommand(t.Context(), "/synthetic/not-executed", arguments, settings)
		if err != nil {
			t.Fatal(err)
		}
		want := 30 * time.Second
		if len(arguments) == 1 && arguments[0] == "--supervisor-request" {
			want = 90 * time.Second
		}
		if command.WaitDelay != want || command.Cancel == nil {
			t.Fatal("actual child constructor truncated cleanup or changed ordinary grace", arguments, command.WaitDelay)
		}
	}
}

// The real command parser carries non-default role joins without silently
// accepting malformed, zero or shorter-than-native values.
func TestSourceSupervisorWorkerJoinOptionIsValidated(t *testing.T) {
	for _, value := range []string{"", "120s", "70s", "69s", "0s", "-1s", "invalid", "2562047h47m16.854775807s"} {
		arguments := sourceSupervisorPreparationArguments()
		if value != "" {
			arguments = append(arguments, "--worker-control-join-timeout="+value)
		}
		parser := &docopt.Parser{HelpHandler: docopt.NoHelpHandler}
		opts, err := parser.ParseArgs(simulationUsage(), arguments, "synthetic-version")
		if err != nil {
			t.Fatal("supervisor usage rejected its control option", err)
		}
		settings, err := sourceSupervisorProcessSettingsFromOptions(opts)
		valid := value == "" || value == "120s" || value == "70s"
		if (err == nil) != valid {
			t.Fatal("invalid role join silently accepted or valid override lost", value, err)
		}
		if !valid {
			continue
		}
		want := 80 * time.Second
		if value != "" {
			want, _ = time.ParseDuration(value)
		}
		command, err := sourceSupervisorProcessCommand(t.Context(), "/synthetic/not-executed", []string{"--supervisor-request"}, settings)
		if err != nil || settings.WorkerControlJoinTimeout != want || command.WaitDelay != want+settings.WorkerHandoffTimeout {
			t.Fatal("explicit role join failed to reach actual constructor", settings, err)
		}
	}
}

// Each invocation has its own actual child and socket. A canceled worker keeps
// cleanup blocked while its sibling still performs work, then both truly join.
func TestSourceSupervisorWorkerCancellationJoinsWithHealthySibling(t *testing.T) {
	directory, err := os.MkdirTemp("/tmp", "synthetic-supervisor-join-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(directory)
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Net: "unix", Name: filepath.Join(directory, "join.sock")})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := listener.SetDeadline(time.Now().Add(25 * time.Second)); err != nil {
		t.Fatal(err)
	}
	type child struct {
		cancel     context.CancelFunc
		connection *net.UnixConn
		reader     *bufio.Reader
		wait       func() error
	}
	children := make([]child, 2)
	for index := range children {
		path, digest := sourceSupervisorJoinProgram(t, "worker", listener.Addr().String())
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		joined := make(chan error, 1)
		go func() {
			_, err := runSourceSupervisorProcessWithSettings(ctx, path, digest, struct{}{}, nil, []string{"--supervisor-request"}, false, defaultSourceSupervisorProcessSettings())
			joined <- err
		}()
		var waitOnce sync.Once
		var commandErr error
		wait := func() error {
			waitOnce.Do(func() { commandErr = <-joined })
			return commandErr
		}
		var connection *net.UnixConn
		t.Cleanup(func() {
			cancel()
			if connection != nil {
				_ = connection.Close()
			}
			_ = wait()
		})
		connection, err = listener.AcceptUnix()
		if err != nil {
			t.Fatal(err)
		}
		defer connection.Close()
		if err := connection.SetDeadline(time.Now().Add(25 * time.Second)); err != nil {
			t.Fatal(err)
		}
		reader := bufio.NewReader(connection)
		if line, err := reader.ReadString('\n'); err != nil || line != "ready\n" {
			t.Fatal("child did not arm cancellation and drain diagnostics", line, err)
		}
		children[index] = child{cancel: cancel, connection: connection, reader: reader, wait: wait}
	}
	children[0].cancel()
	if line, err := children[0].reader.ReadString('\n'); err != nil || line != "term\n" {
		t.Fatal("worker did not enter actual post-term cleanup", line, err)
	}
	if _, err := fmt.Fprintln(children[1].connection, "ping"); err != nil {
		t.Fatal(err)
	}
	if line, err := children[1].reader.ReadString('\n'); err != nil || line != "pong\n" {
		t.Fatal("canceling one worker stopped its healthy sibling", line, err)
	}
	for index, child := range children {
		if index != 0 {
			child.cancel()
			if line, err := child.reader.ReadString('\n'); err != nil || line != "term\n" {
				t.Fatal("sibling did not enter its own cleanup", line, err)
			}
		}
		if _, err := fmt.Fprintln(child.connection, "release"); err != nil {
			t.Fatal(err)
		}
		if line, err := child.reader.ReadString('\n'); err != nil || line != "cleanup-joined\n" {
			t.Fatal("child cleanup was killed or abandoned", line, err)
		}
		if err := child.wait(); !errors.Is(err, context.Canceled) {
			t.Fatal("original command Wait lost requested cancellation", err)
		}
		if _, err := child.reader.ReadByte(); !errors.Is(err, io.EOF) {
			t.Fatal("original Wait returned before descriptor closure", err)
		}
	}
}

// Ordinary programs still have a finite real forced-stop path. A killed child
// is explicitly not credited as successful worker containment cleanup.
func TestSourceSupervisorOrdinaryProgramForceStopJoinsActualChild(t *testing.T) {
	directory, err := os.MkdirTemp("/tmp", "synthetic-supervisor-force-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(directory)
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Net: "unix", Name: filepath.Join(directory, "force.sock")})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := listener.SetDeadline(time.Now().Add(25 * time.Second)); err != nil {
		t.Fatal(err)
	}
	path, digest := sourceSupervisorJoinProgram(t, "stalled", listener.Addr().String())
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	settings := defaultSourceSupervisorProcessSettings()
	settings.ProgramJoinTimeout = 100 * time.Millisecond
	joined := make(chan error, 1)
	go func() {
		_, err := runSourceSupervisorProcessWithSettings(ctx, path, digest, struct{}{}, nil, nil, false, settings)
		joined <- err
	}()
	var waitOnce sync.Once
	var commandErr error
	wait := func() error {
		waitOnce.Do(func() { commandErr = <-joined })
		return commandErr
	}
	var connection *net.UnixConn
	t.Cleanup(func() {
		cancel()
		if connection != nil {
			_ = connection.Close()
		}
		_ = wait()
	})
	connection, err = listener.AcceptUnix()
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if err := connection.SetDeadline(time.Now().Add(25 * time.Second)); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(connection)
	if line, err := reader.ReadString('\n'); err != nil || line != "ready\n" {
		t.Fatal("stalled helper was not ready", line, err)
	}
	cancel()
	err = wait()
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) || !exitError.ProcessState.Sys().(syscall.WaitStatus).Signaled() || exitError.ProcessState.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
		t.Fatal("finite ordinary grace failed to force and originally Wait its child", err)
	}
	content, err := io.ReadAll(reader)
	if err != nil || strings.Contains(string(content), "cleanup-joined") {
		t.Fatal("forced termination was falsely credited as graceful cleanup", string(content), err)
	}
}

// This is only a proposed owner configuration, not an installed-unit or native
// systemd qualification. Its final group kill still covers abandoned children.
func TestSourceSupervisorProposedUnitGraceCoversDefaultWorker(t *testing.T) {
	path := os.Getenv("SIM_LATENCY_TEST_SUPERVISOR_CLEANUP_CONFIG")
	if path == "" {
		path = "launch/supervisor-cleanup-grace.conf"
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "[Service]\nTimeoutStopSec=120s\nKillMode=mixed\n" {
		t.Fatal("proposed unit changed its exact cleanup contract")
	}
	settings := defaultSourceSupervisorProcessSettings()
	stopTimeout := 120 * time.Second
	workerJoinTimeout := settings.WorkerControlJoinTimeout + settings.WorkerHandoffTimeout
	if stopTimeout <= workerJoinTimeout || stopTimeout-workerJoinTimeout < settings.ProgramJoinTimeout {
		t.Fatal("unit stop cap cannot cover default child grace and outer handoff")
	}
}
