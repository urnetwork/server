// Pure synthetic process controls; these run no gate, DB or Main command.
package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Explicit evidence paths retain operational controls; ordinary Go tests use
// their standard temporary directory and the guard in this package directory.
func controlPaths(t *testing.T) (string, string) {
	t.Helper()
	source := os.Getenv("CAPACITY_GUARD_SOURCE")
	if source == "" {
		var err error
		source, err = filepath.Abs("guard.sh")
		if err != nil {
			t.Fatal("guard source path")
		}
	}
	if !filepath.IsAbs(source) {
		t.Fatal("absolute guard source required")
	}
	evidence := os.Getenv("CAPACITY_GUARD_EVIDENCE")
	if evidence == "" {
		evidence = t.TempDir()
	} else if !strings.HasPrefix(evidence, "/run/media/by/data/urnetwork/temp/") || filepath.Clean(evidence) != evidence {
		t.Fatal("private evidence prerequisite")
	}
	return source, evidence
}

// Private markers witness whether each command actually executed.
func TestFailClosedLaunch(t *testing.T) {
	source, evidence := controlPaths(t)
	for _, c := range []struct {
		name       string
		preflight  int
		launch     int
		wantLaunch bool
		wantExit   int
	}{
		{name: "red", preflight: 1, launch: 0, wantExit: 1},
		{name: "preflight_error", preflight: 7, launch: 0, wantExit: 7},
		{name: "green", preflight: 0, launch: 0, wantLaunch: true, wantExit: 0},
		{name: "launch_error", preflight: 0, launch: 19, wantLaunch: true, wantExit: 19},
	} {
		dir, err := os.MkdirTemp(evidence, c.name+"-")
		if err != nil {
			t.Fatal("private fixture allocation")
		}
		preflightMarker := filepath.Join(dir, "preflight")
		launchMarker := filepath.Join(dir, "launch")
		argvMarker := filepath.Join(dir, "argv")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		preflightCode := "0"
		if c.preflight == 1 {
			preflightCode = "1"
		}
		if c.preflight == 7 {
			preflightCode = "7"
		}
		launchCode := "0"
		if c.launch == 19 {
			launchCode = "19"
		}
		argv := []string{source, "--preflight", "bash", "-c", `printf 'preflight\n' >>"$1"; exit "$2"`, "synthetic", preflightMarker, preflightCode, "--launch", "bash", "-c", `printf 'launch\n' >>"$1"; destination=$2; code=$3; shift 3; printf '%s\0' "$@" >"$destination"; exit "$code"`, "synthetic", launchMarker, argvMarker, launchCode, "space value", "", "literal$(never-execute)"}
		cmd := exec.CommandContext(ctx, "bash", argv...)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
		cmd.WaitDelay = 2 * time.Second
		diagnostics, runErr := cmd.CombinedOutput()
		expired := ctx.Err() == context.DeadlineExceeded
		cancel()
		if os.WriteFile(filepath.Join(dir, "diagnostics"), diagnostics, 0600) != nil {
			t.Fatal("diagnostic retention")
		}
		if expired {
			t.Fatal("synthetic command timed out")
		}
		exit := 0
		if runErr != nil {
			var status *exec.ExitError
			if !errors.As(runErr, &status) {
				t.Fatal("synthetic transport error")
			}
			exit = status.ExitCode()
		}
		if exit != c.wantExit {
			t.Fatalf("%s exit propagation", c.name)
		}
		preflight, err := os.ReadFile(preflightMarker)
		if err != nil || string(preflight) != "preflight\n" {
			t.Fatalf("%s preflight count", c.name)
		}
		launch, err := os.ReadFile(launchMarker)
		if !c.wantLaunch {
			if !os.IsNotExist(err) {
				t.Fatalf("%s launched after failure", c.name)
			}
			continue
		}
		if err != nil || string(launch) != "launch\n" {
			t.Fatalf("%s launch not exactly once", c.name)
		}
		observed, err := os.ReadFile(argvMarker)
		if err != nil || !bytes.Equal(observed, []byte("space value\x00\x00literal$(never-execute)\x00")) {
			t.Fatal("launch argv changed")
		}
	}
}

// The pre-fix newline launches even though the preceding preflight exits 7.
func TestOldNewlineContinuesAfterPreflightFailure(t *testing.T) {
	_, evidence := controlPaths(t)
	dir, err := os.MkdirTemp(evidence, "old-newline-")
	if err != nil {
		t.Fatal("private fixture allocation")
	}
	marker := filepath.Join(dir, "launch")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", "-c", "bash -c 'exit 7'\nprintf 'launch\\n' >\"$1\"\n", "synthetic", marker)
	cmd.WaitDelay = 2 * time.Second
	if cmd.Run() != nil || ctx.Err() != nil {
		t.Fatal("old newline control did not reach launcher")
	}
	observed, err := os.ReadFile(marker)
	if err != nil || string(observed) != "launch\n" {
		t.Fatal("old newline failure not reproduced")
	}
}
