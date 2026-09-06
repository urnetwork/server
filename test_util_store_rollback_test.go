package server

// Keeps service effects hermetic while exercising real Vault overrides,
// rollback, retry routing, and completion ownership at explicit failure points.

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
)

// A route observation reads actual resources, not a modeled cleanup result.
type testEnvStoreRoutes struct {
	postgres    string
	maintenance string
	redis       int
}

// The fixture's original database remains reserved throughout failed setup.
func originalTestEnvStoreRoutes() testEnvStoreRoutes {
	return testEnvStoreRoutes{postgres: "owned-base-pg", maintenance: "owned-base-maintenance", redis: 1}
}

// No client or database operation is invoked by this resource-only read.
func currentTestEnvStoreRoutes() testEnvStoreRoutes {
	return testEnvStoreRoutes{
		postgres:    Vault.RequireSimpleResource(DefaultPgVaultResourceName).RequireString("db"),
		maintenance: Vault.RequireSimpleResource(MaintenancePgVaultResourceName).RequireString("db"),
		redis:       RedisDb(),
	}
}

// Each instance has one sequential owner. Goroutine tests join that owner
// before reading these observations or restoring their private resolver.
type testEnvStoreRollbackFixture struct {
	env           *TestEnv
	effects       []string
	calls         map[string]int
	releaseRoutes []testEnvStoreRoutes
	fault         func(string, int) any
}

// Reuses the unchanged neutral fixture's test-owned resolver and credentials.
func newTestEnvStoreRollbackFixture(t *testing.T) *testEnvStoreRollbackFixture {
	t.Helper()
	neutral := newTestEnvStoreLifecycleFixture(t)
	neutral.env.Warmup = true
	return &testEnvStoreRollbackFixture{env: neutral.env, calls: map[string]int{}}
}

// Records an actual driver boundary before raising a call-local owned fault.
func (self *testEnvStoreRollbackFixture) effect(operation string) {
	self.effects = append(self.effects, operation)
	self.calls[operation]++
	if self.fault != nil {
		if failure := self.fault(operation, self.calls[operation]); failure != nil {
			panic(failure)
		}
	}
}

// Intercepts all service closures, including pool reset and optional warmup.
// Lease release records the real routes before the observer may itself fail.
func (self *testEnvStoreRollbackFixture) setup(database int) func() {
	pg := Vault.RequireSimpleResource(DefaultPgVaultResourceName).Parse()
	maintenance := Vault.RequireSimpleResource(MaintenancePgVaultResourceName).Parse()
	resource := Vault.RequireSimpleResource("redis.yml")
	return self.env.setupWithAcquiredStores(
		context.Background(), pg, maintenance, fmt.Sprintf("test_123_%032x", database),
		resource.RequireString("authority"), resource.RequireString("password"), database,
		func() {
			self.releaseRoutes = append(self.releaseRoutes, currentTestEnvStoreRoutes())
			self.effect("lease-release")
		},
		func(operation string, _ func()) { self.effect(operation) },
	)
}

// A panic is returned without changing its identity. Goexit tests instead
// use an explicitly joined owner goroutine.
func recoverTestEnvStoreFailure(callback func()) (failure any) {
	defer func() { failure = recover() }()
	callback()
	return nil
}

// Route restoration must precede every attempt to release lease ownership.
func requireTestEnvStoreRoutesRestored(t *testing.T, fixture *testEnvStoreRollbackFixture, releases int) {
	t.Helper()
	want := originalTestEnvStoreRoutes()
	if got := currentTestEnvStoreRoutes(); got != want {
		t.Fatalf("actual resource routes not restored: got=%+v want=%+v", got, want)
	}
	if len(fixture.releaseRoutes) != releases || fixture.calls["lease-release"] != releases {
		t.Fatalf("lease release count: routes=%v calls=%v", fixture.releaseRoutes, fixture.calls)
	}
	for _, routes := range fixture.releaseRoutes {
		if routes != want {
			t.Fatalf("lease released before route restoration: %+v", routes)
		}
	}
}

