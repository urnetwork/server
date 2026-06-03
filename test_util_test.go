package server

import (
	"os"
	"os/exec"
	"sync/atomic"
	"testing"

	"github.com/go-playground/assert/v2"
)

// These tests exercise the real TestEnv.Run (including per-attempt environment
// setup/teardown): a flaky failure on early attempts is retried and rescued,
// while a failure that persists across every attempt still fails the test.
//
// ApplyDbMigrations is false (callbacks touch no schema) and RerunTimeout is
// zero so reruns happen back-to-back.

func retryTestEnv(rerunCount int) *TestEnv {
	return &TestEnv{
		ApplyDbMigrations: false,
		RerunCount:        rerunCount,
		RerunTimeout:      0,
	}
}

// TestRunRetriesUntilPass checks every failure mode is retried: attempt 1
// panics, 2 calls t.Fail, 3 fails an assertion (assert.Equal -> FailNow ->
// runtime.Goexit), and 4 passes. Each failure is recorded only on the retryTB
// wrapper, so the real *testing.T never fails and the test passes.
func TestRunRetriesUntilPass(t *testing.T) {
	var attempts atomic.Int32
	retryTestEnv(3).Run(t, func(tb testing.TB) {
		switch attempts.Add(1) {
		case 1:
			panic("flaky panic on the first attempt")
		case 2:
			tb.Fail()
		case 3:
			assert.Equal(tb, 1, 2)
		}
		// the fourth attempt falls through and passes
	})
	if got := attempts.Load(); got != 4 {
		t.Fatalf("expected 4 attempts before success, got %d", got)
	}
}

// TestRunFailsAfterExhaustion checks retry does not swallow real failures: a
// failure that persists across every attempt must still fail the test. Run
// calls t.FailNow once reruns are exhausted, so we run that case in a subprocess
// and require it to exit non-zero.
func TestRunFailsAfterExhaustion(t *testing.T) {
	if os.Getenv("URNETWORK_RERUN_EXHAUSTION_CHILD") == "1" {
		// Child process: always fails, so Run exhausts its reruns and fails.
		retryTestEnv(1).Run(t, func(tb testing.TB) {
			tb.Fatal("persistent failure")
		})
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestRunFailsAfterExhaustion$", "-test.v")
	cmd.Env = append(os.Environ(), "URNETWORK_RERUN_EXHAUSTION_CHILD=1")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected the child test to fail after exhausting reruns, but it passed:\n%s", out)
	}
	t.Logf("child test failed after exhausting reruns, as expected:\n%s", out)
}
