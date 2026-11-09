package sidecarhost

// ServiceHost.Redis on server's Redis. The suites run on the private test store through
// .stack/server-test.sh: one Redis node, whose leased database each test gets alone. The cross-slot
// cases still hold there, since the adapter refuses a call whose keys share no tag before Redis is
// asked. The 3-master cluster run waits for server/test-env.sh to attest more than one Redis endpoint
// (plan-TEST-t3-redis-attestation, QD-028), and is not here.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/sidecartest"
	"github.com/urnetwork/sidecar/sidecartest/redissuite"

	"github.com/urnetwork/server"
)

// A Redis error as the sentinel sidecartest's fake answers with, so the suite reads a real node's
// refusal and the fake's alike, as sidecartest/localstack does with its own client.
func suiteRedisError(err error) error {
	if err == nil {
		return nil
	}
	text := err.Error()
	switch {
	case strings.HasPrefix(text, "WRONGTYPE"):
		return fmt.Errorf("%w: %v", sidecartest.ErrRedisWrongType, err)
	case strings.HasPrefix(text, "NOGROUP"):
		return fmt.Errorf("%w: %v", sidecartest.ErrRedisNoGroup, err)
	case strings.HasPrefix(text, "CROSSSLOT"):
		return fmt.Errorf("%w: %v", sidecartest.ErrRedisCrossSlot, err)
	}
	return err
}

// The adapter with its errors read as the suite's sentinels, and nothing else changed.
type suiteRedis struct {
	redis sidecar.Redis
}

func (self *suiteRedis) Get(ctx context.Context, key string) ([]byte, bool, error) {
	value, found, err := self.redis.Get(ctx, key)
	return value, found, suiteRedisError(err)
}

func (self *suiteRedis) GetMany(ctx context.Context, keys []string) ([][]byte, error) {
	values, err := self.redis.GetMany(ctx, keys)
	return values, suiteRedisError(err)
}

func (self *suiteRedis) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	return suiteRedisError(self.redis.Set(ctx, key, value, ttl))
}

func (self *suiteRedis) SetIfAbsent(ctx context.Context, key string, value []byte, ttl time.Duration) (bool, error) {
	set, err := self.redis.SetIfAbsent(ctx, key, value, ttl)
	return set, suiteRedisError(err)
}

func (self *suiteRedis) CompareAndSet(ctx context.Context, key string, expected []byte, value []byte, ttl time.Duration) (bool, error) {
	set, err := self.redis.CompareAndSet(ctx, key, expected, value, ttl)
	return set, suiteRedisError(err)
}

func (self *suiteRedis) CompareAndDelete(ctx context.Context, key string, expected []byte) (bool, error) {
	deleted, err := self.redis.CompareAndDelete(ctx, key, expected)
	return deleted, suiteRedisError(err)
}

func (self *suiteRedis) Increment(ctx context.Context, key string, ttl time.Duration) (int64, error) {
	value, err := self.redis.Increment(ctx, key, ttl)
	return value, suiteRedisError(err)
}

func (self *suiteRedis) Delete(ctx context.Context, keys []string) error {
	return suiteRedisError(self.redis.Delete(ctx, keys))
}

func (self *suiteRedis) Expire(ctx context.Context, key string, ttl time.Duration) (bool, error) {
	found, err := self.redis.Expire(ctx, key, ttl)
	return found, suiteRedisError(err)
}

func (self *suiteRedis) HashGet(ctx context.Context, key string, field string) ([]byte, bool, error) {
	value, found, err := self.redis.HashGet(ctx, key, field)
	return value, found, suiteRedisError(err)
}

func (self *suiteRedis) HashGetAll(ctx context.Context, key string) (map[string][]byte, error) {
	fieldValues, err := self.redis.HashGetAll(ctx, key)
	return fieldValues, suiteRedisError(err)
}

func (self *suiteRedis) HashSet(ctx context.Context, key string, fieldValues map[string][]byte) error {
	return suiteRedisError(self.redis.HashSet(ctx, key, fieldValues))
}

func (self *suiteRedis) HashDelete(ctx context.Context, key string, fields []string) error {
	return suiteRedisError(self.redis.HashDelete(ctx, key, fields))
}

func (self *suiteRedis) HashExpire(ctx context.Context, key string, fields []string, ttl time.Duration) error {
	return suiteRedisError(self.redis.HashExpire(ctx, key, fields, ttl))
}

