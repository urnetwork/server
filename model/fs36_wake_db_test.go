//go:build sidecar_pending

package model

// FS-36 (§22.8, WK1 to WK4; §18.7's steps 0 to 5, §19.4's sender; SDK-M8-12) over Postgres: the token rows
// and their writers (SetPushToken), taskworker's pass (RunPushTokenPass), the hint writer with its enqueue
// and the push sender as they ship, a fake clock for the sender and a vendor sink, on every ground the
// environment gives (redis_cluster_harness_test.go): T2's twin cluster, and the environment's Redis through
// the real Lua, which is T3's 3-master cluster once server/test-env.sh attests its masters. A client's keys
// and its wake shard's sit in different slots, and no call names both (WK1): the twin refuses one that does,
// and a cluster answers CROSSSLOT.
//
//	live refreshed at 0 s and the client lost: a mail hint at 10 s gives one submission at 25 s +- 1 s
//	a hint the client's stack acked gives none
//	a forced router miss gives one at once
//	after a flush nothing is woken; wakes resume on a token POST, and with no POST once the sentinel pass
//	has reconciled the client's slot, the first pass at least 90 s after the one that found it
//	after one master restarts empty and one from an older dump, the first pass at least 90 s after the one
//	that found them reconciles their slots: their clients' wakes resume with no app launch, a changed
//	token's at the newest row's, and none reaches a token deleted since the dump
//
// The failed and the killed XADD and the sender's kills are the writers' and the sender's alone, with no
// row or pass in them: sidecar/sidecartest/fault/fs36_test.go runs them on the twin, where a claim's 30 s
// idle follows the fake clock as a real master's does not. The two masters' restart runs in the twin, and on
// a real cluster through the harness; the environment's one node is never restarted, and the row says so
// where it could not run.
//
// A real master's clock stands still under the schedule: a hint's id carries its time, and the sender drops
// an entry over 60 s from it. So on a real Redis the sender's clock moves only where the schedule waits on
// the sender, 25 s at most, and the 90 s the pass waits moves the pass's clock alone.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/pushservice"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
)

// How long a schedule waits on the sender, a group or a timer before calling itself broken. Every wait is
// on an event.
const fs36Timeout = 60 * time.Second

// What the schedule's writers and sender need of Redis: the twin's fake, or the environment's.
type fs36Redis interface {
	pushservice.WakeSenderRedis
	pushservice.WakeQueueRedis
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
}

// The environment's Redis: server's push adapter for the scripts, SET and XADD, and the sender's group
// commands on server's client, which that adapter does not carry (sidecarhost's does, and sidecarhost
// imports this package).
type fs36ServerRedis struct {
	*SidecarPushRedis
	client server.RedisClient
}

func fs36StreamEntries(messages []redis.XMessage) []*sidecar.RedisStreamEntry {
	entries := make([]*sidecar.RedisStreamEntry, 0, len(messages))
	for _, message := range messages {
		fieldValues := make(map[string][]byte, len(message.Values))
		for field, value := range message.Values {
			if text, ok := value.(string); ok {
				fieldValues[field] = []byte(text)
			}
		}
		entries = append(entries, &sidecar.RedisStreamEntry{Id: message.ID, FieldValues: fieldValues})
	}
	return entries
}

// See pushservice.WakeSenderRedis.
func (self *fs36ServerRedis) GetMany(ctx context.Context, keys []string) ([][]byte, error) {
	replies, err := self.client.MGet(ctx, keys...).Result()
	if err != nil {
		return nil, err
	}
	values := make([][]byte, len(replies))
	for i, reply := range replies {
		if text, ok := reply.(string); ok {
			values[i] = []byte(text)
		}
	}
	return values, nil
}

// See pushservice.WakeSenderRedis: the group from the stream's end, made with its stream, and one that
// stands already left as it is.
func (self *fs36ServerRedis) StreamGroupCreate(ctx context.Context, key string, group string) error {
	err := self.client.XGroupCreateMkStream(ctx, key, group, "$").Err()
	if err != nil && strings.HasPrefix(err.Error(), "BUSYGROUP") {
		return nil
	}
	return err
}

