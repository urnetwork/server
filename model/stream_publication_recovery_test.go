// Stream publication retries repair durable membership after a dependency fault.
package model

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/server"
)

// One exact participant write fails before Redis receives it; all membership
// operations still use the real ordinary pool and committed Redis keys.
type streamParticipantFailureHook struct {
	key     string
	failed  atomic.Bool
	failure error
}

// Connection establishment remains unchanged.
func (self *streamParticipantFailureHook) DialHook(next redis.DialHook) redis.DialHook { return next }

// Only the first participant set fails, without a retryable transport error.
func (self *streamParticipantFailureHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, command redis.Cmder) error {
		args := command.Args()
		participantWrite := command.Name() == "set" && len(args) > 1 && args[1] == self.key ||
			command.Name() == "eval" && len(args) > 3 && args[3] == self.key
		if participantWrite && self.failed.CompareAndSwap(false, true) {
			command.SetErr(self.failure)
			return self.failure
		}
		return next(ctx, command)
	}
}

// Unrelated pipelines retain their actual driver behavior.
func (self *streamParticipantFailureHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

// Leaves the first contract committed with no participant or hop publication.
func failFirstStreamParticipantPublication(t testing.TB) (context.Context, server.Id, server.Id, streamKey) {
	t.Helper()
	source, destination, relay, contract := server.NewId(), server.NewId(), server.NewId(), server.NewId()
	ctx := WithAddStreamBinding(t.Context(), source, destination, []server.Id{relay})
	streamId := proposedStreamIdForAdd(ctx)
	key := newStreamKey(source, destination, []server.Id{relay})
	hook := &streamParticipantFailureHook{key: streamAuthorityKey(streamId, "p"), failure: errors.New("synthetic participant store failure")}
	server.Raise(server.RedisAuth(ctx, func(_ context.Context, r server.RedisClient) error {
		r.AddHook(hook)
		return nil
	}))
	var caught error
	server.HandleError(func() { AddToStream(ctx, contract, source, destination, []server.Id{relay}) }, func(err error) { caught = err })
	if !errors.Is(caught, hook.failure) || !hook.failed.Load() {
		t.Fatal("the controlled participant failure was not propagated", caught)
	}
	if actual, _, ok := GetStream(ctx, contract); !ok || actual != streamId {
		t.Fatal("participant failure did not leave the original committed binding")
	}
	for id := range key.Edges() {
		_, hops := GetStreamHops(ctx, id)
		if len(hops) != 0 {
			t.Fatal("a hop was published before participant authority")
		}
	}
	return ctx, contract, streamId, key
}

// Every participant receives its exact hop and one dirty event after repair.
func requireStreamPublicationRecovered(t testing.TB, ctx context.Context, streamId server.Id, key streamKey) {
	t.Helper()
	server.Raise(server.RedisAuth(ctx, func(ctx context.Context, r server.RedisClient) error {
		if r.Exists(ctx, streamAuthorityKey(streamId, "p")).Val() != 1 {
			t.Fatal("retry omitted participant authority")
		}
		return nil
	}))
	for id, edges := range key.Edges() {
		version, hops := GetStreamHops(ctx, id)
		want := NewStreamHop(edges[0], edges[1], streamId)
		if len(hops) != 1 || !hops[want] || version != 1 {
			t.Fatalf("retry did not publish exactly one unchanged hop: count=%d version=%d", len(hops), version)
		}
	}
}

// The unchanged membership branch must finish the first publication and remain
// idempotent through another duplicate and a later contract on the same path.
func TestStreamParticipantFailureReplayRepairsCommittedMembership(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, contract, streamId, key := failFirstStreamParticipantPublication(t)
		for _, member := range []server.Id{contract, contract, server.NewId()} {
			if actual := AddToStream(ctx, member, key.SourceId(), key.DestinationId(), key.IntermediaryIds()); actual != streamId {
				t.Fatal("retry replaced the committed stream identity")
			}
			requireStreamPublicationRecovered(t, ctx, streamId, key)
		}
	})
}

// A different contract can repair the same incomplete first publication.
func TestStreamParticipantFailureNewContractRepairsCommittedMembership(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, _, streamId, key := failFirstStreamParticipantPublication(t)
		if actual := AddToStream(ctx, server.NewId(), key.SourceId(), key.DestinationId(), key.IntermediaryIds()); actual != streamId {
			t.Fatal("later member replaced the committed stream identity")
		}
		requireStreamPublicationRecovered(t, ctx, streamId, key)
	})
}