func (self *suiteRedis) StreamAdd(ctx context.Context, key string, maxLength int64, fieldValues map[string][]byte) (string, error) {
	entryId, err := self.redis.StreamAdd(ctx, key, maxLength, fieldValues)
	return entryId, suiteRedisError(err)
}

func (self *suiteRedis) StreamRange(ctx context.Context, key string, afterEntryId string, count int64) ([]*sidecar.RedisStreamEntry, error) {
	entries, err := self.redis.StreamRange(ctx, key, afterEntryId, count)
	return entries, suiteRedisError(err)
}

func (self *suiteRedis) StreamGroupCreate(ctx context.Context, key string, group string) error {
	return suiteRedisError(self.redis.StreamGroupCreate(ctx, key, group))
}

func (self *suiteRedis) StreamGroupRead(ctx context.Context, key string, group string, consumer string, count int64, block time.Duration) ([]*sidecar.RedisStreamEntry, error) {
	entries, err := self.redis.StreamGroupRead(ctx, key, group, consumer, count, block)
	return entries, suiteRedisError(err)
}

func (self *suiteRedis) StreamGroupAck(ctx context.Context, key string, group string, entryIds []string) error {
	return suiteRedisError(self.redis.StreamGroupAck(ctx, key, group, entryIds))
}

func (self *suiteRedis) StreamGroupClaim(ctx context.Context, key string, group string, consumer string, minIdle time.Duration, cursor string, count int64) ([]*sidecar.RedisStreamEntry, string, error) {
	entries, nextCursor, err := self.redis.StreamGroupClaim(ctx, key, group, consumer, minIdle, cursor, count)
	return entries, nextCursor, suiteRedisError(err)
}

func (self *suiteRedis) RunScript(ctx context.Context, script *sidecar.RedisScript, keys []string, args []string) (*sidecar.RedisReply, error) {
	reply, err := self.redis.RunScript(ctx, script, keys, args)
	return reply, suiteRedisError(err)
}

func (self *suiteRedis) AddKeyEventCallback(keyPattern string, callback sidecar.RedisKeyEventFunction) (func(), error) {
	return self.redis.AddKeyEventCallback(keyPattern, callback)
}

// A test env whose Redis is the private store's, with a database of the test's own.
func redisTestEnv() *server.TestEnv {
	return &server.TestEnv{ApplyDbMigrations: false}
}

// sidecartest's Redis store suite on the adapter, so T2's fake and the real store answer alike
// (D-TEST-2). A real store keeps real time, so the suite's ttl cases wait.
func TestRedisConformsOnTheTestStore(t *testing.T) {
	redisTestEnv().Run(t, func(tb testing.TB) {
		redissuite.Run(t, &redissuite.Target{
			Redis:        &suiteRedis{redis: NewRedis(nil)},
			Pass:         time.Sleep,
			EventTimeout: 10 * time.Second,
		})
	})
}

// The op-counting wrapper passes the suite too, so a path measured through it behaves as it would
// unwrapped.
func TestCountingRedisConformsOnTheTestStore(t *testing.T) {
	redisTestEnv().Run(t, func(tb testing.TB) {
		counting := NewCountingRedis(NewRedis(nil))
		redissuite.Run(t, &redissuite.Target{
			Redis:        &suiteRedis{redis: counting},
			Pass:         time.Sleep,
			EventTimeout: 10 * time.Second,
		})
		if counting.OpCount() == 0 {
			t.Error("the suite made no counted call")
		}
	})
}

// A boundary that fails the test if a call reaches Redis.
func refusingRedisBoundary(t *testing.T) *redisBoundary {
	reach := func(ctx context.Context, callback func(client server.RedisClient) error) error {
		t.Error("a call reached Redis")
		return errors.New("reached")
	}
	return &redisBoundary{retrying: reach, once: reach}
}

