package server

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect"
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
			connect.AssertEqual(tb, 1, 2)
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

// TestRunReportsPanicOriginAfterExhaustion checks that retry recovery retains
// the callback stack. Without it, the final re-panic points only at TestEnv.Run
// and hides the line that actually failed.
func TestRunReportsPanicOriginAfterExhaustion(t *testing.T) {
	if os.Getenv("URNETWORK_RERUN_PANIC_CHILD") == "1" {
		retryTestEnv(0).Run(t, func(tb testing.TB) {
			panic("persistent panic")
		})
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestRunReportsPanicOriginAfterExhaustion$", "-test.v")
	cmd.Env = append(os.Environ(), "URNETWORK_RERUN_PANIC_CHILD=1")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected the child test to fail after the panic, but it passed:\n%s", out)
	}
	output := string(out)
	if !strings.Contains(output, "persistent panic") {
		t.Fatalf("expected panic value in child output:\n%s", out)
	}
	if !strings.Contains(output, "TestRunReportsPanicOriginAfterExhaustion.func1") {
		t.Fatalf("expected callback origin in child output:\n%s", out)
	}
}

func TestRedisDbCandidatesExcludeCoordinatorAndReservedDatabases(t *testing.T) {
	candidates := testRedisDbCandidates(6, 3, 0)
	expected := []int{1, 2, 4, 5}
	connect.AssertEqual(t, candidates, expected)
}

func TestRedisDbCandidatesWrapTheirStartingOffset(t *testing.T) {
	candidates := testRedisDbCandidates(6, 3, 3)
	expected := []int{4, 5, 1, 2}
	connect.AssertEqual(t, candidates, expected)
}

func TestRedisDbCandidatesHandleNegativeOffsets(t *testing.T) {
	candidates := testRedisDbCandidates(5, 2, -1)
	expected := []int{4, 1, 3}
	connect.AssertEqual(t, candidates, expected)
}

func TestRedisDatabaseLeaseRenewsWhileOwnerIsActive(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	redisResource := Vault.RequireSimpleResource("redis.yml")
	token := strconv.Itoa(os.Getpid()) + "-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	lease := acquireTestRedisDbLease(
		ctx,
		redisResource.RequireString("authority"),
		redisResource.RequireString("password"),
		redisResource.RequireInt("db"),
		token,
		0,
		180*time.Millisecond,
	)
	defer lease.release(context.Background())

	time.Sleep(550 * time.Millisecond)
	value, err := lease.client.Get(ctx, lease.key).Result()
	if err != nil {
		t.Fatalf("read renewed lease: %v", err)
	}
	if value != token {
		t.Fatalf("renewed lease owner = %q; want %q", value, token)
	}
	ttl, err := lease.client.PTTL(ctx, lease.key).Result()
	if err != nil {
		t.Fatalf("read renewed lease ttl: %v", err)
	}
	if ttl <= 0 {
		t.Fatalf("renewed lease ttl = %s; want positive", ttl)
	}
}

func TestRedisDatabaseLeaseSeparatesProcesses(t *testing.T) {
	const (
		roleEnv = "URNETWORK_REDIS_LEASE_TEST_ROLE"
		dirEnv  = "URNETWORK_REDIS_LEASE_TEST_DIR"
	)

	if role := os.Getenv(roleEnv); role != "" {
		teardown := (&TestEnv{ApplyDbMigrations: false}).setup()
		defer teardown()

		dir := os.Getenv(dirEnv)
		path := filepath.Join(dir, role)
		err := os.WriteFile(path, []byte(strconv.Itoa(RedisDb())), 0o600)
		if err != nil {
			t.Fatal(err)
		}

		deadline := time.Now().Add(10 * time.Second)
		for {
			first, firstErr := os.ReadFile(filepath.Join(dir, "first"))
			second, secondErr := os.ReadFile(filepath.Join(dir, "second"))
			if firstErr == nil && secondErr == nil {
				if string(first) == string(second) {
					t.Fatalf("parallel processes leased the same redis db %s", first)
				}
				return
			}
			if deadline.Before(time.Now()) {
				t.Fatalf(
					"timed out waiting for both lease markers: first=%v second=%v",
					firstErr,
					secondErr,
				)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	dir := t.TempDir()
	newChild := func(role string) *exec.Cmd {
		cmd := exec.Command(
			os.Args[0],
			"-test.run=^TestRedisDatabaseLeaseSeparatesProcesses$",
			"-test.v",
		)
		cmd.Env = append(
			os.Environ(),
			roleEnv+"="+role,
			dirEnv+"="+dir,
		)
		return cmd
	}

	first := newChild("first")
	second := newChild("second")
	var firstOutput strings.Builder
	var secondOutput strings.Builder
	first.Stdout = &firstOutput
	first.Stderr = &firstOutput
	second.Stdout = &secondOutput
	second.Stderr = &secondOutput

	if err := first.Start(); err != nil {
		t.Fatal(err)
	}
	if err := second.Start(); err != nil {
		t.Fatal(err)
	}
	firstErr := first.Wait()
	secondErr := second.Wait()
	if firstErr != nil {
		t.Fatalf("first lease child failed: %v\n%s", firstErr, firstOutput.String())
	}
	if secondErr != nil {
		t.Fatalf("second lease child failed: %v\n%s", secondErr, secondOutput.String())
	}
}