// See pushservice.WakeSenderRedis.
func (self *fs36ServerRedis) StreamGroupRead(ctx context.Context, key string, group string, consumer string, count int64, block time.Duration) ([]*sidecar.RedisStreamEntry, error) {
	streams, err := self.client.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group:    group,
		Consumer: consumer,
		Streams:  []string{key, ">"},
		Count:    count,
		Block:    block,
	}).Result()
	if errors.Is(err, redis.Nil) {
		return []*sidecar.RedisStreamEntry{}, nil
	}
	if err != nil {
		return nil, err
	}
	entries := []*sidecar.RedisStreamEntry{}
	for _, stream := range streams {
		entries = append(entries, fs36StreamEntries(stream.Messages)...)
	}
	return entries, nil
}

// See pushservice.WakeSenderRedis.
func (self *fs36ServerRedis) StreamGroupAck(ctx context.Context, key string, group string, entryIds []string) error {
	return self.client.XAck(ctx, key, group, entryIds...).Err()
}

// See pushservice.WakeSenderRedis.
func (self *fs36ServerRedis) StreamGroupClaim(ctx context.Context, key string, group string, consumer string, minIdle time.Duration, cursor string, count int64) ([]*sidecar.RedisStreamEntry, string, error) {
	if cursor == "" {
		cursor = "0-0"
	}
	messages, nextCursor, err := self.client.XAutoClaim(ctx, &redis.XAutoClaimArgs{
		Stream:   key,
		Group:    group,
		Consumer: consumer,
		MinIdle:  minIdle,
		Start:    cursor,
		Count:    count,
	}).Result()
	if err != nil {
		return nil, "", err
	}
	return fs36StreamEntries(messages), nextCursor, nil
}

// A ground's Redis that tells the schedule of each group a sender's reader made: a reader that has made
// its group reads every entry queued from then on, so a schedule queues none before.
type fs36GroupRedis struct {
	fs36Redis
	made chan string
}

// See pushservice.WakeSenderRedis.
func (self *fs36GroupRedis) StreamGroupCreate(ctx context.Context, key string, group string) error {
	err := self.fs36Redis.StreamGroupCreate(ctx, key, group)
	if err == nil {
		self.made <- key
	}
	return err
}

// The sender's token store over the rows, as taskworker's is: a vendor's rejection tombstones only at the
// revision read with the rejected token.
type fs36TokenStore struct{}

// See pushservice.WakeTokenStore.
func (self fs36TokenStore) TombstoneIfRevision(ctx context.Context, clientId connect.Id, revision int64) (bool, error) {
	return TombstonePushTokenIfRevision(ctx, server.Id(clientId), revision, nil)
}

// One schedule's world on one ground.
type fs36World struct {
	t      testing.TB
	ctx    context.Context
	ground *testTokenGround
	// undoes the ground's routing of the token writers
	restoreWriters func()
	// the sender's, the enqueuer's and the vendor's clock: the twin's own, or a fake one beside a real Redis
	clock *sidecartest.FakeClock
	start time.Time
	redis *fs36GroupRedis
	apns  *sidecartest.FakeWakeVendor

	hintSettings   *pushservice.HintWriterSettings
	senderSettings *pushservice.WakeSenderSettings
	liveTimeout    time.Duration
	writer         *pushservice.HintWriter
	settled        chan *pushservice.WakeSettled
	// ends the senders and waits for them
	closeSenders []func()
}

// Runs row on every ground of the environment, each in a world of its own.
func runFs36(t *testing.T, row func(world *fs36World)) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		for _, ground := range testTokenGrounds(t, ctx) {
			func() {
				ground.clearRows()
				world := newFs36World(t, ctx, ground)
				defer world.close()
				row(world)
			}()
		}
	})
}