// §22.4's lint at run time: keys that share no tag, even two on one slot by luck, are refused before any
// node is asked, so one node refuses what 3 masters would.
func TestRedisRefusesCrossTagCallsBeforeRedis(t *testing.T) {
	adapter := newRedis(nil, refusingRedisBoundary(t), nil)
	ctx := t.Context()
	cases := [][]string{
		{"{a}1", "{b}2"},
		{"{a}1", "a"},
		// these share a slot only by luck: no tag at all
		{"x", "y"},
	}
	script := &sidecar.RedisScript{Name: "sidecarhost-test-cross", Source: "return 1"}
	for _, keys := range cases {
		if _, err := adapter.GetMany(ctx, keys); !errors.Is(err, errRedisCrossSlot) {
			t.Errorf("GetMany %q: %v", keys, err)
		}
		if err := adapter.Delete(ctx, keys); !errors.Is(err, errRedisCrossSlot) {
			t.Errorf("Delete %q: %v", keys, err)
		}
		if _, err := adapter.RunScript(ctx, script, keys, nil); !errors.Is(err, errRedisCrossSlot) {
			t.Errorf("RunScript %q: %v", keys, err)
		}
		if !strings.HasPrefix(errRedisCrossSlot.Error(), "CROSSSLOT") {
			t.Errorf("the refusal reads %q, not as the cluster's", errRedisCrossSlot)
		}
	}
}

// What host_redis.go calls the caller's bug never reaches Redis as another operation.
func TestRedisRefusesWhatRedisWouldReadAsAnotherOperation(t *testing.T) {
	adapter := newRedis(nil, refusingRedisBoundary(t), nil)
	ctx := t.Context()
	if _, err := adapter.Expire(ctx, "k", 0); err == nil {
		t.Error("Expire took zero, which deletes")
	}
	if err := adapter.HashExpire(ctx, "k", []string{"f"}, 0); err == nil {
		t.Error("HashExpire took zero, which deletes")
	}
	if err := adapter.Set(ctx, "k", []byte("v"), -time.Second); err == nil {
		t.Error("Set took a negative ttl")
	}
	if _, err := adapter.StreamAdd(ctx, "k", -1, map[string][]byte{"f": nil}); err == nil {
		t.Error("StreamAdd took a negative length")
	}
	if _, err := adapter.StreamGroupRead(ctx, "k", "g", "c", 0, 0); err == nil {
		t.Error("StreamGroupRead took a count of zero")
	}
	if _, err := adapter.StreamGroupRead(ctx, "k", "g", "c", 1, -time.Second); err == nil {
		t.Error("StreamGroupRead took a negative block")
	}
	if _, _, err := adapter.StreamGroupClaim(ctx, "k", "g", "c", 0, "", 0); err == nil {
		t.Error("StreamGroupClaim took a count of zero")
	}
	if redisRoundUp(time.Microsecond) != time.Millisecond || redisRoundUp(1500*time.Microsecond) != 2*time.Millisecond {
		t.Error("a positive ttl does not round up to the millisecond")
	}
}

// Which boundary each call took.
type recordingRedisBoundary struct {
	stateLock sync.Mutex
	calls     []string
}

func (self *recordingRedisBoundary) boundary() *redisBoundary {
	record := func(name string) func(context.Context, func(server.RedisClient) error) error {
		return func(ctx context.Context, callback func(client server.RedisClient) error) error {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			self.calls = append(self.calls, name)
			return errors.New("recorded")
		}
	}
	return &redisBoundary{retrying: record("retrying"), once: record("once")}
}

func (self *recordingRedisBoundary) take() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	calls := self.calls
	self.calls = nil
	return calls
}

