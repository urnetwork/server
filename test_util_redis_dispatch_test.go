package server

// Exercises the actual post-acquisition teardown and Redis command wrapper.
// Only pool construction/reset and transport are replaced: every observed
// command passes through a real go-redis client bound to its construction DB.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"reflect"
	"slices"
	"sync"
	"testing"

	"github.com/redis/go-redis/v9"
)

// Values are detached from both mutable Vault routes and client options.
type testEnvRedisDispatchCommand struct {
	database int
	clientID int
	name     string
}

// Client construction and global pool replacement have one fixture owner.
// Command observations are lock protected; paused teardown tests explicitly
// join their worker before inspecting state or restoring any global variable.
type testEnvRedisDispatchFixture struct {
	t                  *testing.T
	ctx                context.Context
	cancel             context.CancelFunc
	env                *TestEnv
	stateLock          sync.Mutex
	commands           []testEnvRedisDispatchCommand
	clients            []*redis.Client
	releases           map[int]int
	dialCalls          int
	unexpectedCommands []string
	pipelineCalls      int
	beforeReset        func(int)
}

// Each hook is immutable and belongs to exactly one synthetic client. A
// process hook acknowledges only PING/FLUSHDB without invoking transport.
type testEnvRedisDispatchHook struct {
	fixture  *testEnvRedisDispatchFixture
	client   *redis.Client
	database int
	clientID int
}

// A refused dial cancels the owned context, so an unexpected wrapper retry
// cannot wait through its ordinary connection-recovery budget.
func (self *testEnvRedisDispatchFixture) refuseDial(context.Context, string, string) (net.Conn, error) {
	self.stateLock.Lock()
	self.dialCalls++
	self.stateLock.Unlock()
	self.cancel()
	return nil, errors.New("hermetic Redis dispatch fixture forbids transport")
}

// Never delegates to the real dialer, even if command interception regresses.
func (self *testEnvRedisDispatchHook) DialHook(redis.DialHook) redis.DialHook {
	return self.fixture.refuseDial
}

// Records the actual client selected by Redis, not the currently visible DB.
func (self *testEnvRedisDispatchHook) ProcessHook(redis.ProcessHook) redis.ProcessHook {
	return func(_ context.Context, command redis.Cmder) error {
		if self.client.Options().DB != self.database {
			return errors.New("hermetic Redis dispatch client construction identity changed")
		}
		name := command.Name()
		status, statusOK := command.(*redis.StatusCmd)
		if !statusOK || name != "ping" && name != "flushdb" {
			self.fixture.stateLock.Lock()
			self.fixture.unexpectedCommands = append(self.fixture.unexpectedCommands, name)
			self.fixture.stateLock.Unlock()
			return fmt.Errorf("hermetic Redis dispatch fixture refuses command %s", name)
		}
		self.fixture.stateLock.Lock()
		self.fixture.commands = append(self.fixture.commands, testEnvRedisDispatchCommand{database: self.database, clientID: self.clientID, name: name})
		self.fixture.stateLock.Unlock()
		if name == "ping" {
			status.SetVal("PONG")
		} else {
			status.SetVal("OK")
		}
		return nil
	}
}

// Pipelines must not bypass single-command transport refusal.
func (self *testEnvRedisDispatchHook) ProcessPipelineHook(redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(_ context.Context, _ []redis.Cmder) error {
		self.fixture.stateLock.Lock()
		self.fixture.pipelineCalls++
		self.fixture.stateLock.Unlock()
		return errors.New("hermetic Redis dispatch fixture refuses pipeline")
	}
}

// Reuses the original neutral fixture's actual, test-owned Vault resources.
// Neither original global pool is opened or reset. All owned clients are
// closed before restoring those globals and the private resolver.
func newTestEnvRedisDispatchFixture(t *testing.T) *testEnvRedisDispatchFixture {
	t.Helper()
	neutral := newTestEnvStoreLifecycleFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	fixture := &testEnvRedisDispatchFixture{t: t, ctx: ctx, cancel: cancel, env: neutral.env, releases: map[int]int{}}
	previous, previousOnce := safeClient, safeNoCommandRetryClient
	t.Cleanup(func() {
		cancel()
		for _, client := range fixture.clients {
			if err := client.Close(); err != nil {
				t.Errorf("close owned Redis dispatch client: %v", err)
			}
		}
		safeClient, safeNoCommandRetryClient = previous, previousOnce
		fixture.stateLock.Lock()
		dialCalls := fixture.dialCalls
		fixture.stateLock.Unlock()
		if dialCalls != 0 {
			t.Errorf("hermetic Redis dispatch attempted transport %d times", dialCalls)
		}
	})
	fixture.installClient()
	return fixture
}