// Every partial acquisition stage must undo only its already-owned resources;
// a failed setup does not acquire new authority to flush or drop a database.
func TestTestEnvStoreRollbackEveryPartialStage(t *testing.T) {
	for _, stage := range []string{"pg-reap", "pg-create", "pg-reset", "redis-reset", "redis-flush", "pg-migrate", "warmup"} {
		fixture := newTestEnvStoreRollbackFixture(t)
		sentinel := fmt.Errorf("owned setup failure at %s", stage)
		fixture.fault = func(operation string, ordinal int) any {
			if operation == stage && ordinal == 1 {
				return sentinel
			}
			return nil
		}
		var teardown func()
		failure := recoverTestEnvStoreFailure(func() { teardown = fixture.setup(2) })
		if failure != sentinel || teardown != nil || fixture.calls[stage] == 0 {
			t.Fatalf("%s did not fail at its exact boundary: panic=%v teardown=%t calls=%v", stage, failure, teardown != nil, fixture.calls)
		}
		requireTestEnvStoreRoutesRestored(t, fixture, 1)
		if fixture.calls["pg-drop"] != 0 || fixture.calls["redis-flush"] > 1 {
			t.Fatalf("%s added destructive rollback effects: %v", stage, fixture.effects)
		}
		wantReset := 0
		if stage == "warmup" {
			wantReset = 1
		}
		if fixture.calls["reset"] != wantReset {
			t.Fatalf("%s partial warmup cleanup: calls=%v", stage, fixture.calls)
		}
	}
}

// Preserving an error value is not enough: callers may raise a non-error
// sentinel, and clean rollback must rethrow that same object.
func TestTestEnvStoreRollbackPreservesNonErrorPanic(t *testing.T) {
	fixture := newTestEnvStoreRollbackFixture(t)
	sentinel := &struct{ message string }{message: "owned non-error setup panic"}
	fixture.fault = func(operation string, _ int) any {
		if operation == "pg-create" {
			return sentinel
		}
		return nil
	}
	failure := recoverTestEnvStoreFailure(func() { fixture.setup(2) })
	if failure != sentinel {
		t.Fatalf("non-error setup panic changed identity: %v", failure)
	}
	requireTestEnvStoreRoutesRestored(t, fixture, 1)
}

// A failing restorer must not hide the setup failure or prevent later route
// restoration and release. These are real driver callbacks, not fake pops.
func TestTestEnvStoreRollbackRetainsCleanupFailures(t *testing.T) {
	for _, stage := range []string{"redis-reset", "pg-reset", "lease-release"} {
		fixture := newTestEnvStoreRollbackFixture(t)
		primary := errors.New("owned migration failure")
		secondary := fmt.Errorf("owned cleanup failure at %s", stage)
		fixture.fault = func(operation string, ordinal int) any {
			if operation == "pg-migrate" && ordinal == 1 {
				return primary
			}
			wantOrdinal := 2
			if stage == "lease-release" {
				wantOrdinal = 1
			}
			if operation == stage && ordinal == wantOrdinal {
				return secondary
			}
			return nil
		}
		failure := recoverTestEnvStoreFailure(func() { fixture.setup(2) })
		err, ok := failure.(error)
		if !ok || !errors.Is(err, primary) || !errors.Is(err, secondary) {
			t.Fatalf("%s cleanup erased a failure: %v", stage, failure)
		}
		requireTestEnvStoreRoutesRestored(t, fixture, 1)
		if fixture.calls["pg-reset"] != 2 || fixture.calls["redis-reset"] != 2 || fixture.calls["pg-drop"] != 0 {
			t.Fatalf("%s cleanup did not drain restorations: %v", stage, fixture.calls)
		}
	}
}

