//go:build linux

package server

// Diagnostic controls exercise the real Run refusal and descriptor metadata.
// They create no process, cgroup, network connection or service namespace.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Missing readiness must expose the already-completed actual Run result, not
// only EOF. The invalid original join budget refuses before any capability I/O.
func TestTestProcessReadinessEOFReportsCompletedRunRefusal(t *testing.T) {
	deadline, ok := t.Deadline()
	if !ok {
		t.Fatal("diagnostic control requires the original test deadline")
	}
	ctx, cancel := context.WithDeadline(t.Context(), deadline)
	defer cancel()
	owner := &TestProcessCgroup{admission: make(chan struct{}, 1)}
	owner.admission <- struct{}{}
	fixture := &testProcessExecutionFixture{
		owner: owner, ctx: ctx, cancel: cancel, deadline: deadline,
		resultChannel: make(chan testProcessExecutionOutcome, 1),
		spec:          TestProcessSpec{JoinReserve: 0, Parallel: 1, WorkingDirectory: t.TempDir()},
	}
	t.Cleanup(func() {
		cancel()
		if fixture.input != nil {
			fixture.input.Close()
		}
		if fixture.output != nil {
			fixture.output.Close()
		}
		if fixture.started && !fixture.finished {
			fixture.finish(t)
		}
	})
	fixture.start(t, "prelaunch-refusal")
	value, err := fixture.readEvent("COLD_RESOURCE_READY")
	if value != "" || err == nil ||
		!strings.Contains(err.Error(), "run_result={Started:false Joined:false PID:0") ||
		!strings.Contains(err.Error(), "run_error=owned test process requires a root, absolute directory and original deadline with join reserve") {
		t.Fatalf("readiness EOF discarded actual Run admission failure: value=%q error=%v", value, err)
	}
	if !fixture.finished || fixture.outcome.err == nil || fixture.outcome.result.Started ||
		fixture.outcome.result.Joined || owner.used || owner.job != nil {
		t.Fatalf("prelaunch diagnostic control manufactured process effects: result=%+v error=%v used=%t job=%v", fixture.outcome.result, fixture.outcome.err, owner.used, owner.job != nil)
	}
}

// Exact source metadata is part of executable admission, independently of its
// digest. Copy/build permissions must be fixed on a fresh fixture before use.
func TestTestProcessExecutableRefusesWritableSourceAuthority(t *testing.T) {
	value := []byte("owned descriptor metadata control")
	path := filepath.Join(t.TempDir(), "executable")
	if err := os.WriteFile(path, value, 0o750); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := file.Close(); err != nil {
			t.Error(err)
		}
	})
	hash := sha256.Sum256(value)
	expected := hex.EncodeToString(hash[:])
	for _, mode := range []os.FileMode{0o770, 0o775, 0o702} {
		if err := file.Chmod(mode); err != nil {
			t.Fatal(err)
		}
		sealed, err := sealTestProcessExecutable(file, int64(len(value)), expected)
		if sealed != nil {
			sealed.Close()
			t.Fatalf("writable source acquired a sealed executable: mode=%o error=%v", mode, err)
		}
		if err == nil || err.Error() != "owned test executable metadata differs" {
			t.Fatalf("writable source did not fail at its metadata boundary: mode=%o error=%v", mode, err)
		}
		if err := file.Chmod(0o750); err != nil {
			t.Fatal(err)
		}
		if err := verifyTestProcessExecutable(file, int64(len(value)), expected); err != nil {
			t.Fatalf("private source control did not retain its exact bytes: %v", err)
		}
	}
}