// Real override state supplies each client's immutable construction identity.
// Former clients remain fixture owned until joined cleanup; this does not
// qualify real RedisReset connection lifetime or connection establishment.
func (self *testEnvRedisDispatchFixture) installClient() *redis.Client {
	resource := Vault.RequireSimpleResource("redis.yml")
	database := resource.RequireInt("db")
	client := redis.NewClient(&redis.Options{
		Addr: resource.RequireString("authority"), Password: resource.RequireString("password"), DB: database,
		MaxRetries: -1, MinIdleConns: 0, PoolSize: 1, MaxActiveConns: 1, Dialer: self.refuseDial,
	})
	self.clients = append(self.clients, client)
	client.AddHook(&testEnvRedisDispatchHook{fixture: self, client: client, database: database, clientID: len(self.clients)})
	safeClient = &safeRedisClient{client: client}
	safeNoCommandRetryClient = &safeRedisClient{client: client, disableCommandRetry: true}
	return client
}

// Runs real resource installation and real Redis flush effect closures. All
// PostgreSQL, migration, reset/warmup and notification I/O remains intercepted.
// The supplied release observes callback order only; it is not a real lease.
func (self *testEnvRedisDispatchFixture) setup(database int) func() {
	pg := Vault.RequireSimpleResource(DefaultPgVaultResourceName).Parse()
	maintenance := Vault.RequireSimpleResource(MaintenancePgVaultResourceName).Parse()
	resource := Vault.RequireSimpleResource("redis.yml")
	return self.env.setupWithAcquiredStores(
		self.ctx, pg, maintenance, fmt.Sprintf("test_123_%032x", database),
		resource.RequireString("authority"), resource.RequireString("password"), database,
		func() {
			self.stateLock.Lock()
			self.releases[database]++
			self.stateLock.Unlock()
		},
		func(operation string, effect func()) {
			switch operation {
			case "reset":
				if self.beforeReset != nil {
					self.beforeReset(database)
				}
			case "redis-reset":
				self.installClient()
			case "redis-flush":
				effect()
			}
		},
	)
}

// Snapshot values cannot change when a later command is dispatched.
func (self *testEnvRedisDispatchFixture) snapshot() []testEnvRedisDispatchCommand {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.commands)
}

// Independent prerequisites pin the actual ping/flush pair and client chosen.
func requireTestEnvRedisDispatchPair(t *testing.T, commands []testEnvRedisDispatchCommand, database int) {
	t.Helper()
	if len(commands) != 2 || commands[0].name != "ping" || commands[1].name != "flushdb" || commands[0].database != database || commands[1].database != database || commands[0].clientID != commands[1].clientID {
		t.Fatalf("actual Redis command pair differs from bound client: db=%d commands=%+v", database, commands)
	}
}

// A normal lifecycle dispatches both flushes only to its own actual client.
func TestTestEnvRedisDispatchSingleOwnerUsesBoundDatabase(t *testing.T) {
	fixture := newTestEnvRedisDispatchFixture(t)
	teardown := fixture.setup(2)
	requireTestEnvRedisDispatchPair(t, fixture.snapshot(), 2)
	teardown()
	commands := fixture.snapshot()
	if len(commands) != 4 {
		t.Fatalf("single-owner lifecycle command count: %+v", commands)
	}
	requireTestEnvRedisDispatchPair(t, commands[2:], 2)
	if RedisDb() != 1 || fixture.releases[2] != 1 {
		t.Fatalf("single-owner cleanup did not restore/release: db=%d releases=%v", RedisDb(), fixture.releases)
	}
}

// Keeping A's actual teardown while B installs its real overrides must not
// cause A to dispatch a destructive command through B's constructed client.
func TestTestEnvRedisDispatchRetainedTeardownRejectsSuccessorTarget(t *testing.T) {
	fixture := newTestEnvRedisDispatchFixture(t)
	teardownA := fixture.setup(2)
	requireTestEnvRedisDispatchPair(t, fixture.snapshot(), 2)
	teardownB := fixture.setup(3)
	defer teardownB()
	requireTestEnvRedisDispatchPair(t, fixture.snapshot()[2:], 3)
	before := len(fixture.snapshot())
	teardownA()
	for _, command := range fixture.snapshot()[before:] {
		if command.name == "flushdb" && command.database != 2 {
			t.Fatalf("retained teardown dispatched FLUSHDB through successor client: owner_db=2 dispatched_db=%d client=%d", command.database, command.clientID)
		}
	}
	if fixture.releases[2] != 1 {
		t.Fatal("retained teardown did not complete its real release callback")
	}
}

