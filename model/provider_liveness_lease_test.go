// The provider liveness lease's keys and writes (provider_liveness_lease.go):
// the pinned shard function and its cluster-safe keys, the refresh on the
// Redis node's clock and its pruning, the release, the writer stamps, and the
// lease as the client's rather than one connection's. The Redis tests run on
// the local test env.
package model

import (
	"context"
	"encoding/binary"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/server"
)

// The Redis node's clock, which scores every lease.
func livenessTestNodeTime(ctx context.Context, t testing.TB) time.Time {
	t.Helper()
	var nodeTime time.Time
	server.Redis(ctx, func(r server.RedisClient) {
		var err error
		nodeTime, err = r.Time(ctx).Result()
		if err != nil {
			t.Fatal(err)
		}
	})
	return nodeTime
}

// Sets members' scores in a sorted set as they are, in unix milliseconds.
func livenessTestScore(ctx context.Context, t testing.TB, key string, memberMillis map[string]int64) {
	t.Helper()
	server.Redis(ctx, func(r server.RedisClient) {
		for member, millis := range memberMillis {
			if err := r.ZAdd(ctx, key, redis.Z{Score: float64(millis), Member: member}).Err(); err != nil {
				t.Fatal(err)
			}
		}
	})
}

// The members of a sorted set and their scores.
func livenessTestMembers(ctx context.Context, t testing.TB, key string) map[string]int64 {
	t.Helper()
	memberMillis := map[string]int64{}
	server.Redis(ctx, func(r server.RedisClient) {
		members, err := r.ZRangeWithScores(ctx, key, 0, -1).Result()
		if err != nil {
			t.Fatal(err)
		}
		for _, member := range members {
			memberMillis[member.Member.(string)] = int64(member.Score)
		}
	})
	return memberMillis
}

// A synthetic id of the given shard: the first counter value that lands
// there, after skip of them.
func livenessTestIdInShard(shard int, skip int) server.Id {
	for i := uint64(0); ; i += 1 {
		var clientId server.Id
		clientId[0] = 0x01
		binary.BigEndian.PutUint64(clientId[8:], i)
		if providerLivenessLeaseShard(clientId) == shard {
			if skip == 0 {
				return clientId
			}
			skip -= 1
		}
	}
}

// Each key is one hash tag, so one cluster slot, and the shard function is
// pinned: connect writers and API readers of different builds must agree on
// it. Every shard takes its share of ids.
func TestProviderLivenessLeaseShardKeys(t *testing.T) {
	for shard := range providerLivenessLeaseShardCount {
		key := providerLivenessLeaseKey(shard)
		if !strings.HasPrefix(key, "{provider_live:") || strings.Index(key, "}") != len(key)-1 || len(key) != len("{provider_live:00}") {
			t.Fatalf("shard %d key %q is not one whole hash tag", shard, key)
		}
	}
	if providerLivenessLeaseKey(7) != "{provider_live:07}" || providerLivenessLeaseKey(31) != "{provider_live:31}" {
		t.Fatal("shard keys changed")
	}
	if !strings.HasPrefix(providerLivenessWritersKey, "{") || strings.Index(providerLivenessWritersKey, "}") != len(providerLivenessWritersKey)-1 {
		t.Fatalf("the writer set %q is not one whole hash tag", providerLivenessWritersKey)
	}

	for _, c := range []struct {
		clientId string
		shard    int
	}{
		{clientId: "00000000-0000-0000-0000-000000000000", shard: 5},
		{clientId: "018f2a3b-4c5d-7e6f-8a9b-0c1d2e3f4a5b", shard: 14},
		{clientId: "ffffffff-ffff-ffff-ffff-ffffffffffff", shard: 21},
		{clientId: "0199e7d2-1b3a-7c4d-9e5f-a1b2c3d4e5f6", shard: 28},
		{clientId: "0123abcd-0000-7000-8000-000000000001", shard: 10},
	} {
		if shard := providerLivenessLeaseShard(server.RequireParseId(c.clientId)); shard != c.shard {
			t.Fatalf("client %s is in shard %d, want the pinned %d", c.clientId, shard, c.shard)
		}
	}

	shardCounts := make([]int, providerLivenessLeaseShardCount)
	for i := range 100 * providerLivenessLeaseShardCount {
		var clientId server.Id
		clientId[0] = 0x01
		binary.BigEndian.PutUint64(clientId[8:], uint64(i))
		shardCounts[providerLivenessLeaseShard(clientId)] += 1
	}
	for shard, count := range shardCounts {
		if count < 50 || 150 < count {
			t.Fatalf("shard %d took %d of %d sequential ids, want about 100", shard, count, 100*providerLivenessLeaseShardCount)
		}
	}
}