// A write that is not safe to apply twice, and a call whose second try answers differently, never takes
// the boundary that reruns after a connection error.
func TestRedisNonIdempotentCallsAreNeverReplayed(t *testing.T) {
	recording := &recordingRedisBoundary{}
	adapter := newRedis(nil, recording.boundary(), nil)
	ctx := t.Context()
	script := &sidecar.RedisScript{Name: "sidecarhost-test-once", Source: "return 1"}
	once := map[string]func(){
		"Increment":        func() { adapter.Increment(ctx, "k", 0) },
		"Increment ttl":    func() { adapter.Increment(ctx, "k", time.Minute) },
		"StreamAdd":        func() { adapter.StreamAdd(ctx, "k", 0, map[string][]byte{"f": nil}) },
		"RunScript":        func() { adapter.RunScript(ctx, script, []string{"k"}, nil) },
		"CompareAndSet":    func() { adapter.CompareAndSet(ctx, "k", nil, nil, 0) },
		"CompareAndDelete": func() { adapter.CompareAndDelete(ctx, "k", nil) },
		"SetIfAbsent":      func() { adapter.SetIfAbsent(ctx, "k", nil, 0) },
		"StreamGroupRead":  func() { adapter.StreamGroupRead(ctx, "k", "g", "c", 1, 0) },
		"StreamGroupClaim": func() { adapter.StreamGroupClaim(ctx, "k", "g", "c", 0, "", 1) },
	}
	retrying := map[string]func(){
		"Get":               func() { adapter.Get(ctx, "k") },
		"GetMany":           func() { adapter.GetMany(ctx, []string{"{k}a", "{k}b"}) },
		"Set":               func() { adapter.Set(ctx, "k", nil, 0) },
		"Delete":            func() { adapter.Delete(ctx, []string{"k"}) },
		"Expire":            func() { adapter.Expire(ctx, "k", time.Minute) },
		"HashGet":           func() { adapter.HashGet(ctx, "k", "f") },
		"HashGetAll":        func() { adapter.HashGetAll(ctx, "k") },
		"HashSet":           func() { adapter.HashSet(ctx, "k", map[string][]byte{"f": nil}) },
		"HashDelete":        func() { adapter.HashDelete(ctx, "k", []string{"f"}) },
		"HashExpire":        func() { adapter.HashExpire(ctx, "k", []string{"f"}, time.Minute) },
		"StreamRange":       func() { adapter.StreamRange(ctx, "k", "", 1) },
		"StreamGroupCreate": func() { adapter.StreamGroupCreate(ctx, "k", "g") },
		"StreamGroupAck":    func() { adapter.StreamGroupAck(ctx, "k", "g", []string{"1-1"}) },
	}
	for name, call := range once {
		call()
		if calls := recording.take(); !slices.Equal(calls, []string{"once"}) {
			t.Errorf("%s took %v", name, calls)
		}
	}
	for name, call := range retrying {
		call()
		if calls := recording.take(); !slices.Equal(calls, []string{"retrying"}) {
			t.Errorf("%s took %v", name, calls)
		}
	}
}

// Drops the answer of the first INCR of key after Redis applied it, as a reset connection would, with
// an error server's wrapper reads as a connection error.
type dropFirstIncrHook struct {
	key string

	stateLock sync.Mutex
	dropped   bool
}

func (self *dropFirstIncrHook) DialHook(next redis.DialHook) redis.DialHook {
	return next
}

func (self *dropFirstIncrHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		err := next(ctx, cmd)
		args := cmd.Args()
		if err != nil || len(args) != 2 || !strings.EqualFold(fmt.Sprint(args[0]), "incr") || fmt.Sprint(args[1]) != self.key {
			return err
		}
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.dropped {
			return err
		}
		self.dropped = true
		lost := errors.New("read tcp 192.0.2.1:1: i/o timeout")
		cmd.SetErr(lost)
		return lost
	}
}

