// The post batch a deadline owner shares across its turns: coalescing, and the
// ambiguous-reply control on its one clock pipeline.
package model

import (
	"context"
	"io"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/server"
)

// Batched clock and stream posts only record their committed values until the
// owner finishes the batch; a nested owner shares the outer batch.
func TestLegacySettlementPostBatchCoalescesClockAndStreamPosts(t *testing.T) {
	ctx, finish := withLegacySettlementPostBatch(t.Context())
	batch := ctx.Value(legacySettlementPostBatchKey{}).(*legacySettlementPostBatch)
	nested, nestedFinish := withLegacySettlementPostBatch(ctx)
	if nested.Value(legacySettlementPostBatchKey{}).(*legacySettlementPostBatch) != batch {
		t.Fatal("a nested owner replaced the shared batch")
	}
	// The outer owner alone drains the batch.
	nestedFinish()
	contractIds := []server.Id{server.NewId(), server.NewId()}
	for _, post := range []server.PostFunction{
		legacySettlementClockPost(nested, 11),
		legacySettlementStreamPost(nested, contractIds[0]),
		legacySettlementClockPost(ctx, 17),
		legacySettlementStreamPost(ctx, contractIds[1]),
	} {
		if result := post(); result != nil {
			t.Fatal("a batched post returned follow-up work", result)
		}
	}
	func() {
		batch.stateLock.Lock()
		defer batch.stateLock.Unlock()
		if !slices.Equal(batch.clockByteCounts, []ByteCount{11, 17}) || !slices.Equal(batch.streamContractIds, contractIds) {
			t.Fatal("the batch lost or reordered a committed projection", batch.clockByteCounts, batch.streamContractIds)
		}
		// An empty batch finishes without any Redis work.
		batch.clockByteCounts, batch.streamContractIds = nil, nil
	}()
	finish()
}

type legacyPostClockReplyKey struct{}

type legacyPostClockReplyHook struct {
	calls atomic.Int64
}

func (self *legacyPostClockReplyHook) DialHook(next redis.DialHook) redis.DialHook { return next }
func (self *legacyPostClockReplyHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return next
}
func (self *legacyPostClockReplyHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, commands []redis.Cmder) error {
		matched := false
		if ctx.Value(legacyPostClockReplyKey{}) == self {
			for _, command := range commands {
				args := command.Args()
				if command.Name() == "incrby" && len(args) > 1 && args[1] == clockTransferByteCountRedisKey {
					matched = true
				}
			}
		}
		err := next(ctx, commands)
		if matched {
			self.calls.Add(1)
			if err == nil {
				return io.ErrUnexpectedEOF
			}
		}
		return err
	}
}

// The batch publishes its clock increments in one non-retrying pipeline. A
// reply lost after Redis applied them is not replayed, and a second finish
// has nothing left to publish.
func TestLegacySettlementPostBatchLostClockReplyIsNotReplayed(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		testingResetClock(ctx)
		defer testingResetClock(ctx)
		hook := &legacyPostClockReplyHook{}
		server.RedisDoOnce(ctx, func(client server.RedisClient) { client.AddHook(hook) })
		marked := context.WithValue(ctx, legacyPostClockReplyKey{}, hook)
		batched, finish := withLegacySettlementPostBatch(marked)
		server.RunPosts(batched, legacySettlementClockPost(batched, 11), legacySettlementClockPost(batched, 11))
		// The reset clock is absent, and it stays absent until the owner finishes.
		if clock, ok := GetClock(ctx); hook.calls.Load() != 0 || ok {
			t.Fatal("a batched clock post published before its owner finished", hook.calls.Load(), clock)
		}
		finish()
		if hook.calls.Load() != 1 {
			t.Fatal("the batch did not publish its clock in one pipeline", hook.calls.Load())
		}
		requireRedisExpiryClock(t, ctx, "22")
		finish()
		if hook.calls.Load() != 1 {
			t.Fatal("a repeated finish retried an ambiguous clock publication", hook.calls.Load())
		}
		requireRedisExpiryClock(t, ctx, "22")
	})
}