// A process prunes each shard at most once an interval, and shards prune
// independently.
func TestProviderLivenessLeasePruneDue(t *testing.T) {
	shard := 7
	providerLivenessLeasePruneTimes[shard].Store(0)
	providerLivenessLeasePruneTimes[shard+1].Store(0)
	t.Cleanup(func() {
		providerLivenessLeasePruneTimes[shard].Store(0)
		providerLivenessLeasePruneTimes[shard+1].Store(0)
	})
	start := time.UnixMilli(1_800_000_000_000)
	for _, c := range []struct {
		shard int
		now   time.Time
		due   bool
	}{
		{shard: shard, now: start, due: true},
		{shard: shard, now: start, due: false},
		{shard: shard, now: start.Add(providerLivenessLeasePruneInterval - time.Millisecond), due: false},
		{shard: shard + 1, now: start, due: true},
		{shard: shard, now: start.Add(providerLivenessLeasePruneInterval), due: true},
		{shard: shard, now: start.Add(providerLivenessLeasePruneInterval), due: false},
	} {
		if due := providerLivenessLeasePruneDue(c.shard, c.now); due != c.due {
			t.Fatalf("shard %d at %s: prune due %t, want %t", c.shard, c.now.Sub(start), due, c.due)
		}
	}
}

// A refresh scores the client with the Redis node's clock, a later refresh
// moves it forward, and the release removes it; releasing again is harmless.
func TestProviderLivenessLeaseRefreshAndRelease(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		clientId := server.NewId()
		key := providerLivenessLeaseKey(providerLivenessLeaseShard(clientId))

		before := livenessTestNodeTime(ctx, t).Truncate(time.Millisecond)
		if err := RefreshProviderLivenessLease(ctx, clientId); err != nil {
			t.Fatal(err)
		}
		after := livenessTestNodeTime(ctx, t)
		refreshTime, ok := Testing_GetProviderLivenessLease(ctx, clientId)
		if !ok {
			t.Fatal("the refresh wrote no lease")
		}
		if refreshTime.Before(before) || refreshTime.After(after) {
			t.Fatalf("the lease is scored %s, outside the node's clock during the refresh [%s, %s]", refreshTime, before, after)
		}

		// the node's clock moves on by at least a millisecond
		for !before.Add(time.Millisecond).Before(livenessTestNodeTime(ctx, t)) {
		}
		if err := RefreshProviderLivenessLease(ctx, clientId); err != nil {
			t.Fatal(err)
		}
		laterTime, ok := Testing_GetProviderLivenessLease(ctx, clientId)
		if !ok || !refreshTime.Before(laterTime) {
			t.Fatalf("a later refresh scored the lease %s, want after %s", laterTime, refreshTime)
		}
		if members := livenessTestMembers(ctx, t, key); len(members) != 1 {
			t.Fatalf("the shard holds %d members after two refreshes of one client, want 1", len(members))
		}

		if err := ReleaseProviderLivenessLease(ctx, clientId); err != nil {
			t.Fatal(err)
		}
		if _, ok := Testing_GetProviderLivenessLease(ctx, clientId); ok {
			t.Fatal("the released lease remains")
		}
		if err := ReleaseProviderLivenessLease(ctx, clientId); err != nil {
			t.Fatalf("releasing a lease that is gone failed: %v", err)
		}
	})
}