func (self *dropFirstIncrHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

// What the counter holds.
func redisTestCount(t testing.TB, ctx context.Context, adapter *Redis, key string) int64 {
	value, found, err := adapter.Get(ctx, key)
	if err != nil || !found {
		t.Fatalf("the counter: %v, %v", found, err)
	}
	count, err := strconv.ParseInt(string(value), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return count
}

// An Increment whose answer is lost after Redis applied it errs and is never applied twice, while the
// same loss on the boundary that reruns after a connection error applies it twice: the hazard the once
// boundary is there for.
func TestRedisDroppedAnswerNeverAppliesAnIncrementTwice(t *testing.T) {
	redisTestEnv().Run(t, func(tb testing.TB) {
		ctx := t.Context()
		adapter := NewRedis(nil)

		onceKey := fmt.Sprintf("{sidecarhost_test_%s}once", server.NewId())
		server.RedisDoOnce(ctx, func(client server.RedisClient) {
			client.AddHook(&dropFirstIncrHook{key: onceKey})
		})
		if _, err := adapter.Increment(ctx, onceKey, 0); err == nil {
			tb.Fatal("a lost answer read as an applied increment")
		}
		if count := redisTestCount(tb, ctx, adapter, onceKey); count != 1 {
			tb.Fatalf("the increment applied %d times", count)
		}

		// the same loss through the rerunning boundary: what the once boundary prevents
		retryKey := fmt.Sprintf("{sidecarhost_test_%s}retry", server.NewId())
		server.Redis(ctx, func(client server.RedisClient) {
			client.AddHook(&dropFirstIncrHook{key: retryKey})
		})
		replaying := newRedis(nil, &redisBoundary{retrying: redisRetrying, once: redisRetrying}, processRedisKeyEvents())
		if _, err := replaying.Increment(ctx, retryKey, 0); err != nil {
			tb.Fatal(err)
		}
		if count := redisTestCount(tb, ctx, adapter, retryKey); count != 2 {
			tb.Fatalf("the rerunning boundary applied the increment %d times; this case proves nothing", count)
		}
	})
}

// A script a node has not loaded, as after a flush or a failover, runs by its source and is loaded for
// the next call by SHA-1.
func TestRedisScriptLoadsWhereTheNodeLacksIt(t *testing.T) {
	redisTestEnv().Run(t, func(tb testing.TB) {
		ctx := t.Context()
		adapter := NewRedis(nil)
		// a source no node has seen, so its SHA-1 is unknown everywhere
		script := &sidecar.RedisScript{
			Name:   "sidecarhost-test-fresh",
			Source: fmt.Sprintf("-- %s\nreturn ARGV[1]", server.NewId()),
		}
		sha := redis.NewScript(script.Source).Hash()
		key := fmt.Sprintf("{sidecarhost_test_%s}k", server.NewId())
		exists := func() bool {
			var loaded []bool
			server.Redis(ctx, func(client server.RedisClient) {
				var err error
				loaded, err = client.ScriptExists(ctx, sha).Result()
				server.Raise(err)
			})
			return 0 < len(loaded) && loaded[0]
		}
		if exists() {
			tb.Fatal("the fresh script is already loaded; this case proves nothing")
		}
		for range 2 {
			reply, err := adapter.RunScript(ctx, script, []string{key}, []string{"answer"})
			if err != nil {
				tb.Fatal(err)
			}
			if reply.Kind != sidecar.RedisReplyBulk || string(reply.Bulk) != "answer" {
				tb.Fatalf("reply %+v", reply)
			}
			if !exists() {
				tb.Fatal("the script did not load")
			}
		}
	})
}

// Numbers past 2^53 stay exact: an increment's value, a stored value and a script's bulk reply.
func TestRedisValuesPast2To53Survive(t *testing.T) {
	redisTestEnv().Run(t, func(tb testing.TB) {
		ctx := t.Context()
		adapter := NewRedis(nil)
		key := fmt.Sprintf("{sidecarhost_test_%s}n", server.NewId())
		// 2^53 + 1, which a double cannot hold
		if err := adapter.Set(ctx, key, []byte("9007199254740993"), time.Minute); err != nil {
			tb.Fatal(err)
		}
		for _, ttl := range []time.Duration{0, time.Minute} {
			if _, err := adapter.Increment(ctx, key, ttl); err != nil {
				tb.Fatal(err)
			}
		}
		if value, err := adapter.Increment(ctx, key, time.Minute); err != nil || value != 9007199254740996 {
			tb.Fatalf("the increment answered %d, %v", value, err)
		}
		script := &sidecar.RedisScript{Name: "sidecarhost-test-echo", Source: "return redis.call('GET', KEYS[1])"}
		reply, err := adapter.RunScript(ctx, script, []string{key}, nil)
		if err != nil || reply.Kind != sidecar.RedisReplyBulk || string(reply.Bulk) != "9007199254740996" {
			tb.Fatalf("the script answered %+v, %v", reply, err)
		}
	})
}

// One fake subscription: what it was asked for, and the channels the test drives it with.
type fakeKeyEventSubscription struct {
	patterns []string
	messages chan *redisKeyEventMessage
	done     chan struct{}
	ended    chan struct{}
}

// server.SubscribeKeyEvents with the test holding each subscription: a subscribe returns, acknowledged,
// once the test lets it through.
type fakeKeyEventSubscriber struct {
	subscriptions chan *fakeKeyEventSubscription
	acknowledge   chan struct{}
}

func newFakeKeyEventSubscriber() *fakeKeyEventSubscriber {
	return &fakeKeyEventSubscriber{
		subscriptions: make(chan *fakeKeyEventSubscription, 16),
		acknowledge:   make(chan struct{}),
	}
}

func (self *fakeKeyEventSubscriber) subscribe(ctx context.Context, topologyRefresh time.Duration, patterns ...string) (<-chan *redisKeyEventMessage, <-chan struct{}, func(), error) {
	subscription := &fakeKeyEventSubscription{
		patterns: patterns,
		messages: make(chan *redisKeyEventMessage),
		done:     make(chan struct{}),
		ended:    make(chan struct{}),
	}
	self.subscriptions <- subscription
	select {
	case <-self.acknowledge:
	case <-ctx.Done():
		return nil, nil, nil, ctx.Err()
	}
	var once sync.Once
	return subscription.messages, subscription.done, func() {
		once.Do(func() { close(subscription.ended) })
	}, nil
}

// How long a step of a subscriber test may take before it fails rather than hang.
const keyEventStepTimeout = 10 * time.Second

// The next subscription asked for, acknowledged.
func (self *fakeKeyEventSubscriber) next(t *testing.T) *fakeKeyEventSubscription {
	t.Helper()
	var subscription *fakeKeyEventSubscription
	select {
	case subscription = <-self.subscriptions:
	case <-time.After(keyEventStepTimeout):
		t.Fatal("no subscription")
	}
	select {
	case self.acknowledge <- struct{}{}:
	case <-time.After(keyEventStepTimeout):
		t.Fatal("the subscription was not waiting")
	}
	return subscription
}

// Waits for ch to close.
func requireClosed(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(keyEventStepTimeout):
		t.Fatalf("%s did not happen", what)
	}
}

// Events a callback heard, in order.
type keyEventLog struct {
	events chan sidecar.RedisKeyEvent
}

func newKeyEventLog() *keyEventLog {
	return &keyEventLog{events: make(chan sidecar.RedisKeyEvent, 64)}
}

func (self *keyEventLog) callback(event sidecar.RedisKeyEvent) {
	self.events <- event
}

func (self *keyEventLog) next(t *testing.T) sidecar.RedisKeyEvent {
	t.Helper()
	select {
	case event := <-self.events:
		return event
	case <-time.After(keyEventStepTimeout):
		t.Fatal("no event")
		return sidecar.RedisKeyEvent{}
	}
}

// Adds a callback on a goroutine, since AddCallback returns only once a subscription holding its family
// is acknowledged.
func addKeyEventCallback(keyEvents *redisKeyEvents, keyPattern string, log *keyEventLog) <-chan func() {
	added := make(chan func(), 1)
	go func() {
		remove, err := keyEvents.AddCallback(keyPattern, log.callback)
		if err != nil {
			added <- nil
			return
		}
		added <- remove
	}()
	return added
}

// The removal AddCallback gave, once it returned.
func requireAdded(t *testing.T, added <-chan func()) func() {
	t.Helper()
	select {
	case remove := <-added:
		if remove == nil {
			t.Fatal("the callback was refused")
		}
		return remove
	case <-time.After(keyEventStepTimeout):
		t.Fatal("AddCallback did not return")
		return nil
	}
}

// One subscription per process however many families and callbacks. A new family is subscribed beside
// the live subscription, which ends only once its successor is acknowledged, so no callback hears a
// Resync for it; a lost subscription tells every callback Resync at the loss and again when one is back;
// an event reaches only its own family's callbacks; and the last removal ends the subscription.
func TestRedisKeyEventsShareOneSubscription(t *testing.T) {
	subscriber := newFakeKeyEventSubscriber()
	keyEvents := newRedisKeyEvents(nil, subscriber.subscribe, func() int { return 3 })

	scpLog := newKeyEventLog()
	scpAdded := addKeyEventCallback(keyEvents, "{scp_*}n", scpLog)
	first := subscriber.next(t)
	if !slices.Equal(first.patterns, []string{"__keyspace@3__:{scp_*}n"}) {
		t.Fatalf("patterns %q", first.patterns)
	}
	removeScp := requireAdded(t, scpAdded)

	// a second callback on a held family shares the subscription at once
	otherScpLog := newKeyEventLog()
	removeOtherScp := requireAdded(t, addKeyEventCallback(keyEvents, "{scp_*}n", otherScpLog))

	// a new family is subscribed with both, beside the live one, which keeps delivering meanwhile
	tesLog := newKeyEventLog()
	tesAdded := addKeyEventCallback(keyEvents, "{tes_*}n", tesLog)
	var second *fakeKeyEventSubscription
	select {
	case second = <-subscriber.subscriptions:
	case <-time.After(keyEventStepTimeout):
		t.Fatal("no subscription for the new family")
	}
	if !slices.Equal(second.patterns, []string{"__keyspace@3__:{scp_*}n", "__keyspace@3__:{tes_*}n"}) {
		t.Fatalf("patterns %q", second.patterns)
	}
	first.messages <- &redisKeyEventMessage{pattern: "__keyspace@3__:{scp_*}n", channel: "__keyspace@3__:{scp_c0}n"}
	for _, log := range []*keyEventLog{scpLog, otherScpLog} {
		if event := log.next(t); event.Key != "{scp_c0}n" {
			t.Fatalf("%+v before the successor", event)
		}
	}
	subscriber.acknowledge <- struct{}{}
	removeTes := requireAdded(t, tesAdded)
	requireClosed(t, first.ended, "the predecessor's end")

	second.messages <- &redisKeyEventMessage{pattern: "__keyspace@3__:{tes_*}n", channel: "__keyspace@3__:{tes_c1}n"}
	if event := tesLog.next(t); event.Resync || event.Key != "{tes_c1}n" {
		t.Fatalf("%+v, want the key", event)
	}
	second.messages <- &redisKeyEventMessage{pattern: "__keyspace@3__:{scp_*}n", channel: "__keyspace@3__:{scp_c2}n"}
	for _, log := range []*keyEventLog{scpLog, otherScpLog} {
		if event := log.next(t); event.Key != "{scp_c2}n" {
			t.Fatalf("%+v", event)
		}
	}
	// a growth is no gap: nobody heard Resync, and nobody heard another family's event
	for _, log := range []*keyEventLog{scpLog, otherScpLog, tesLog} {
		if 0 < len(log.events) {
			t.Fatalf("an extra event %+v", <-log.events)
		}
	}

	// a lost subscription: Resync at the loss, and again once a successor is acknowledged
	close(second.done)
	for _, log := range []*keyEventLog{scpLog, otherScpLog, tesLog} {
		if event := log.next(t); !event.Resync {
			t.Fatalf("%+v, want Resync at the loss", event)
		}
	}
	third := subscriber.next(t)
	if len(third.patterns) != 2 {
		t.Fatalf("patterns %q", third.patterns)
	}
	for _, log := range []*keyEventLog{scpLog, otherScpLog, tesLog} {
		if event := log.next(t); !event.Resync {
			t.Fatalf("%+v, want Resync once back", event)
		}
	}

	// the last callback's removal ends the subscription
	removeScp()
	removeOtherScp()
	removeTes()
	removeTes()
	requireClosed(t, third.ended, "the subscription's end with its last callback")
}

// A pattern naming one key, or every key, is refused: watching is per family, never per client.
func TestRedisKeyEventPatternNeedsAFamily(t *testing.T) {
	subscriber := newFakeKeyEventSubscriber()
	keyEvents := newRedisKeyEvents(nil, subscriber.subscribe, func() int { return 0 })
	for _, keyPattern := range []string{"{scp_c1}n", "*", "**", ""} {
		if _, err := keyEvents.AddCallback(keyPattern, func(sidecar.RedisKeyEvent) {}); err == nil {
			t.Errorf("%q was taken", keyPattern)
		}
	}
	if 0 < len(subscriber.subscriptions) {
		t.Error("a refused pattern subscribed")
	}
}

// With no subscription acknowledged in time the callback is removed and the call errs, so a caller
// learns Redis is away rather than hear nothing.
func TestRedisKeyEventCallbackFailsWithoutASubscription(t *testing.T) {
	settings := DefaultRedisKeyEventsSettings()
	settings.AddTimeout = 50 * time.Millisecond
	settings.ResubscribeTimeout = time.Hour
	failing := func(ctx context.Context, topologyRefresh time.Duration, patterns ...string) (<-chan *redisKeyEventMessage, <-chan struct{}, func(), error) {
		return nil, nil, nil, errors.New("no master answers")
	}
	keyEvents := newRedisKeyEvents(settings, failing, func() int { return 0 })
	if _, err := keyEvents.AddCallback("{scp_*}n", func(sidecar.RedisKeyEvent) {}); err == nil {
		t.Fatal("a callback with no subscription was taken")
	}
	if 0 < len(keyEvents.keyPatterns()) {
		t.Fatal("the refused callback stayed registered")
	}
}