func TestStreamRemovalBatchPreservesSharedMembersAndReplay(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		source, destination, intermediary := server.NewId(), server.NewId(), server.NewId()
		first, second, third, separate, missing := server.NewId(), server.NewId(), server.NewId(), server.NewId(), server.NewId()
		shared := AddToStream(ctx, first, source, destination, nil)
		if AddToStream(ctx, second, source, destination, nil) != shared || AddToStream(ctx, third, source, destination, nil) != shared {
			t.Fatal("fixture failed to share one stream across three contracts")
		}
		AddToStream(ctx, separate, source, destination, []server.Id{intermediary})
		before := GetStreamEventId(ctx, source)
		removed := removeFromStreams(ctx, []server.Id{first, separate, missing})
		if len(removed) != 2 || removed[first] != shared {
			t.Fatal("batch confused present and missing contract ownership")
		}
		for _, id := range []server.Id{second, third} {
			if got, _, ok := GetStream(ctx, id); !ok || got != shared {
				t.Fatal("removing one member removed another member's alias")
			}
		}
		_, hops := GetStreamHops(ctx, source)
		if len(hops) != 1 || GetStreamEventId(ctx, source) != before+1 {
			t.Fatal("batch changed the surviving hop or failed to publish one final-stream removal")
		}
		removeFromStreams(ctx, []server.Id{second, third})
		_, hops = GetStreamHops(ctx, source)
		if len(hops) != 0 || GetStreamEventId(ctx, source) != before+2 {
			t.Fatal("last shared member failed to remove exactly one stream")
		}
		if replay := removeFromStreams(ctx, []server.Id{first, second, third, separate, missing}); len(replay) != 0 || GetStreamEventId(ctx, source) != before+2 {
			t.Fatal("replay repeated stream removal or its dirty notification")
		}
	})
}

func TestStreamRemovalBatchIsolatesMalformedProjection(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		for _, failure := range []string{"lookup_type", "lookup_value", "membership_type"} {
			bad, good := server.NewId(), server.NewId()
			badSource, goodSource := server.NewId(), server.NewId()
			AddToStream(ctx, bad, badSource, server.NewId(), nil)
			AddToStream(ctx, good, goodSource, server.NewId(), nil)
			_, badKey, ok := GetStream(ctx, bad)
			if !ok {
				t.Fatal("malformed-neighbor fixture has no original stream")
			}
			before := GetStreamEventId(ctx, goodSource)
			server.Redis(ctx, func(r server.RedisClient) {
				switch failure {
				case "lookup_type":
					server.Raise(r.Del(ctx, contractStreamKey(bad)).Err())
					server.Raise(r.RPush(ctx, contractStreamKey(bad), "synthetic").Err())
				case "lookup_value":
					server.Raise(r.Set(ctx, contractStreamKey(bad), "short", time.Minute).Err())
				case "membership_type":
					server.Raise(r.Del(ctx, streamContractsKey(badKey)).Err())
					server.Raise(r.Set(ctx, streamContractsKey(badKey), "synthetic", time.Minute).Err())
				}
			})
			var caught error
			server.HandleError(func() { removeFromStreams(ctx, []server.Id{bad, good}) }, func(err error) { caught = err })
			if caught == nil {
				t.Fatal("malformed stream projection became successful cleanup", failure)
			}
			if _, _, ok := GetStream(ctx, good); ok {
				t.Fatal("malformed neighbor suppressed healthy stream cleanup", failure)
			}
			_, hops := GetStreamHops(ctx, goodSource)
			if len(hops) != 0 || GetStreamEventId(ctx, goodSource) != before+1 {
				t.Fatal("healthy neighbor lost its hop removal or atomic dirty notification", failure)
			}
			if _, ok := RemoveFromStream(ctx, good); ok || GetStreamEventId(ctx, goodSource) != before+1 {
				t.Fatal("healthy replay changed after a neighboring failure", failure)
			}
		}
	})
}