// A channel barrier pauses the actual teardown at Reset, before its global
// Redis lookup. B installs fully before A resumes; the worker is always joined.
// This reproduces the stale-worker routing boundary, not timeout abandonment.
func TestTestEnvRedisDispatchPausedTeardownRejectsSuccessorTarget(t *testing.T) {
	fixture := newTestEnvRedisDispatchFixture(t)
	teardownA := fixture.setup(2)
	requireTestEnvRedisDispatchPair(t, fixture.snapshot(), 2)
	entered, resume, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	fixture.beforeReset = func(database int) {
		if database == 2 {
			close(entered)
			<-resume
		}
	}
	var failure any
	go func() {
		defer close(done)
		defer func() { failure = recover() }()
		teardownA()
	}()
	resumed := false
	defer func() {
		if !resumed {
			close(resume)
		}
		<-done
	}()
	select {
	case <-entered:
	case <-done:
		t.Fatalf("teardown did not reach the owned reset barrier: %v", failure)
	}
	teardownB := fixture.setup(3)
	defer teardownB()
	requireTestEnvRedisDispatchPair(t, fixture.snapshot()[2:], 3)
	before := len(fixture.snapshot())
	resumed = true
	close(resume)
	<-done
	if failure != nil {
		t.Fatalf("paused teardown failed before the command-target assertion: %v", failure)
	}
	for _, command := range fixture.snapshot()[before:] {
		if command.name == "flushdb" && command.database != 2 {
			t.Fatalf("paused teardown dispatched FLUSHDB through successor client: owner_db=2 dispatched_db=%d client=%d", command.database, command.clientID)
		}
	}
	if fixture.releases[2] != 1 {
		t.Fatal("paused teardown did not complete its real release callback")
	}
}

// A retained real client remains observable as A even while Vault names B.
// This catches a canary that merely rereads RedisDb at observation time.
func TestTestEnvRedisDispatchObservationKeepsConstructionIdentity(t *testing.T) {
	fixture := newTestEnvRedisDispatchFixture(t)
	teardownA := fixture.setup(2)
	defer teardownA()
	clientA, ok := safeClient.current().(*redis.Client)
	if !ok {
		t.Fatal("fixture did not install an actual go-redis client")
	}
	teardownB := fixture.setup(3)
	defer teardownB()
	before := len(fixture.snapshot())
	if RedisDb() != 3 || clientA.Options().DB != 2 {
		t.Fatal("distinct global route and construction identity are absent")
	}
	if err := clientA.Ping(fixture.ctx).Err(); err != nil {
		t.Fatal(err)
	}
	if err := clientA.FlushDB(fixture.ctx).Err(); err != nil {
		t.Fatal(err)
	}
	requireTestEnvRedisDispatchPair(t, fixture.snapshot()[before:], 2)
}

// Commands outside the declared observation boundary cannot reach a socket.
func TestTestEnvRedisDispatchHookRejectsUnexpectedCommands(t *testing.T) {
	fixture := newTestEnvRedisDispatchFixture(t)
	client := safeClient.current()
	for _, key := range []string{"owned-one", "owned-two"} {
		if err := client.Get(fixture.ctx, key).Err(); err == nil || err.Error() != "hermetic Redis dispatch fixture refuses command get" {
			t.Fatalf("unexpected command escaped the hermetic hook: %v", err)
		}
	}
	if len(fixture.snapshot()) != 0 || !reflect.DeepEqual(fixture.unexpectedCommands, []string{"get", "get"}) {
		t.Fatal("refused commands were accepted or not observed")
	}
}

// Even supported individual commands cannot bypass refusal in a pipeline.
func TestTestEnvRedisDispatchHookRejectsPipeline(t *testing.T) {
	fixture := newTestEnvRedisDispatchFixture(t)
	_, err := safeClient.current().Pipelined(fixture.ctx, func(pipe redis.Pipeliner) error {
		pipe.Ping(fixture.ctx)
		pipe.FlushDB(fixture.ctx)
		return nil
	})
	if err == nil || err.Error() != "hermetic Redis dispatch fixture refuses pipeline" || fixture.pipelineCalls != 1 || len(fixture.snapshot()) != 0 {
		t.Fatalf("pipeline escaped the hermetic hook: error=%v calls=%d commands=%v", err, fixture.pipelineCalls, fixture.snapshot())
	}
}