func newFs36World(t testing.TB, ctx context.Context, ground *testTokenGround) *fs36World {
	t.Helper()
	self := &fs36World{
		t:              t,
		ctx:            ctx,
		ground:         ground,
		restoreWriters: ground.routeWriters(),
		hintSettings:   pushservice.DefaultHintWriterSettings(),
		senderSettings: pushservice.DefaultWakeSenderSettings(),
		liveTimeout:    pushservice.DefaultSidecarPublisherSettings().LiveTimeout,
		settled:        make(chan *pushservice.WakeSettled, 256),
	}
	// claims and the switch's poll wait long, each its own length, so the timers a schedule waits on are
	// the dispatcher's and the readers' backoffs
	self.senderSettings.ClaimInterval = 1000 * time.Hour
	self.senderSettings.GateInterval = 10000 * time.Hour
	var ground36 fs36Redis
	if ground.twin != nil {
		self.clock = ground.twin.clock
		ground36 = ground.twin.redis
		// a read blocks on the twin's clock
		self.senderSettings.ReadBlock = 100 * time.Hour
	} else {
		self.clock = sidecartest.NewFakeClock(time.Now())
		ground36 = &fs36ServerRedis{
			SidecarPushRedis: NewSidecarPushRedis(nil),
			client:           redisClientOf(ctx),
		}
		// a read blocks on the master's clock, and a sender's end waits for its reads
		self.senderSettings.ReadBlock = 1 * time.Second
	}
	self.start = self.clock.Now()
	self.redis = &fs36GroupRedis{fs36Redis: ground36, made: make(chan string, 4096)}
	self.apns = sidecartest.NewFakeWakeVendor(pushservice.WakePlatformApns, self.clock)
	enqueuer, err := pushservice.NewWakeEnqueuer(self.redis, self.senderSettings.Queue, self.clock, func() bool { return true }, nil)
	if err != nil {
		t.Fatal(err)
	}
	self.writer, err = pushservice.NewWakingHintWriter(self.redis, nil, nil, nil, enqueuer)
	if err != nil {
		t.Fatal(err)
	}
	return self
}

// Ends the world: its senders, and the token writers' routing as before.
func (self *fs36World) close() {
	for _, closeSender := range self.closeSenders {
		closeSender()
	}
	self.restoreWriters()
	if self.ground.twin != nil {
		// WK1 at run time: the twin records a call whose keys share no tag
		if violations := self.ground.twin.redis.Violations(); len(violations) != 0 {
			self.t.Fatalf("%s: violations %v", self.ground.name, violations)
		}
	}
}

func (self *fs36World) shardCount() int {
	if self.senderSettings.Queue != nil {
		return self.senderSettings.Queue.ShardCount
	}
	return pushservice.DefaultWakeQueueSettings().ShardCount
}

// A new client in slot with a token row, posted as its app does at a launch and projected. Its keys and
// its wake shard's key sit in different slots. Returns the client and its token.
func (self *fs36World) client(slot int, name string) (server.Id, string) {
	self.t.Helper()
	for range 64 {
		clientId := self.ground.newClientInSlot(slot)
		shardKey := pushservice.WakeQueueKey(pushservice.WakeQueueShard(connect.Id(clientId), self.shardCount()))
		if server.RedisHashSlot(shardKey) == slot {
			continue
		}
		token := "synthetic-" + name + "-token"
		self.post(clientId, token)
		return clientId, token
	}
	self.t.Fatalf("%s: no client of slot %d whose shard sits apart", self.ground.name, slot)
	return server.Id{}, ""
}

// A POST of token for clientId, "" for an empty one, which must land.
func (self *fs36World) post(clientId server.Id, token string) *SetPushTokenResult {
	self.t.Helper()
	result, err := SetPushToken(self.ctx, clientId, sidecar.PushTokenPlatformApns, token, nil)
	if err != nil {
		self.t.Fatalf("%s: %v", self.ground.name, err)
	}
	if result.Applied != pushservice.WakeTokenApplied && result.Applied != pushservice.WakeTokenCurrent {
		self.t.Fatalf("%s: a POST's apply answered %q", self.ground.name, result.Applied)
	}
	self.ground.expectNewest(clientId, "after a POST")
	return result
}

// clientId's publisher refreshes live now. Nothing refreshes it again: the client is lost from here.
func (self *fs36World) live(clientId server.Id) {
	self.t.Helper()
	if err := self.redis.Set(self.ctx, pushservice.HintLiveKey(connect.Id(clientId)), []byte("1"), self.liveTimeout); err != nil {
		self.t.Fatalf("%s: %v", self.ground.name, err)
	}
}

// One hint to clientId through the waking writer.
func (self *fs36World) hint(clientId server.Id, hint *sidecar.ClientHint) *pushservice.PushHintResult {
	self.t.Helper()
	result, err := self.writer.HintClient(self.ctx, connect.Id(clientId), hint)
	if err != nil {
		self.t.Fatalf("%s: %v", self.ground.name, err)
	}
	return result
}