// All independent failures remain discoverable even when warmup, reset,
// both pool resets, and release fail during the same unwind.
func TestTestEnvStoreRollbackJoinsAllCleanupFailures(t *testing.T) {
	fixture := newTestEnvStoreRollbackFixture(t)
	const primary = "owned warmup non-error panic"
	cleanupFailures := map[string]error{
		"reset":         errors.New("owned warmup reset failure"),
		"redis-reset":   errors.New("owned redis reset failure"),
		"pg-reset":      errors.New("owned postgres reset failure"),
		"lease-release": errors.New("owned lease release failure"),
	}
	fixture.fault = func(operation string, ordinal int) any {
		if operation == "warmup" {
			return primary
		}
		wantOrdinal := 1
		if operation == "redis-reset" || operation == "pg-reset" {
			wantOrdinal = 2
		}
		if ordinal == wantOrdinal {
			return cleanupFailures[operation]
		}
		return nil
	}
	failure := recoverTestEnvStoreFailure(func() { fixture.setup(2) })
	err, ok := failure.(error)
	if !ok || !strings.Contains(err.Error(), primary) {
		t.Fatalf("original non-error setup failure disappeared: %v", failure)
	}
	for stage, cause := range cleanupFailures {
		if !errors.Is(err, cause) {
			t.Fatalf("%s cleanup cause disappeared: %v", stage, err)
		}
	}
	requireTestEnvStoreRoutesRestored(t, fixture, 1)
	if fixture.calls["reset"] != 1 || fixture.calls["pg-reset"] != 2 || fixture.calls["redis-reset"] != 2 {
		t.Fatalf("multi-failure unwind did not drain every callback: %v", fixture.calls)
	}
}

// The retry uses the real candidate function after rollback and completes a
// second lifecycle without ever changing which original Redis DB is reserved.
func TestTestEnvStoreRollbackRetryKeepsReservedRoute(t *testing.T) {
	fixture := newTestEnvStoreRollbackFixture(t)
	sentinel := errors.New("owned first-attempt migration failure")
	fixture.fault = func(operation string, ordinal int) any {
		if operation == "pg-migrate" && ordinal == 1 {
			return sentinel
		}
		return nil
	}
	if failure := recoverTestEnvStoreFailure(func() { fixture.setup(2) }); failure != sentinel {
		t.Fatalf("wrong first-attempt failure: %v", failure)
	}
	candidates := testRedisDbCandidates(3, RedisDb(), 0)
	if !reflect.DeepEqual(candidates, []int{2}) {
		t.Fatalf("retry candidate no longer excludes original reserved DB: %v", candidates)
	}
	teardown := fixture.setup(candidates[0])
	teardown()
	requireTestEnvStoreRoutesRestored(t, fixture, 2)
}

// Notification configuration was and remains best-effort; it must not turn
// an otherwise successful setup into a new rollback path.
func TestTestEnvStoreRollbackKeepsOptionalNotificationSemantics(t *testing.T) {
	fixture := newTestEnvStoreRollbackFixture(t)
	fixture.fault = func(operation string, _ int) any {
		if operation == "redis-notifications" {
			return errors.New("owned optional notification failure")
		}
		return nil
	}
	teardown := fixture.setup(2)
	if RedisDb() != 2 || fixture.calls["redis-notifications"] != 1 || len(fixture.releaseRoutes) != 0 {
		t.Fatalf("optional notification changed successful setup: effects=%v", fixture.effects)
	}
	teardown()
	requireTestEnvStoreRoutesRestored(t, fixture, 1)
	want := []string{
		"pg-reap", "pg-create", "pg-reset", "redis-reset", "redis-flush", "pg-migrate", "warmup", "redis-notifications",
		"reset", "redis-flush", "redis-reset", "pg-reset", "pg-drop", "lease-release",
	}
	if !reflect.DeepEqual(fixture.effects, want) {
		t.Fatalf("successful lifecycle effect order changed: got=%v want=%v", fixture.effects, want)
	}
}