// The refresh that finds its shard's prune due removes the members older than
// the prune age, and keeps the younger ones; a refresh before the prune is due
// removes nothing.
func TestProviderLivenessLeaseRefreshPrunesItsShard(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		clientId := server.NewId()
		shard := providerLivenessLeaseShard(clientId)
		key := providerLivenessLeaseKey(shard)
		t.Cleanup(func() {
			providerLivenessLeasePruneTimes[shard].Store(0)
		})

		nodeMillis := livenessTestNodeTime(ctx, t).UnixMilli()
		pruneMillis := providerLivenessLeasePruneAge.Milliseconds()
		livenessTestScore(ctx, t, key, map[string]int64{
			"test-old":    nodeMillis - pruneMillis - 10_000,
			"test-recent": nodeMillis - pruneMillis + 60_000,
		})

		providerLivenessLeasePruneTimes[shard].Store(time.Now().UnixMilli())
		if err := RefreshProviderLivenessLease(ctx, clientId); err != nil {
			t.Fatal(err)
		}
		members := livenessTestMembers(ctx, t, key)
		if _, ok := members["test-old"]; !ok {
			t.Fatal("a refresh pruned its shard before the prune was due")
		}

		providerLivenessLeasePruneTimes[shard].Store(time.Now().Add(-providerLivenessLeasePruneInterval).UnixMilli())
		if err := RefreshProviderLivenessLease(ctx, clientId); err != nil {
			t.Fatal(err)
		}
		members = livenessTestMembers(ctx, t, key)
		if _, ok := members["test-old"]; ok {
			t.Fatal("the due prune kept a member older than the prune age")
		}
		if _, ok := members["test-recent"]; !ok {
			t.Fatal("the prune removed a member younger than the prune age")
		}
		if _, ok := members[clientId.String()]; !ok {
			t.Fatal("the pruning refresh wrote no lease")
		}
	})
}

// A handler's stamp is scored with the node's clock and each stamp prunes the
// writer set of stamps older than the prune age.
func TestProviderLivenessWriterStamp(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		handlerId := server.NewId()
		nodeMillis := livenessTestNodeTime(ctx, t).UnixMilli()
		livenessTestScore(ctx, t, providerLivenessWritersKey, map[string]int64{
			"test-old-handler": nodeMillis - providerLivenessLeasePruneAge.Milliseconds() - 10_000,
		})

		before := livenessTestNodeTime(ctx, t).Truncate(time.Millisecond)
		if err := PublishProviderLivenessWriter(ctx, handlerId); err != nil {
			t.Fatal(err)
		}
		after := livenessTestNodeTime(ctx, t)
		stampTime, ok := Testing_GetProviderLivenessWriter(ctx, handlerId)
		if !ok || stampTime.Before(before) || stampTime.After(after) {
			t.Fatalf("the handler is stamped %s (%t), want within the node's clock [%s, %s]", stampTime, ok, before, after)
		}
		if _, ok := livenessTestMembers(ctx, t, providerLivenessWritersKey)["test-old-handler"]; ok {
			t.Fatal("the stamp kept a stamp older than the prune age")
		}
	})
}