// A mail hint to clientId at version, which must find a token and be due now.
func (self *fs36World) hintDue(clientId server.Id, version uint64, when string) {
	self.t.Helper()
	result := self.hint(clientId, &sidecar.ClientHint{Topic: sidecar.PushTopicMail, Version: version, Body: []byte{1}})
	if result.Token == nil || result.Wake != pushservice.WakeEnqueueDue {
		self.t.Fatalf("%s, %s: token %q, wake %d", self.ground.name, when, result.Token, result.Wake)
	}
}

// A mail hint to clientId at version, which must find no token and queue nothing.
func (self *fs36World) hintNone(clientId server.Id, version uint64, when string) {
	self.t.Helper()
	result := self.hint(clientId, &sidecar.ClientHint{Topic: sidecar.PushTopicMail, Version: version, Body: []byte{1}})
	if result.Token != nil || result.Wake != pushservice.WakeEnqueueNone {
		self.t.Fatalf("%s, %s: token %q, wake %d", self.ground.name, when, result.Token, result.Wake)
	}
}

// Starts a sender, and returns once each of its readers has made its group.
func (self *fs36World) startSender(consumer string) {
	self.t.Helper()
	sender, err := pushservice.NewWakeSender(self.ctx, &pushservice.WakeSenderArgs{
		Redis:    self.redis,
		Tokens:   fs36TokenStore{},
		Vendors:  []pushservice.WakeVendor{self.apns},
		Consumer: consumer,
		Enabled:  func() bool { return true },
		Clock:    self.clock,
		Settings: self.senderSettings,
		Settled: func(settled *pushservice.WakeSettled) {
			self.settled <- settled
		},
	})
	if err != nil {
		self.t.Fatalf("%s: %v", self.ground.name, err)
	}
	self.closeSenders = append(self.closeSenders, sender.Close)
	self.awaitGroups()
}

// Returns once every shard's group has been made again.
func (self *fs36World) awaitGroups() {
	self.t.Helper()
	made := map[string]bool{}
	guard := time.After(fs36Timeout)
	for len(made) < self.shardCount() {
		select {
		case key := <-self.redis.made:
			made[key] = true
		case <-guard:
			self.t.Fatalf("%s: %d of %d groups made", self.ground.name, len(made), self.shardCount())
		}
	}
}

// Returns once count timers at at are pending on the sender's clock: its dispatcher waiting for a due, or
// its readers in their backoff.
func (self *fs36World) awaitTimers(at time.Time, count int) {
	self.t.Helper()
	guard := time.After(fs36Timeout)
	for {
		pending, added := self.clock.WatchTimers()
		found := 0
		for _, timer := range pending {
			if timer.Equal(at) {
				found += 1
			}
		}
		if count <= found {
			return
		}
		select {
		case <-added:
		case <-guard:
			self.t.Fatalf("%s: %d of %d timers %s after the start", self.ground.name, found, count, at.Sub(self.start))
		}
	}
}

// Moves the sender's clock on by d, and the pass's with it.
func (self *fs36World) advance(d time.Duration) {
	self.t.Helper()
	self.ground.advance(d)
	if self.ground.twin == nil {
		self.clock.Advance(d)
	}
}

// Moves the pass's clock on by d: the twin's one clock, the sender's too; beside a real Redis the pass's
// alone, the sender waiting on nothing meanwhile (the file's comment).
func (self *fs36World) advancePass(d time.Duration) {
	self.t.Helper()
	self.ground.advance(d)
}

// The next settlement, which must be result for clientId.
func (self *fs36World) expect(clientId server.Id, result pushservice.WakeSettledResult) *pushservice.WakeSettled {
	self.t.Helper()
	select {
	case settled := <-self.settled:
		if settled.ClientId != connect.Id(clientId) || settled.Result != result {
			self.t.Fatalf("%s: settled %+v, not %s for %s", self.ground.name, settled, result, clientId)
		}
		return settled
	case <-time.After(fs36Timeout):
		self.t.Fatalf("%s: nothing settled", self.ground.name)
		return nil
	}
}

// Every submission that carried token, in order.
func (self *fs36World) submissions(token string) []*sidecartest.FakeWakeSubmission {
	submissions := []*sidecartest.FakeWakeSubmission{}
	for _, submission := range self.apns.Submissions() {
		if submission.Token == token {
			submissions = append(submissions, submission)
		}
	}
	return submissions
}