// Companion membership has the same publication obligation as an ordinary add.
func TestStreamParticipantFailureCompanionRepairsCommittedMembership(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, contract, streamId, key := failFirstStreamParticipantPublication(t)
		if actual, ok := AddCompanionContractToStream(ctx, server.NewId(), contract, key.DestinationId(), key.SourceId()); !ok || actual != streamId {
			t.Fatal("companion could not recover the original stream")
		}
		requireStreamPublicationRecovered(t, ctx, streamId, key)
	})
}

// Repair neither renews duplicate membership nor shortens shared hop-set ttl.
func TestStreamPublicationReplayPreservesMembershipLifetime(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, contract, streamId, key := failFirstStreamParticipantPublication(t)
		AddToStream(ctx, contract, key.SourceId(), key.DestinationId(), key.IntermediaryIds())
		server.Redis(ctx, func(r server.RedisClient) {
			server.Raise(r.PExpire(ctx, streamIdKey(key), time.Minute).Err())
			server.Raise(r.PExpire(ctx, streamContractsKey(key), time.Minute).Err())
		})
		AddToStream(ctx, contract, key.SourceId(), key.DestinationId(), key.IntermediaryIds())
		requireStreamPublicationRecovered(t, ctx, streamId, key)
		server.Redis(ctx, func(r server.RedisClient) {
			if remaining := r.PTTL(ctx, streamIdKey(key)).Val(); remaining <= 0 || remaining > time.Minute {
				t.Fatal("duplicate renewed its stream lifetime", remaining)
			}
			for id := range key.Edges() {
				if remaining := r.PTTL(ctx, clientStreamHopsKey(id)).Val(); remaining < time.Hour {
					t.Fatal("duplicate shortened a shared hop-set lifetime", remaining)
				}
			}
		})
	})
}

// The source and every newly repaired key use the identical absolute expiry,
// measured by Redis itself rather than elapsed sleeps or the process clock.
func TestStreamPublicationRepairUsesOriginalRedisHorizon(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, contract, streamId, key := failFirstStreamParticipantPublication(t)
		var deadline time.Time
		server.Redis(ctx, func(r server.RedisClient) {
			now, err := r.Time(ctx).Result()
			server.Raise(err)
			deadline = now.Truncate(time.Millisecond).Add(time.Minute)
			server.Raise(r.PExpireAt(ctx, streamIdKey(key), deadline).Err())
			server.Raise(r.PExpireAt(ctx, streamContractsKey(key), deadline).Err())
		})
		AddToStream(ctx, contract, key.SourceId(), key.DestinationId(), key.IntermediaryIds())
		requireStreamPublicationRecovered(t, ctx, streamId, key)
		check := func(ctx context.Context, r server.RedisClient, redisKey string) {
			got, err := r.Eval(ctx, `local clock=redis.call('TIME'); local ttl=redis.call('PTTL',KEYS[1]); if ttl<=0 then return ttl end; return clock[1]*1000+math.floor(clock[2]/1000)+ttl`, []string{redisKey}).Int64()
			server.Raise(err)
			if got != deadline.UnixMilli() {
				t.Fatalf("repaired key expiry %d differs from original Redis horizon %d", got, deadline.UnixMilli())
			}
		}
		server.Redis(ctx, func(r server.RedisClient) {
			for _, redisKey := range []string{streamIdKey(key), streamContractsKey(key), contractStreamKey(contract), contractStreamId(contract)} {
				check(ctx, r, redisKey)
			}
			for id := range key.Edges() {
				check(ctx, r, clientStreamHopsKey(id))
			}
		})
		server.Raise(server.RedisAuth(ctx, func(ctx context.Context, r server.RedisClient) error {
			check(ctx, r, streamAuthorityKey(streamId, "p"))
			return nil
		}))
	})
}

// An exhausted horizon must not turn a zero duration into permanent authority.
func TestStreamPublicationExpiredHorizonCreatesNoAuthority(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		for _, deadline := range []int64{0, 1} {
			streamId := server.NewId()
			key := newStreamKey(server.NewId(), server.NewId(), nil)
			if err := storeStreamParticipantsUntil(ctx, streamId, key, deadline); err == nil {
				t.Fatal("exhausted membership published participant authority")
			}
			server.Raise(server.RedisAuth(ctx, func(ctx context.Context, r server.RedisClient) error {
				if r.Exists(ctx, streamAuthorityKey(streamId, "p")).Val() != 0 {
					t.Fatal("exhausted membership left a participant key")
				}
				return nil
			}))
		}
	})
}