// The lease is the client's: refreshes from two connections of one client keep
// one member, scored by the later refresh, so the lease lasts the window from
// the latest refresh of any of the client's connections, and nothing a
// connection writes is its own.
func TestProviderLivenessLeaseIsTheClients(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		clientId := server.NewId()
		key := providerLivenessLeaseKey(providerLivenessLeaseShard(clientId))
		// the first connection's refresh, then the second's
		if err := RefreshProviderLivenessLease(ctx, clientId); err != nil {
			t.Fatal(err)
		}
		first, _ := Testing_GetProviderLivenessLease(ctx, clientId)
		for !first.Add(time.Millisecond).Before(livenessTestNodeTime(ctx, t)) {
		}
		if err := RefreshProviderLivenessLease(ctx, clientId); err != nil {
			t.Fatal(err)
		}
		second, _ := Testing_GetProviderLivenessLease(ctx, clientId)
		members := livenessTestMembers(ctx, t, key)
		if len(members) != 1 || members[clientId.String()] != second.UnixMilli() || !first.Before(second) {
			t.Fatalf("two connections' refreshes left %v, want the client alone scored by the later refresh", members)
		}
	})
}

// The contract between connect writers and API readers, in time. A healthy
// connection survives one failed write without its process withholding the
// stamp: its shard's writes are a pass apart, passes start an interval and its
// jitter apart and each ends within its bound, so two passes and a pass's
// bound separate the successes around a failure. A process whose writes stop,
// for one shard or all, has its last stamp lapse at every reader before any
// lease it wrote lapses: a write's success is the time it started, up to a
// write's bound before its score, then the withhold age, the stamp's own
// write, the stamp window, the readers' cache and the bound of a reader's
// judging. Stamps renew well within their window. The writes run on their own
// interval: a refresh tied to sync rounds waited out a failed read's retries
// (up to a minute) and the longest backoff (two minutes), 180 s, past the
// lease window.
func TestProviderLivenessBudgetStaysUnderTheLeaseWindow(t *testing.T) {
	passGap := ProviderLivenessLeaseRefreshInterval + ProviderLivenessLeaseRefreshJitter
	oneMissedWrite := 2*passGap + ProviderLivenessLeasePassTimeout
	if ProviderLivenessWriterWithholdAge <= oneMissedWrite {
		t.Fatalf("a connection that misses one write reaches the withhold age: %s <= %s", ProviderLivenessWriterWithholdAge, oneMissedWrite)
	}
	lastEnforced := ProviderLivenessLeaseWriteTimeout + ProviderLivenessWriterWithholdAge + ProviderLivenessLeaseWriteTimeout +
		ProviderLivenessWriterWindow + ProviderLivenessCoverageCacheAge + ProviderLivenessCoverageJudgeTimeout
	if ProviderLivenessLeaseWindow <= lastEnforced {
		t.Fatalf("readers may enforce until %s after a lease's last score, past the %s lease window", lastEnforced, ProviderLivenessLeaseWindow)
	}
	if ProviderLivenessWriterWindow <= 2*(ProviderLivenessWriterInterval+ProviderLivenessLeaseWriteTimeout) {
		t.Fatalf("one failed stamp lapses the writer: %s window, %s interval", ProviderLivenessWriterWindow, ProviderLivenessWriterInterval)
	}
	if ProviderLivenessLeaseWindow <= passGap+ProviderLivenessLeasePassTimeout {
		t.Fatalf("a healthy lease lapses between passes: %s window", ProviderLivenessLeaseWindow)
	}
	if ProviderLivenessLeasePassTimeout < ProviderLivenessLeaseWriteTimeout {
		t.Fatalf("a pass's bound %s is shorter than one write's %s", ProviderLivenessLeasePassTimeout, ProviderLivenessLeaseWriteTimeout)
	}
}

