package server

import (
	"os"
	"os/exec"
	"sync/atomic"
	"testing"

	"github.com/go-playground/assert/v2"
)

// These tests exercise the real TestEnv.Run, including its per-attempt
// environment setup/teardown, to prove that a flaky failure on early attempts
// is retried and rescued, while a failure that persists across every attempt
// still fails the test.
//
// ApplyDbMigrations is false (the callbacks touch no schema) and RerunTimeout is
// zero so the reruns happen back-to-back.

func retryTestEnv(rerunCount int) *TestEnv {
	return &TestEnv{
		ApplyDbMigrations: false,
		RerunCount:        rerunCount,
		RerunTimeout:      0,
	}
}

// TestRunRetriesUntilPass proves every failure mode is retried: the first
// attempt panics, the second fails (t.Fail), the third fails an assertion
// (assert.Equal, which calls FailNow -> runtime.Goexit), and the fourth passes.
// Because each failure is recorded only on the retryTB wrapper, the real
// *testing.T is never failed and the overall test passes.
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

// TestRunFailsAfterExhaustion proves the retry does not silently swallow real
// failures: a failure that persists across every attempt must still fail the
// test. Run calls t.FailNow once the reruns are exhausted, so we run that case
// in a subprocess and require it to exit non-zero.
func TestRunFailsAfterExhaustion(t *testing.T) {
	if os.Getenv("URNETWORK_RERUN_EXHAUSTION_CHILD") == "1" {
		// Child process: always fails, so Run should exhaust its reruns and fail
		// this test.
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