// Failure anywhere in teardown still restores both routes before release;
// a completed repeat rethrows the same failure without another side effect.
func TestTestEnvStoreTeardownDrainsEveryFailure(t *testing.T) {
	for _, stage := range []string{"reset", "redis-flush", "redis-reset", "pg-reset", "pg-drop", "lease-release"} {
		fixture := newTestEnvStoreRollbackFixture(t)
		teardown := fixture.setup(2)
		sentinel := fmt.Errorf("owned teardown failure at %s", stage)
		failOrdinal := fixture.calls[stage] + 1
		fixture.fault = func(operation string, ordinal int) any {
			if operation == stage && ordinal == failOrdinal {
				return sentinel
			}
			return nil
		}
		failure := recoverTestEnvStoreFailure(teardown)
		if failure != sentinel {
			t.Fatalf("%s teardown changed the sole failure: %v", stage, failure)
		}
		requireTestEnvStoreRoutesRestored(t, fixture, 1)
		before := append([]string(nil), fixture.effects...)
		if repeated := recoverTestEnvStoreFailure(teardown); repeated != sentinel {
			t.Fatalf("%s completed teardown lost its failure: %v", stage, repeated)
		}
		if !reflect.DeepEqual(fixture.effects, before) {
			t.Fatalf("%s completed teardown reran effects: before=%v after=%v", stage, before, fixture.effects)
		}
	}
}

// A former cleanup closure must not pop a successor's reused override IDs or
// dispatch another flush/drop after its own lifecycle has already completed.
func TestTestEnvStoreTeardownOneShotPreservesSuccessor(t *testing.T) {
	fixture := newTestEnvStoreRollbackFixture(t)
	firstTeardown := fixture.setup(2)
	firstTeardown()
	secondTeardown := fixture.setup(3)
	successor := currentTestEnvStoreRoutes()
	before := append([]string(nil), fixture.effects...)
	firstTeardown()
	if successor.redis != 3 || currentTestEnvStoreRoutes() != successor || !reflect.DeepEqual(before, fixture.effects) || len(fixture.releaseRoutes) != 1 {
		t.Fatalf("completed teardown changed its successor: routes=%+v effects=%v", currentTestEnvStoreRoutes(), fixture.effects)
	}
	secondTeardown()
	requireTestEnvStoreRoutesRestored(t, fixture, 2)
}

// Pool reset can fail before the paired helper returns its cleanup closure.
// Both already-pushed routes still belong to that failed constructor.
func TestTestEnvStorePgPairInitialResetFailureRestoresRoutes(t *testing.T) {
	newTestEnvStoreLifecycleFixture(t)
	pg := Vault.RequireSimpleResource(DefaultPgVaultResourceName).Parse()
	maintenance := Vault.RequireSimpleResource(MaintenancePgVaultResourceName).Parse()
	sentinel := errors.New("owned initial postgres pool reset failure")
	var resetRoutes []testEnvStoreRoutes
	var cleanup func()
	failure := recoverTestEnvStoreFailure(func() {
		cleanup = pushTestPgResourcesWithReset(pg, maintenance, "owned-pair", func() {
			resetRoutes = append(resetRoutes, currentTestEnvStoreRoutes())
			if len(resetRoutes) == 1 {
				panic(sentinel)
			}
		})
	})
	if failure != sentinel || cleanup != nil || len(resetRoutes) != 2 {
		t.Fatalf("paired constructor did not fail at reset: panic=%v cleanup=%t resets=%v", failure, cleanup != nil, resetRoutes)
	}
	if resetRoutes[0] != (testEnvStoreRoutes{postgres: "owned-pair", maintenance: "owned-pair", redis: 1}) ||
		resetRoutes[1] != originalTestEnvStoreRoutes() || currentTestEnvStoreRoutes() != originalTestEnvStoreRoutes() {
		t.Fatalf("partial pair did not restore routes before cleanup reset: %v", resetRoutes)
	}
}

