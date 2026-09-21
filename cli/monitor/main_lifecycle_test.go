// Exercise process-owned scrubber shutdown without timing or live resources.
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// Observe the command/drain/return ordering through an explicit drain barrier.
type monitorProcessObservation struct {
	returnedBeforeDrain bool
	stdoutAtDrain       string
	stderrAtDrain       string
	events              []string
	exitCode            int
}

// A discarded-restore mutation uses the same observer as the positive controls.
func observeMonitorProcessLifecycle(t *testing.T, commandErr error, setupErr error, discardRestore bool) monitorProcessObservation {
	t.Helper()
	var stdout, stderr bytes.Buffer
	var events []string
	drainStarted := make(chan struct{})
	releaseDrain := make(chan struct{})
	result := make(chan int, 1)
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseDrain) }) }
	t.Cleanup(release)
	scrubLogs := func() (func(), error) {
		events = append(events, "install")
		restore := func() {
			events = append(events, "drain-start")
			close(drainStarted)
			<-releaseDrain
			events = append(events, "drain-finish")
		}
		if discardRestore {
			return func() {}, setupErr
		}
		return restore, setupErr
	}
	go func() {
		result <- runMonitorProcess(func() error {
			events = append(events, "command")
			_, err := io.WriteString(&stdout, "synthetic command output")
			return errors.Join(commandErr, err)
		}, &stderr, scrubLogs)
	}()
	var observation monitorProcessObservation
	select {
	case observation.exitCode = <-result:
		observation.returnedBeforeDrain = true
	case <-drainStarted:
		// The command cannot progress past this boundary until released.
		observation.stdoutAtDrain = stdout.String()
		observation.stderrAtDrain = stderr.String()
		release()
		observation.exitCode = <-result
	}
	observation.events = events
	return observation
}

// Successful command completion must join the descriptor readers before exit.
func TestMonitorProcessDrainsBeforeSuccessfulReturn(t *testing.T) {
	got := observeMonitorProcessLifecycle(t, nil, nil, false)
	wantEvents := []string{"install", "command", "drain-start", "drain-finish"}
	if got.returnedBeforeDrain || got.exitCode != 0 ||
		got.stdoutAtDrain != "synthetic command output" || got.stderrAtDrain != "" ||
		!reflect.DeepEqual(got.events, wantEvents) {
		t.Fatalf("successful process did not drain before return: %+v", got)
	}
}

// Error diagnostics must enter the scrubbed stream before the same joined drain.
func TestMonitorProcessDrainsBeforeErrorReturn(t *testing.T) {
	got := observeMonitorProcessLifecycle(t, errors.New("synthetic command failure"), nil, false)
	wantEvents := []string{"install", "command", "drain-start", "drain-finish"}
	if got.returnedBeforeDrain || got.exitCode != 1 ||
		got.stdoutAtDrain != "synthetic command output" || got.stderrAtDrain != "synthetic command failure\n" ||
		!reflect.DeepEqual(got.events, wantEvents) {
		t.Fatalf("failed process did not drain diagnostics before return: %+v", got)
	}
}

// Setup failures retain the existing nonfatal command and exit-code contract.
func TestMonitorProcessPreservesScrubberSetupFailureBehavior(t *testing.T) {
	for _, test := range []struct {
		name       string
		commandErr error
		exitCode   int
		stderr     string
	}{
		{name: "successful command"},
		{name: "failed command", commandErr: errors.New("synthetic command failure"), exitCode: 1, stderr: "synthetic command failure\n"},
	} {
		got := observeMonitorProcessLifecycle(t, test.commandErr, errors.New("synthetic scrubber setup unavailable"), false)
		if got.returnedBeforeDrain || got.exitCode != test.exitCode || got.stderrAtDrain != test.stderr ||
			got.stdoutAtDrain != "synthetic command output" {
			t.Fatalf("%s: scrubber setup error changed command behavior: %+v", test.name, got)
		}
	}
}

// Cancellation still returns the command's status only after queued diagnostics.
func TestMonitorProcessDrainsCanceledCommand(t *testing.T) {
	got := observeMonitorProcessLifecycle(t, context.Canceled, nil, false)
	if got.returnedBeforeDrain || got.exitCode != 1 || got.stderrAtDrain != "context canceled\n" {
		t.Fatalf("cancellation bypassed the command-owned drain: %+v", got)
	}
}

// The observer rejects the exact dropped-restore mutation without a timeout.
func TestMonitorProcessLifecycleDetectsDiscardedRestore(t *testing.T) {
	for _, test := range []struct {
		name       string
		commandErr error
		exitCode   int
	}{
		{name: "successful command"},
		{name: "failed command", commandErr: errors.New("synthetic command failure"), exitCode: 1},
	} {
		got := observeMonitorProcessLifecycle(t, test.commandErr, nil, true)
		if !got.returnedBeforeDrain || got.exitCode != test.exitCode ||
			!reflect.DeepEqual(got.events, []string{"install", "command"}) {
			t.Fatalf("%s: observer accepted the discarded-restore mutation: %+v", test.name, got)
		}
	}
}

