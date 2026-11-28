//go:build linux

// Reproduce the test runner's stderr alias in an isolated native subprocess.
package server

import (
	"context"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// Separate output files prove alias writes and raw fd-2 writes have different owners.
func TestScrubCaptureAliasedStderrRoutesToStdout(t *testing.T) {
	stdout, err := os.CreateTemp(t.TempDir(), "synthetic-stdout")
	if err != nil {
		t.Fatal(err)
	}
	defer stdout.Close()
	stderr, err := os.CreateTemp(t.TempDir(), "synthetic-stderr")
	if err != nil {
		t.Fatal(err)
	}
	defer stderr.Close()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, executable, "-test.run=^TestScrubCaptureAliasedStderrChild$")
	command.Env = append(os.Environ(), "UR_SYNTHETIC_STDERR_ALIAS_CHILD=1")
	command.Stdout = stdout
	command.Stderr = stderr
	if err := command.Run(); err != nil {
		t.Fatal("synthetic alias child failed", err)
	}
	gotOut, err := os.ReadFile(stdout.Name())
	if err != nil {
		t.Fatal(err)
	}
	gotErr, err := os.ReadFile(stderr.Name())
	if err != nil {
		t.Fatal(err)
	}
	// Child test success also emits a PASS marker on stdout; the exact raw owner stays separate.
	if string(gotOut) != "synthetic-alias\nPASS\n" || string(gotErr) != "synthetic-raw-fd2\n" {
		t.Fatal("stderr alias and raw descriptor did not retain separate destinations")
	}
}

// Force the same alias that testing.M.Run installs in JSON mode, without scrubbing.
func TestScrubCaptureAliasedStderrChild(t *testing.T) {
	if os.Getenv("UR_SYNTHETIC_STDERR_ALIAS_CHILD") != "1" {
		t.Skip("synthetic subprocess helper")
	}
	original := os.Stderr
	os.Stderr = os.Stdout
	defer func() { os.Stderr = original }()
	if _, err := os.Stderr.Write([]byte("synthetic-alias\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := syscall.Write(2, []byte("synthetic-raw-fd2\n")); err != nil {
		t.Fatal(err)
	}
}