// Cleanup reset failure cannot replace the original reset error, and cannot
// leave either PostgreSQL route pointing at the incomplete database.
func TestTestEnvStorePgPairInitialResetRetainsCleanupFailure(t *testing.T) {
	newTestEnvStoreLifecycleFixture(t)
	pg := Vault.RequireSimpleResource(DefaultPgVaultResourceName).Parse()
	maintenance := Vault.RequireSimpleResource(MaintenancePgVaultResourceName).Parse()
	primary := errors.New("owned initial reset failure")
	secondary := errors.New("owned rollback reset failure")
	resets := 0
	failure := recoverTestEnvStoreFailure(func() {
		pushTestPgResourcesWithReset(pg, maintenance, "owned-pair", func() {
			resets++
			if resets == 1 {
				panic(primary)
			}
			panic(secondary)
		})
	})
	err, ok := failure.(error)
	if !ok || !errors.Is(err, primary) || !errors.Is(err, secondary) || resets != 2 || currentTestEnvStoreRoutes() != originalTestEnvStoreRoutes() {
		t.Fatalf("partial pair lost cleanup authority or failure: panic=%v resets=%d routes=%+v", failure, resets, currentTestEnvStoreRoutes())
	}
}

// The standalone paired helper also owns a one-shot pop, since resolver IDs
// can be reused by a subsequently created resource pair.
func TestTestEnvStorePgPairCleanupIsOneShot(t *testing.T) {
	newTestEnvStoreLifecycleFixture(t)
	pg := Vault.RequireSimpleResource(DefaultPgVaultResourceName).Parse()
	maintenance := Vault.RequireSimpleResource(MaintenancePgVaultResourceName).Parse()
	resets := 0
	reset := func() { resets++ }
	first := pushTestPgResourcesWithReset(pg, maintenance, "owned-first", reset)
	first()
	second := pushTestPgResourcesWithReset(pg, maintenance, "owned-second", reset)
	before := resets
	first()
	if resets != before || currentTestEnvStoreRoutes() != (testEnvStoreRoutes{postgres: "owned-second", maintenance: "owned-second", redis: 1}) {
		t.Fatalf("completed paired pop changed successor: resets=%d routes=%+v", resets, currentTestEnvStoreRoutes())
	}
	second()
	if currentTestEnvStoreRoutes() != originalTestEnvStoreRoutes() || resets != before+1 {
		t.Fatalf("successor paired cleanup did not complete: resets=%d routes=%+v", resets, currentTestEnvStoreRoutes())
	}
}

// TryLock proves the external callback does not inherit the state lock before
// attempting reentry, so a regression fails explicitly instead of deadlocking.
func TestTestEnvStoreCleanupCallbackDoesNotHoldStateLock(t *testing.T) {
	cleanup := &testEnvCleanup{}
	calls, lockHeld := 0, false
	cleanup.callback = func() {
		calls++
		if !cleanup.stateLock.TryLock() {
			lockHeld = true
			return
		}
		cleanup.stateLock.Unlock()
		cleanup.execute()
	}
	cleanup.execute()
	cleanup.execute()
	if lockHeld || calls != 1 {
		t.Fatalf("cleanup callback ran with state lock or repeated: lockHeld=%t calls=%d", lockHeld, calls)
	}
}