// A shard write larger than one script's limit is split, and every client of
// it is written: the scored all at the node's clock, the held extended
// reported in order across the scripts.
func TestProviderLivenessLeaseShardWriteSplitsLargeBatches(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		shard := 6
		shardClientIds := []server.Id{}
		for i := uint64(0); len(shardClientIds) < 2*providerLivenessLeaseScriptMaxClients+12; i += 1 {
			var clientId server.Id
			clientId[0] = 0x02
			binary.BigEndian.PutUint64(clientId[8:], i)
			if providerLivenessLeaseShard(clientId) == shard {
				shardClientIds = append(shardClientIds, clientId)
			}
		}
		scored := shardClientIds[:2*providerLivenessLeaseScriptMaxClients+7]
		extended := []server.Id{}
		heldWant := []bool{}
		nodeMillis := livenessTestNodeTime(ctx, t).UnixMilli()
		for i := range 5 {
			clientId := shardClientIds[len(scored)+i]
			extended = append(extended, clientId)
			heldWant = append(heldWant, i%2 == 0)
			if i%2 == 0 {
				livenessTestScore(ctx, t, providerLivenessLeaseKey(shard), map[string]int64{clientId.String(): nodeMillis - 60_000})
			}
		}
		held, err := WriteProviderLivenessLeaseShard(ctx, shard, scored, extended, nil)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(held, heldWant) {
			t.Fatalf("held %v across the split scripts, want %v", held, heldWant)
		}
		members := livenessTestMembers(ctx, t, providerLivenessLeaseKey(shard))
		for _, clientId := range scored {
			if members[clientId.String()] < nodeMillis {
				t.Fatalf("a scored client of the split write is at %d, want at or after %d", members[clientId.String()], nodeMillis)
			}
		}
	})
}

// One shard script scores, extends only what the shard holds, and removes, at
// the node's clock, and refuses a client of another shard before writing.
func TestProviderLivenessLeaseShardWrite(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		shard := 4
		scored := livenessTestIdInShard(shard, 0)
		held := livenessTestIdInShard(shard, 1)
		unheld := livenessTestIdInShard(shard, 2)
		removed := livenessTestIdInShard(shard, 3)
		nodeMillis := livenessTestNodeTime(ctx, t).UnixMilli()
		livenessTestScore(ctx, t, providerLivenessLeaseKey(shard), map[string]int64{
			held.String():    nodeMillis - 60_000,
			removed.String(): nodeMillis - 60_000,
		})
		flags, err := WriteProviderLivenessLeaseShard(ctx, shard, []server.Id{scored}, []server.Id{held, unheld}, []server.Id{removed})
		if err != nil {
			t.Fatal(err)
		}
		if len(flags) != 2 || !flags[0] || flags[1] {
			t.Fatalf("extensions reported held %v, want the held member alone", flags)
		}
		members := livenessTestMembers(ctx, t, providerLivenessLeaseKey(shard))
		if _, ok := members[unheld.String()]; ok {
			t.Fatal("an extension created a member the shard did not hold")
		}
		if _, ok := members[removed.String()]; ok {
			t.Fatal("the removed member remains")
		}
		if members[scored.String()] < nodeMillis || members[held.String()] < nodeMillis {
			t.Fatalf("the scored and extended members are not at the node's clock: %v", members)
		}
		if _, err := WriteProviderLivenessLeaseShard(ctx, shard+1, []server.Id{scored}, nil, nil); err == nil {
			t.Fatal("a client of another shard was written")
		}
	})
}

// A lease write takes its own deadline and runs no retry loop: on a canceled
// context each write fails at once, where the shared pool's wrapper would
// raise and its client would retry.
func TestProviderLivenessLeaseWriteIsBounded(t *testing.T) {
	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	started := time.Now()
	if err := RefreshProviderLivenessLease(canceledCtx, server.NewId()); err == nil {
		t.Fatal("a refresh on a canceled context succeeded")
	}
	if err := PublishProviderLivenessWriter(canceledCtx, server.NewId()); err == nil {
		t.Fatal("a stamp on a canceled context succeeded")
	}
	if err := ReleaseProviderLivenessLease(canceledCtx, server.NewId()); err == nil {
		t.Fatal("a release on a canceled context succeeded")
	}
	if elapsed := time.Since(started); ProviderLivenessLeaseWriteTimeout < elapsed {
		t.Fatalf("writes on a canceled context took %s", elapsed)
	}
}
