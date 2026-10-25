// Reproduces privileged preflight versus unprivileged manager access using
// synthetic files only. Execute this Linux integration test as root.
package reproduction

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

// A readable privileged hash says nothing about an unprivileged caller's
// ability to traverse the same private directory.
func TestCalibrationPrivateLeafRequiresMatchingCaller(t *testing.T) {
	binary, target := calibrationPrincipalFixture(t, 0700)
	calibrationPrincipalRead(t, binary, target, 0, "read", 0)
	calibrationPrincipalRead(t, binary, target, 65534, "read", 3)
}

// Metadata visibility is not a substitute for opening the actual input under
// the evaluator's inherited credentials.
func TestCalibrationVisibleMetadataDoesNotProveReadAccess(t *testing.T) {
	binary, target := calibrationPrincipalFixture(t, 0755)
	calibrationPrincipalRead(t, binary, target, 0, "stat-read", 0)
	calibrationPrincipalRead(t, binary, target, 65534, "stat-read", 3)
}

// A separate process returns exact permission denial, not a timing inference.
// A failed executable launch is distinguished from a denied fixture read.
func TestCalibrationPrincipalReadChild(t *testing.T) {
	if os.Getenv("CALIBRATION_PRINCIPAL_CHILD") != "1" {
		return
	}
	target := os.Getenv("CALIBRATION_PRINCIPAL_TARGET")
	if os.Getenv("CALIBRATION_PRINCIPAL_MODE") == "stat-read" {
		if _, err := os.Stat(target); err != nil {
			os.Exit(4)
		}
	}
	value, err := os.ReadFile(target)
	if errors.Is(err, os.ErrPermission) {
		os.Exit(3)
	}
	if err != nil || string(value) != "synthetic-fixture-only\n" {
		os.Exit(4)
	}
	os.Exit(0)
}

// Keeps the copied helper executable reachable by the synthetic unprivileged
// uid while independently controlling the protected input boundary.
func calibrationPrincipalFixture(t *testing.T, directoryMode os.FileMode) (string, string) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Fatal("principal reproduction must execute as root, not be silently skipped")
	}
	root, err := os.MkdirTemp("/tmp", "calibration-principal-synthetic-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Error(err)
		}
	})
	if err := os.Chmod(root, 0755); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	input, err := os.Open(executable)
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	binary := filepath.Join(root, "principal-probe")
	output, err := os.OpenFile(binary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0755)
	if err != nil {
		t.Fatal(err)
	}
	_, copyErr := io.Copy(output, input)
	closeErr := output.Close()
	if copyErr != nil || closeErr != nil {
		t.Fatalf("helper copy failed: %v / %v", copyErr, closeErr)
	}
	protected := filepath.Join(root, "protected")
	if err := os.Mkdir(protected, directoryMode); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(protected, "fixture.txt")
	if err := os.WriteFile(target, []byte("synthetic-fixture-only\n"), 0400); err != nil {
		t.Fatal(err)
	}
	return binary, target
}

// Launches the identical helper with explicit credentials and no inherited
// operator environment. The deadline is a failure guard, not the proof.
func calibrationPrincipalRead(t *testing.T, binary, target string, uid uint32, mode string, wantExit int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, binary, "-test.run=^TestCalibrationPrincipalReadChild$")
	command.Env = []string{
		"CALIBRATION_PRINCIPAL_CHILD=1",
		"CALIBRATION_PRINCIPAL_TARGET=" + target,
		"CALIBRATION_PRINCIPAL_MODE=" + mode,
	}
	command.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{Uid: uid, Gid: uid, Groups: []uint32{}},
	}
	output, err := command.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("principal probe exceeded its guard: %v", ctx.Err())
	}
	exitCode := 0
	if err != nil {
		var exitError *exec.ExitError
		if !errors.As(err, &exitError) {
			t.Fatalf("helper did not execute: %v / %s", err, output)
		}
		exitCode = exitError.ExitCode()
	}
	if exitCode != wantExit {
		t.Fatalf("uid %d mode %s: exit %d, want %d: %s", uid, mode, exitCode, wantExit, output)
	}
}