// Only a privately selected subprocess changes real process descriptors.
func TestMonitorProcessFixture(t *testing.T) {
	mode := os.Getenv("URNETWORK_MONITOR_LIFECYCLE_FIXTURE")
	switch mode {
	case "":
		return
	case "list":
		os.Args = []string{"monitor", "-list-signals"}
		main()
		os.Exit(0)
	case "invalid-format":
		os.Args = []string{"monitor", "-format", "192.0.2.23"}
		main()
		os.Exit(0)
	case "success", "failure":
		code := runMonitorProcess(func() error {
			stdout := strings.Repeat("synthetic peer 192.0.2.23\n", 8192) + "synthetic trailing peer 2001:db8::7"
			if _, err := io.WriteString(os.Stdout, stdout); err != nil {
				return err
			}
			if _, err := io.WriteString(os.Stderr, "synthetic diagnostic peer 192.0.2.23"); err != nil {
				return err
			}
			if mode == "failure" {
				return errors.New(" synthetic failure peer 2001:db8::7")
			}
			return nil
		}, os.Stderr, server.ScrubProcessLogs)
		os.Exit(code)
	default:
		t.Fatal("unknown synthetic lifecycle fixture mode")
	}
}

// Use an empty private configuration root; inherit no credentials or host inputs.
func runMonitorProcessFixture(t *testing.T, mode string) (string, string, int) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	process := exec.CommandContext(ctx, executable, "-test.run=^TestMonitorProcessFixture$", "-test.count=1")
	process.Env = []string{
		"WARP_HOME=" + t.TempDir(),
		"WARP_ENV=local",
		"URNETWORK_MONITOR_LIFECYCLE_FIXTURE=" + mode,
	}
	var stdout, stderr bytes.Buffer
	process.Stdout, process.Stderr = &stdout, &stderr
	err = process.Run()
	if ctx.Err() != nil {
		t.Fatalf("%s: lifecycle subprocess did not complete: %v", mode, ctx.Err())
	}
	exitCode := 0
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("%s: lifecycle subprocess could not run: %v", mode, err)
		}
		exitCode = exitErr.ExitCode()
	}
	return stdout.String(), stderr.String(), exitCode
}

// EOF must flush a short unterminated tail on both descriptors before real exit.
func TestMonitorProcessRealScrubberCompletenessAndPrivacy(t *testing.T) {
	for _, test := range []struct {
		mode     string
		exitCode int
		stderr   string
	}{
		{mode: "success", stderr: "synthetic diagnostic peer [scrubbed]"},
		{mode: "failure", exitCode: 1, stderr: "synthetic diagnostic peer [scrubbed] synthetic failure peer [scrubbed]\n"},
	} {
		stdout, stderr, exitCode := runMonitorProcessFixture(t, test.mode)
		wantStdout := strings.Repeat("synthetic peer [scrubbed]\n", 8192) + "synthetic trailing peer [scrubbed]"
		if exitCode != test.exitCode || stdout != wantStdout || !strings.HasSuffix(stderr, test.stderr) {
			t.Fatalf("%s: final output/status mismatch: exit=%d stdout_bytes=%d want_stdout_bytes=%d stderr_has_tail=%t",
				test.mode, exitCode, len(stdout), len(wantStdout), strings.HasSuffix(stderr, test.stderr))
		}
		for _, address := range []string{"192.0.2.23", "2001:db8::7"} {
			if strings.Contains(stdout, address) || strings.Contains(stderr, address) {
				t.Fatalf("%s: a synthetic address bypassed the scrubber", test.mode)
			}
		}
	}
}

// The actual entrypoint preserves the full registry and its ordinary error code.
func TestMonitorMainRealExitPreservesListAndErrorOutput(t *testing.T) {
	var expected bytes.Buffer
	if err := run([]string{"-list-signals"}, &expected); err != nil {
		t.Fatal(err)
	}
	stdout, _, exitCode := runMonitorProcessFixture(t, "list")
	if exitCode != 0 || stdout != expected.String() {
		t.Fatalf("list: actual entrypoint lost registered output: exit=%d bytes=%d want=%d", exitCode, len(stdout), expected.Len())
	}
	stdout, stderr, exitCode := runMonitorProcessFixture(t, "invalid-format")
	wantError := fmt.Sprintf("monitor: format %q is not supported; use markdown or jsonl\n", "[scrubbed]")
	if exitCode != 1 || stdout != "" || !strings.HasSuffix(stderr, wantError) || strings.Contains(stderr, "192.0.2.23") {
		t.Fatalf("error: actual entrypoint lost scrubbed diagnostic/status: exit=%d stdout_bytes=%d matching_error=%t",
			exitCode, len(stdout), strings.HasSuffix(stderr, wantError))
	}
}