// A barrier fixes the in-progress state. The initiating caller owns completion;
// a concurrent or reentrant repeat may not launch a second cleanup callback.
func TestTestEnvStoreCleanupConcurrentCallsHaveOneOwner(t *testing.T) {
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan any, 1)
	var calls atomic.Int32
	cleanup := &testEnvCleanup{callback: func() {
		calls.Add(1)
		close(entered)
		<-release
	}}
	go func() { done <- recoverTestEnvStoreFailure(cleanup.execute) }()
	<-entered
	if !cleanup.stateLock.TryLock() {
		close(release)
		<-done
		t.Fatal("cleanup state lock held across blocked external callback")
	}
	cleanup.stateLock.Unlock()
	cleanup.execute()
	close(release)
	if failure := <-done; failure != nil || calls.Load() != 1 {
		t.Fatalf("cleanup had more than one completion owner: panic=%v calls=%d", failure, calls.Load())
	}
	cleanup.execute()
	if calls.Load() != 1 {
		t.Fatalf("completed cleanup reran its callback: %d", calls.Load())
	}
}

// All registered callbacks are deferred before execution, so even Goexit in
// one callback drains the remainder before the explicit owner join completes.
func TestTestEnvStoreCleanupDrainsCallbacksAfterGoexit(t *testing.T) {
	done := make(chan struct{})
	var effects []string
	go func() {
		defer close(done)
		runTestEnvCleanup(nil,
			func() { effects = append(effects, "first"); runtime.Goexit() },
			func() { effects = append(effects, "second") },
			func() { effects = append(effects, "third") },
		)
	}()
	<-done
	if !reflect.DeepEqual(effects, []string{"first", "second", "third"}) {
		t.Fatalf("cleanup Goexit skipped a later callback: %v", effects)
	}
}

// A failed completion remains failed without running its callbacks again,
// including non-error panic values whose identity must stay intact.
func TestTestEnvStoreCleanupReplaysFailureWithoutCallbacks(t *testing.T) {
	for _, sentinel := range []any{errors.New("owned cleanup error"), &struct{ message string }{message: "owned cleanup panic"}} {
		calls := 0
		cleanup := oneShotTestEnvCleanup(func() { calls++; panic(sentinel) })
		first := recoverTestEnvStoreFailure(cleanup)
		second := recoverTestEnvStoreFailure(cleanup)
		if first != sentinel || second != sentinel || calls != 1 {
			t.Fatalf("completed cleanup failure changed or reran: first=%v second=%v calls=%d", first, second, calls)
		}
	}
}

// An abnormal cleanup return cannot be mistaken for successful completion on
// a later call. The owner is joined before checking its latched outcome.
func TestTestEnvStoreCleanupLatchesAbnormalExit(t *testing.T) {
	done := make(chan any, 1)
	calls := 0
	cleanup := oneShotTestEnvCleanup(func() { calls++; runtime.Goexit() })
	go func() {
		var failure any
		defer func() { done <- failure }()
		defer func() { failure = recover() }()
		cleanup()
	}()
	first := <-done
	second := recoverTestEnvStoreFailure(cleanup)
	err, ok := first.(error)
	if !ok || err.Error() != "test environment cleanup exited before completing" || second != first || calls != 1 {
		t.Fatalf("abnormal cleanup completion was lost: first=%v second=%v calls=%d", first, second, calls)
	}
}

// Setup Goexit after partial warmup uses the same real restoration path.
// No network closure executes, and the owner joins before resolver teardown.
func TestTestEnvStoreRollbackDrainsAfterSetupGoexit(t *testing.T) {
	fixture := newTestEnvStoreRollbackFixture(t)
	fixture.fault = func(operation string, _ int) any {
		if operation == "warmup" {
			runtime.Goexit()
		}
		return nil
	}
	done := make(chan struct{})
	var failure any
	returned := false
	go func() {
		defer close(done)
		defer func() { failure = recover() }()
		fixture.setup(2)
		returned = true
	}()
	<-done
	if returned || failure != nil || fixture.calls["reset"] != 1 {
		t.Fatalf("setup Goexit changed outcome or skipped warmup cleanup: returned=%t panic=%v calls=%v", returned, failure, fixture.calls)
	}
	requireTestEnvStoreRoutesRestored(t, fixture, 1)
}
