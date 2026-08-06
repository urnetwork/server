package server

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/connect/v2026"
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

// testRedisLeaseConfig reads the coordinator connection settings the same way
// acquireTestRedisDbLease's callers do.
func testRedisLeaseConfig() (authority string, password string, reservedDb int) {
	redisResource := Vault.RequireSimpleResource("redis.yml")
	return redisResource.RequireString("authority"),
		redisResource.RequireString("password"),
		redisResource.RequireInt("db")
}

func testRedisLeaseToken() string {
	return strconv.Itoa(os.Getpid()) + "-" + strconv.FormatInt(time.Now().UnixNano(), 10)
}

// TestRedisDatabaseLeaseReleaseSurvivesRetriedRelease simulates the release
// script executing server-side while its response is lost on a broken
// connection: go-redis then retries the script, and the retry must read the
// released marker as success instead of misreporting the lease as not owned.
func TestRedisDatabaseLeaseReleaseSurvivesRetriedRelease(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	authority, password, reservedDb := testRedisLeaseConfig()
	lease := acquireTestRedisDbLease(
		ctx,
		authority,
		password,
		reservedDb,
		testRedisLeaseToken(),
		0,
		time.Minute,
	)

	first, err := lease.client.Eval(
		ctx,
		testRedisLeaseReleaseScript,
		[]string{lease.key, lease.releasedKey},
		lease.token,
		testRedisLeaseReleasedMarkerTtl.Milliseconds(),
	).Int64()
	if err != nil {
		t.Fatalf("first release attempt: %v", err)
	}
	if first != 1 {
		t.Fatalf("first release attempt = %d; want 1", first)
	}

	// the retried release must not panic
	lease.release(context.Background())
}

// TestRedisDatabaseLeaseAcquireClaimsOwnTokenAfterLostSetResponse simulates a
// SET NX that the server applied while the client's response was lost: the
// retried SET NX reports not-acquired even though the key holds this process's
// token, and acquisition must claim the lease instead of skipping the db.
func TestRedisDatabaseLeaseAcquireClaimsOwnTokenAfterLostSetResponse(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	authority, password, reservedDb := testRedisLeaseConfig()
	token := testRedisLeaseToken()

	client := redis.NewClient(&redis.Options{
		Addr:     authority,
		Password: password,
		DB:       testRedisLeaseCoordinatorDb,
	})
	defer client.Close()

	config, err := client.ConfigGet(ctx, "databases").Result()
	if err != nil {
		t.Fatal(err)
	}
	databaseCount, err := strconv.Atoi(config["databases"])
	if err != nil {
		t.Fatal(err)
	}

	// write the lost-response state into the first free candidate db
	preOwnedDb := -1
	for _, db := range testRedisDbCandidates(databaseCount, reservedDb, 0) {
		key := fmt.Sprintf("urnetwork:server-test:redis-db-lease:%d", db)
		acquired, err := client.SetNX(ctx, key, token, time.Minute).Result()
		if err != nil {
			t.Fatal(err)
		}
		if acquired {
			preOwnedDb = db
			break
		}
	}
	if preOwnedDb < 0 {
		t.Fatal("no free redis test db to stage the lost-response state")
	}

	// an offset of preOwnedDb-1 makes preOwnedDb the first candidate scanned
	lease := acquireTestRedisDbLease(
		ctx,
		authority,
		password,
		reservedDb,
		token,
		preOwnedDb-1,
		time.Minute,
	)
	defer lease.release(context.Background())

	if lease.db != preOwnedDb {
		t.Fatalf("acquire leased db %d; want it to claim its own token on db %d", lease.db, preOwnedDb)
	}
}

// TestRedisDatabaseLeaseReleaseDetectsForeignOwner: a lease key rewritten by
// another owner must fail release with the foreign-owner diagnostic.
func TestRedisDatabaseLeaseReleaseDetectsForeignOwner(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	authority, password, reservedDb := testRedisLeaseConfig()
	lease := acquireTestRedisDbLease(
		ctx,
		authority,
		password,
		reservedDb,
		testRedisLeaseToken(),
		0,
		time.Minute,
	)

	err := lease.client.Set(ctx, lease.key, "foreign-token", time.Minute).Err()
	if err != nil {
		t.Fatal(err)
	}

	defer func() {
		r := recover()
		// release closes lease.client before panicking; clean the stomped
		// key with a fresh client so the db frees up for concurrent runs
		cleanup := redis.NewClient(&redis.Options{
			Addr:     authority,
			Password: password,
			DB:       testRedisLeaseCoordinatorDb,
		})
		cleanup.Del(context.Background(), lease.key)
		cleanup.Close()
		if r == nil {
			t.Fatal("expected release to panic for a foreign-owned lease")
		}
		if !strings.Contains(fmt.Sprint(r), "owned by another process") {
			t.Fatalf("expected foreign-owner diagnostic, got: %v", r)
		}
	}()
	lease.release(context.Background())
}

// TestRedisDatabaseLeaseReleaseDetectsMissingKey: a lease key that vanished
// entirely (flush, expiry) must still fail release with the not-owned
// diagnostic.
func TestRedisDatabaseLeaseReleaseDetectsMissingKey(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	authority, password, reservedDb := testRedisLeaseConfig()
	lease := acquireTestRedisDbLease(
		ctx,
		authority,
		password,
		reservedDb,
		testRedisLeaseToken(),
		0,
		time.Minute,
	)

	if err := lease.client.Del(ctx, lease.key).Err(); err != nil {
		t.Fatal(err)
	}

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected release to panic for a missing lease key")
		}
		if !strings.Contains(fmt.Sprint(r), "was not owned during release") {
			t.Fatalf("expected not-owned diagnostic, got: %v", r)
		}
	}()
	lease.release(context.Background())
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

		// The peer has to finish a whole TestEnv setup (redis lease + pg
		// database creation) before it can write its marker, and that cost is
		// not bounded by anything this test controls: it is ~1.2s idle but was
		// observed at ~30s during a loaded -race suite against a pg carrying
		// abandoned test databases. This budget only has to be longer than a
		// pathological setup — the parent still bounds the test overall — so
		// keep it generous. A tight budget here fails the run for being slow,
		// which says nothing about lease separation.
		started := time.Now()
		deadline := started.Add(90 * time.Second)
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
					"timed out after %s waiting for both lease markers: first=%v second=%v",
					time.Since(started),
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