// Fails unless token has had count submissions.
func (self *fs36World) expectSubmissions(token string, count int, when string) {
	self.t.Helper()
	if submissions := self.submissions(token); len(submissions) != count {
		self.t.Fatalf("%s, %s: %d submissions to %s, not %d", self.ground.name, when, len(submissions), token, count)
	}
}

// After a flush every reader meets its missing group, backs off and makes it again: the schedule waits
// for each. The twin's blocked reads meet it at the next stream write, which the schedule's hints were; a
// real master fails them at the flush or at their block's end.
func (self *fs36World) awaitGroupsAfterFlush() {
	self.t.Helper()
	backoff := self.senderSettings.ErrorBackoff
	self.awaitTimers(self.clock.Now().Add(backoff), self.shardCount())
	self.advance(backoff)
	self.awaitGroups()
}

// Live refreshed at 0 s and the client lost: a mail hint at 10 s is put off 15 s, since a resident seemed
// live, and gives one submission at 25 s +- 1 s to the row's token, and no second one.
func TestFs36LostClientsMailHintWakesOnceAt25sOverTheRows(t *testing.T) {
	runFs36(t, func(world *fs36World) {
		clientId, token := world.client(world.ground.slots[0], "lost-client")
		world.live(clientId)
		world.startSender("a")

		world.advance(10 * time.Second)
		result := world.hint(clientId, &sidecar.ClientHint{Topic: sidecar.PushTopicMail, Version: 41, Body: []byte{1}})
		if !result.Live || result.Wake != pushservice.WakeEnqueueDeferred {
			t.Fatalf("%s: the hint at 10 s: live %t, wake %d", world.ground.name, result.Live, result.Wake)
		}
		world.awaitTimers(world.start.Add(25*time.Second), 1)
		world.advance(14 * time.Second)
		world.expectSubmissions(token, 0, "24 s after the refresh")
		world.advance(2 * time.Second)
		if settled := world.expect(clientId, pushservice.WakeSettledSent); !settled.Acked || settled.TopicClass != "mail" {
			t.Fatalf("%s: %+v", world.ground.name, settled)
		}
		submissions := world.submissions(token)
		if len(submissions) != 1 {
			t.Fatalf("%s: %d submissions", world.ground.name, len(submissions))
		}
		if at := submissions[0].At.Sub(world.start); at < 24*time.Second || 26*time.Second < at {
			t.Fatalf("%s: the submission at %s, not 25 s +- 1 s", world.ground.name, at)
		}
		if payload := submissions[0].Payload; payload.TopicClass != "mail" || payload.Version != 41 {
			t.Fatalf("%s: the payload %+v", world.ground.name, payload)
		}
		world.advance(10 * time.Second)
		world.expectSubmissions(token, 1, "10 s on")
	})
}

// A hint the client's stack acked gives no submission: at 25 s the sender reads ack at the entry's hint
// and drops the entry.
func TestFs36AckedHintIsNotWokenOverTheRows(t *testing.T) {
	runFs36(t, func(world *fs36World) {
		clientId, token := world.client(world.ground.slots[0], "acking-client")
		world.live(clientId)
		world.startSender("a")

		world.advance(10 * time.Second)
		result := world.hint(clientId, &sidecar.ClientHint{Topic: sidecar.PushTopicMail, Version: 41, Body: []byte{1}})
		if result.Wake != pushservice.WakeEnqueueDeferred {
			t.Fatalf("%s: the hint at 10 s: wake %d", world.ground.name, result.Wake)
		}
		world.awaitTimers(world.start.Add(25*time.Second), 1)
		world.advance(2 * time.Second)
		if raised, err := pushservice.RaiseHintAck(world.ctx, world.redis, connect.Id(clientId), result.StreamId, world.hintSettings.AckTimeout); err != nil || !raised {
			t.Fatalf("%s: the ack: %t, %v", world.ground.name, raised, err)
		}
		world.advance(13 * time.Second)
		if settled := world.expect(clientId, pushservice.WakeSettledAcked); !settled.Acked {
			t.Fatalf("%s: %+v", world.ground.name, settled)
		}
		world.advance(10 * time.Second)
		world.expectSubmissions(token, 0, "after an acked hint")
	})
}

