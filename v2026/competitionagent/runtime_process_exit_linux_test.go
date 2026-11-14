//go:build linux

// Census races are forced with explicit state transitions, not timing sleeps.
package competitionagent

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"testing"
)

// An observed helper can exit between the owned census and its first proc read.
// Live, reused, escaped and unobservable process identities never waive a fault.
func TestRuntimeBoundaryProcessExitReconciliation(t *testing.T) {
	observationErr := &os.PathError{Op: "readlink", Path: "/proc/12345/exe", Err: os.ErrNotExist}
	for _, c := range []struct {
		exited    bool
		exitErr   error
		wantError bool
	}{
		{exited: true},
		{wantError: true},
		{exited: true, exitErr: errors.New("synthetic pidfd permission refusal"), wantError: true},
	} {
		calls := 0
		err := reconcileRuntimeBoundaryProcessExit(observationErr, 12345, func(pid int) (bool, error) {
			calls++
			if pid != 12345 {
				t.Fatal("checked another process")
			}
			return c.exited, c.exitErr
		})
		if (err != nil) != c.wantError || calls != 1 || c.wantError && !errors.Is(err, observationErr) {
			t.Fatal("helper lifetime was not reconciled conservatively", err, calls)
		}
	}
	if err := reconcileRuntimeBoundaryProcessExit(nil, 12345, func(int) (bool, error) { t.Fatal("healthy proof checked an unrelated later exit"); return false, nil }); err != nil {
		t.Fatal(err)
	}
}

// Positive exit cannot erase a real security violation observed before exit.
func TestRuntimeBoundaryProcessExitPreservesSecurityFailures(t *testing.T) {
	for _, observationErr := range []error{errors.New("synthetic unpinned executable"), errors.New("synthetic escaped namespace"), os.ErrPermission,
		&os.PathError{Op: "open", Path: "/synthetic/runtime/socket", Err: os.ErrNotExist},
		&os.PathError{Op: "open", Path: "/proc/54321/exe", Err: os.ErrNotExist},
	} {
		calls := 0
		err := reconcileRuntimeBoundaryProcessExit(observationErr, 12345, func(int) (bool, error) { calls++; return true, nil })
		if !errors.Is(err, observationErr) || calls != 0 {
			t.Fatal("exit erased an observed policy failure", err, calls)
		}
	}
}

// The child blocks on its pipe until the parent explicitly allows termination.
func TestRuntimeBoundaryProcessExitChild(t *testing.T) {
	if os.Getenv("SYNTHETIC_PROCESS_EXIT_CHILD") != "1" {
		return
	}
	_, _ = io.Copy(io.Discard, os.Stdin)
	os.Exit(0)
}

// Actual pidfds distinguish a live child from its positively completed lifetime.
func TestRuntimeBoundaryNativeProcessExit(t *testing.T) {
	command := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestRuntimeBoundaryProcessExitChild$")
	command.Env = []string{"SYNTHETIC_PROCESS_EXIT_CHILD=1"}
	input, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = input.Close(); _ = command.Process.Kill(); _ = command.Wait() }()
	if exited, err := runtimeBoundaryProcessExited(command.Process.Pid); err != nil || exited {
		t.Fatal("live child was retired", exited, err)
	}
	if err := input.Close(); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err != nil {
		t.Fatal(err)
	}
	if exited, err := runtimeBoundaryProcessExited(command.Process.Pid); err != nil || !exited {
		t.Fatal("completed child was not retired", exited, err)
	}
	if exited, err := runtimeBoundaryProcessExited(0); err == nil || exited {
		t.Fatal("invalid identity proved exit")
	}
}
