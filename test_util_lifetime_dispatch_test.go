package server

// Exercises callback-return and testing.Cleanup ownership using the real
// lifecycle driver and Redis wrapper. Only service effects and transport are
// intercepted; client construction identities supply every target observation.

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
)

// A retained Background worker has an explicit ready/release/join protocol.
// Result fields are read only after done closes; no test assertion runs in
// the worker. All owners join it before restoring fixture globals.
type testEnvLifetimeWorker struct {
	release     chan struct{}
	done        chan struct{}
	releaseOnce sync.Once
	failure     any
	returned    bool
}

// Starts one real wrapper call, but only after the owner releases its barrier.
// No-retry changes recovery only, not routing; unexpected transport must fail
// immediately rather than retrying a Background context until a test timeout.
func newTestEnvLifetimeWorker() *testEnvLifetimeWorker {
	worker := &testEnvLifetimeWorker{release: make(chan struct{}), done: make(chan struct{})}
	ready := make(chan struct{})
	go func() {
		defer close(worker.done)
		defer func() { worker.failure = recover() }()
		close(ready)
		<-worker.release
		ctx := context.Background()
		Redis(ctx, func(client RedisClient) {
			Raise(client.FlushDB(ctx).Err())
		}, OptNoRetry())
		worker.returned = true
	}()
	<-ready
	return worker
}

// Safe for repeated owner cleanup, including after a prior assertion failed.
func (self *testEnvLifetimeWorker) resumeAndJoin() {
	self.releaseOnce.Do(func() { close(self.release) })
	<-self.done
}

// Positive final census checks the actual three PING/FLUSHDB pairs, including
// the cleanup or worker effect, without inferring a target from mutable Vault.
func requireTestEnvLifetimeOwnedCommands(t *testing.T, fixture *testEnvRedisDispatchFixture) {
	t.Helper()
	commands := fixture.snapshot()
	if len(commands) != 6 {
		t.Fatalf("lifetime fixture did not observe its complete command census: %+v", commands)
	}
	for index := 0; index < len(commands); index += 2 {
		requireTestEnvRedisDispatchPair(t, commands[index:index+2], 2)
	}
	if fixture.releases[2] != 1 {
		t.Fatalf("lifetime fixture did not join one real release: %v", fixture.releases)
	}
}

// retryTB currently forwards Cleanup to the parent testing.T, after Run has
// already restored the original route. The actual controller uses precisely
// this shape for a Background-context Redis cleanup after callback success.
func TestTestEnvLifetimeCleanupKeepsOwnedRedisTarget(t *testing.T) {
	newTestEnvRedisDNSGuard(t)
	fixture := newTestEnvRedisDispatchFixture(t)
	t.Setenv("WARP_TEST_ENV_FAIL_FAST", "1")
	var cleanupCalls atomic.Int32
	t.Cleanup(func() {
		if cleanupCalls.Load() != 1 {
			t.Fatalf("owned testing cleanup did not run exactly once: %d", cleanupCalls.Load())
		}
		for _, command := range fixture.snapshot() {
			if command.name == "flushdb" && command.database != 2 {
				t.Fatalf("test cleanup dispatched through restored parent client: owner_db=2 dispatched_db=%d", command.database)
			}
		}
		requireTestEnvLifetimeOwnedCommands(t, fixture)
	})
	fixture.env.runWithSetup(t, func(tb testing.TB) {
		tb.Cleanup(func() {
			ctx := context.Background()
			Redis(ctx, func(client RedisClient) {
				Raise(client.FlushDB(ctx).Err())
			}, OptNoRetry())
			cleanupCalls.Add(1)
		})
	}, func() error { return nil }, func() func() { return fixture.setup(2) })
}

// A successful callback can leave an owned worker parked before its first
// Background-context lookup. Release it only after the actual Run returns;
// this distinguishes callback success from completion of all admitted work.
func TestTestEnvLifetimeSuccessfulCallbackKeepsWorkerTarget(t *testing.T) {
	newTestEnvRedisDNSGuard(t)
	fixture := newTestEnvRedisDispatchFixture(t)
	t.Setenv("WARP_TEST_ENV_FAIL_FAST", "1")
	var worker *testEnvLifetimeWorker
	t.Cleanup(func() {
		if worker != nil {
			worker.resumeAndJoin()
		}
	})
	fixture.env.runWithSetup(t, func(testing.TB) {
		worker = newTestEnvLifetimeWorker()
	}, func() error { return nil }, func() func() { return fixture.setup(2) })
	if worker == nil {
		t.Fatal("successful callback never created the owned worker")
	}
	before := len(fixture.snapshot())
	worker.resumeAndJoin()
	if worker.failure != nil || !worker.returned {
		t.Fatalf("owned Background worker failed before target observation: failure=%v returned=%t", worker.failure, worker.returned)
	}
	commands := fixture.snapshot()
	if len(commands) != before+2 {
		t.Fatalf("owned Background worker command census differs: before=%d commands=%+v", before, commands)
	}
	for _, command := range commands[before:] {
		if command.name == "flushdb" && command.database != 2 {
			t.Fatalf("successful attempt worker dispatched through restored parent client: owner_db=2 dispatched_db=%d", command.database)
		}
	}
	requireTestEnvRedisDispatchPair(t, commands[before:], 2)
	t.Cleanup(func() { requireTestEnvLifetimeOwnedCommands(t, fixture) })
}

// Joining the same Background worker before callback return is a positive
// control: the wrapper, setup and teardown all use their real owned client.
func TestTestEnvLifetimeJoinedCallbackWorkerKeepsOwnedTarget(t *testing.T) {
	newTestEnvRedisDNSGuard(t)
	fixture := newTestEnvRedisDispatchFixture(t)
	t.Setenv("WARP_TEST_ENV_FAIL_FAST", "1")
	var worker *testEnvLifetimeWorker
	t.Cleanup(func() {
		if worker != nil {
			worker.resumeAndJoin()
		}
		requireTestEnvLifetimeOwnedCommands(t, fixture)
	})
	fixture.env.runWithSetup(t, func(tb testing.TB) {
		worker = newTestEnvLifetimeWorker()
		worker.resumeAndJoin()
		if worker.failure != nil || !worker.returned {
			tb.Fatalf("joined Background worker failed before target observation: failure=%v returned=%t", worker.failure, worker.returned)
		}
	}, func() error { return nil }, func() func() { return fixture.setup(2) })
}