// A forced router miss gives one submission at once, whatever live says.
func TestFs36ForcedRouterMissWakesAtOnceOverTheRows(t *testing.T) {
	runFs36(t, func(world *fs36World) {
		clientId, token := world.client(world.ground.slots[0], "missed-client")
		world.live(clientId)
		world.startSender("a")

		world.advance(10 * time.Second)
		result := world.hint(clientId, &sidecar.ClientHint{Topic: sidecar.PushTopicCore, Version: 9, Forced: true})
		if !result.Live || result.Wake != pushservice.WakeEnqueueDue {
			t.Fatalf("%s: the forced hint: live %t, wake %d", world.ground.name, result.Live, result.Wake)
		}
		if settled := world.expect(clientId, pushservice.WakeSettledSent); !settled.Acked {
			t.Fatalf("%s: %+v", world.ground.name, settled)
		}
		submissions := world.submissions(token)
		if len(submissions) != 1 || !submissions[0].At.Equal(world.start.Add(10*time.Second)) || submissions[0].Payload.TopicClass != "core" {
			t.Fatalf("%s: submissions %+v", world.ground.name, submissions)
		}
		world.advance(20 * time.Second)
		world.expectSubmissions(token, 1, "20 s on")
	})
}

// After a flush nothing is woken, every token gone with the rest, and wakes resume on a token POST or the
// sentinel pass. Two clients' rows are older than the sweep reaches. One's app launches and posts its
// token, unchanged, and its next hint wakes at once. The other's never launches: the pass that follows the
// flush finds its slot's sentinel gone and reconciles nothing, nor does the next, 60 s on; the first pass at
// least 90 s after the finding applies its row, and its next hint wakes.
func TestFs36WakesResumeAfterAFlushOnATokenPostOrTheSentinelPass(t *testing.T) {
	runFs36(t, func(world *fs36World) {
		ground := world.ground
		posting, postingToken := world.client(ground.slots[0], "posting-client")
		waiting, waitingToken := world.client(ground.slots[1], "waiting-client")
		ageTestPushTokenRows(t, world.ctx, []server.Id{posting, waiting}, time.Hour)
		world.startSender("a")
		world.hintDue(posting, 1, "before the flush")
		world.expect(posting, pushservice.WakeSettledSent)
		world.hintDue(waiting, 1, "before the flush")
		world.expect(waiting, pushservice.WakeSettledSent)

		ground.flush()
		world.hintNone(posting, 2, "after the flush")
		world.hintNone(waiting, 2, "after the flush")
		world.awaitGroupsAfterFlush()

		// the app's launch posts the token it holds: nothing commits, and the row is applied as it stands
		if result := world.post(posting, postingToken); result.Outcome != PushTokenUnchanged {
			t.Fatalf("%s: the POST after the flush: %s", ground.name, result.Outcome)
		}
		world.hintDue(posting, 3, "after the POST")
		world.expect(posting, pushservice.WakeSettledSent)
		world.expectSubmissions(postingToken, 2, "after the POST")

		// no launch: the sentinel pass
		if report := ground.pass(); report.FoundSlotCount < 2 || report.ReconciledSlotCount != 0 {
			t.Fatalf("%s: the finding pass: %+v", ground.name, report)
		}
		world.hintNone(waiting, 3, "after the finding pass")
		world.advancePass(60 * time.Second)
		if report := ground.pass(); report.ReconciledSlotCount != 0 {
			t.Fatalf("%s: a pass 60 s after the finding reconciled: %+v", ground.name, report)
		}
		world.hintNone(waiting, 4, "60 s after the finding")
		world.advancePass(30 * time.Second)
		if report := ground.pass(); report.ReconciledSlotCount < 2 {
			t.Fatalf("%s: the pass 90 s after the finding: %+v", ground.name, report)
		}
		ground.expectNewest(waiting, "after the reconcile")
		world.hintDue(waiting, 5, "after the reconcile")
		world.expect(waiting, pushservice.WakeSettledSent)
		world.expectSubmissions(waitingToken, 2, "after the reconcile")
		world.expectSubmissions(postingToken, 2, "after the reconcile")
	})
}

// One master restarts empty and one from an older dump. The first held one client's token; the dump holds
// a second client's older token, the token a third has since deleted and the keys of a fourth whose row is
// gone since. Every row is a day old by then, past what the sweep reaches, and no app launches. Until the reconcile the
// first master's client has no token and is not woken. The pass that follows the restarts finds the slots
// and reconciles nothing, nor does the next, 60 s on; the first pass at least 90 s after the finding
// reconciles them. From then each client's hint wakes at its newest row's token, the first two's wakes
// resumed with no launch, and none reaches the deleted token or the removed client's.
func TestFs36TwoMastersRestartedWakesResumeAfterTheReconcile(t *testing.T) {
	runFs36(t, func(world *fs36World) {
		ground := world.ground
		if !ground.restarts() {
			ground.logNoRestart("FS-36's two restarted masters")
			return
		}
		emptied, emptiedToken := world.client(ground.otherSlot(), "emptied-masters-client")
		changed, dumpedToken := world.client(ground.slots[0], "dumped")
		deleted, deletedToken := world.client(ground.slots[1], "since-deleted")
		removed, removedToken := world.client(ground.slots[2], "removed-clients")
		dump := ground.dump()

		// since the dump: a new token, an empty POST, and a removal whose row the pass has deleted since
		newerToken := "synthetic-newer-token"
		world.post(changed, newerToken)
		world.post(deleted, "")
		var removalErr error
		server.Tx(world.ctx, func(tx server.PgTx) {
			_, removalErr = TombstonePushTokenInTx(world.ctx, tx, []server.Id{removed})
		})
		if removalErr != nil {
			t.Fatalf("%s: %v", ground.name, removalErr)
		}
		ground.expectNewest(removed, "after the removal")
		world.advancePass(ground.passSettings.RemovedRowRetention + time.Minute)
		if report := ground.pass(); report.DeletedRowCount != 1 || report.FoundSlotCount != 0 {
			t.Fatalf("%s: the pass 24 h after the removal: %+v", ground.name, report)
		}
		if row := ground.expectNewest(removed, "24 h after the removal"); row != nil {
			t.Fatalf("%s: the removed client's row stayed: %s", ground.name, row)
		}

		ground.restartOther()
		ground.restart(dump)
		for clientId, token := range map[server.Id]string{emptied: "", changed: dumpedToken, deleted: deletedToken, removed: removedToken} {
			want := ""
			if token != "" {
				want = string(pushservice.WakeTokenValue(sidecar.PushTokenPlatformApns, token))
			}
			if tok, _ := ground.tokenKeys(clientId); tok != want {
				t.Fatalf("%s, after the restarts: tok %q, not %q", ground.name, tok, want)
			}
		}
		world.hintNone(emptied, 1, "after its master restarted empty")
		if report := ground.pass(); report.FoundSlotCount < 4 || report.ReconciledSlotCount != 0 {
			t.Fatalf("%s: the finding pass: %+v", ground.name, report)
		}
		world.advancePass(60 * time.Second)
		if report := ground.pass(); report.ReconciledSlotCount != 0 {
			t.Fatalf("%s: a pass 60 s after the finding reconciled: %+v", ground.name, report)
		}
		world.hintNone(emptied, 2, "60 s after the finding")
		world.advancePass(30 * time.Second)
		if report := ground.pass(); report.ReconciledSlotCount < 4 {
			t.Fatalf("%s: the pass 90 s after the finding: %+v", ground.name, report)
		}
		for _, clientId := range []server.Id{emptied, changed, deleted, removed} {
			ground.expectNewest(clientId, "after the reconcile")
		}

		// a sender up after the restarts; no client has launched since
		world.startSender("after-the-restarts")
		world.hintDue(emptied, 3, "after the reconcile")
		world.expect(emptied, pushservice.WakeSettledSent)
		world.hintDue(changed, 1, "after the reconcile")
		world.expect(changed, pushservice.WakeSettledSent)
		world.hintNone(deleted, 1, "after the reconcile")
		world.hintNone(removed, 1, "after the reconcile")
		world.expectSubmissions(emptiedToken, 1, "after the reconcile")
		world.expectSubmissions(newerToken, 1, "after the reconcile")
		world.expectSubmissions(dumpedToken, 0, "after the reconcile")
		world.expectSubmissions(deletedToken, 0, "after the reconcile")
		world.expectSubmissions(removedToken, 0, "after the reconcile")
	})
}
