// Commit, cancellation and ambiguous-reply controls exercise the real owners.
package model

import (
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/server/v2026"
)

func TestLegacySettlementPostBatchCanceledPrefixAndResume(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		first, firstId, second, secondIds := legacyHeadRevisitFixture(t, ctx, 1)
		testingResetClock(ctx)
		defer testingResetClock(ctx)
		bounded, expire := context.WithCancelCause(ctx)
		defer expire(context.Canceled)
		visits := 0
		shard := int(firstId[15]) % LegacySettlementShardCount
		page, err := flushLegacySettlementsPage(ctx, bounded, shard, nil, 2,
			func(callCtx context.Context, id server.Id, wait *legacySettlementGrantWait) (bool, bool, legacySettlementBusyGate, error) {
				visits++
				if visits != 1 || id != firstId {
					t.Fatal("canceled page admitted another financial visit")
				}
				completed, busy, gate, err := flushLegacySettlementWithGrantWait(callCtx, id, wait)
				if err != nil || !completed || busy {
					t.Fatal("prefix did not reach the real committed owner", err)
				}
				// Cancellation occurs after its enqueuing post has joined and
				// before the next selection. No timeout or scheduler race orders it.
				expire(errLegacySettlementPageBudget)
				return completed, busy, gate, err
			})
		if err != nil || page.Completed != 1 || page.Visited != 1 || !page.More || page.Cursor == nil || page.Cursor.ContractId != firstId {
			t.Fatalf("cancellation lost the committed prefix: %+v %v", page, err)
		}
		requireLegacySettlementTestState(t, ctx, first, firstId, false, true, 989, 0)
		requireLegacySettlementTestState(t, ctx, second, secondIds[0], true, false, 1000000, 100)
		requireLegacyProviderDurability(t, ctx, first, firstId, 11)
		requireRedisExpiryClock(t, ctx, "11")
		resumed, err := FlushLegacySettlements(ctx, shard, page.Cursor, 2)
		if err != nil || resumed.Completed != 1 || resumed.Visited != 1 || resumed.Cursor != nil {
			t.Fatal("resumption did not own only the remaining intent", resumed, err)
		}
		requireLegacySettlementTestState(t, ctx, second, secondIds[0], false, true, 999989, 0)
		requireLegacyProviderDurability(t, ctx, second, secondIds[0], 11)
		requireRedisExpiryClock(t, ctx, "22")
		if replay, err := FlushLegacySettlements(ctx, shard, nil, 2); err != nil || replay.Visited != 0 {
			t.Fatal("canceled prefix replay repeated consumption", replay, err)
		}
		requireRedisExpiryClock(t, ctx, "22")
	})
}

func TestLegacySettlementPostBatchRollbackPublishesNothing(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		owner, id := legacySettlementTestIntent(t, ctx)
		streamId := AddToStream(ctx, id, owner.sourceId, owner.destinationId, nil)
		testingResetClock(ctx)
		defer testingResetClock(ctx)
		reconcileClockCandidate(ctx, 0)
		batched, finish := withLegacySettlementPostBatch(ctx)
		synthetic := errors.New("synthetic financial rollback before projection admission")
		var caught error
		server.HandleError(func() {
			server.Tx(batched, func(tx server.PgTx) {
				posts, complete, busy, _, err := flushLegacySettlementInTx(batched, tx, id)
				server.Raise(err)
				if !complete || busy || len(posts) == 0 {
					t.Fatal("rollback did not exercise the financial body and projection construction")
				}
				panic(synthetic)
			}, server.TxReadCommitted, server.OptNoRetry())
		}, func(err error) { caught = err })
		finish()
		finish()
		if caught != synthetic {
			t.Fatal("financial rollback changed its cause", caught)
		}
		requireLegacySettlementTestState(t, ctx, owner, id, true, false, 1000, 100)
		requireLegacyProviderDurability(t, ctx, owner, id, 0)
		requireRedisExpiryClock(t, ctx, "0")
		if got, _, ok := GetStream(ctx, id); !ok || got != streamId {
			t.Fatal("rolled-back financial attempt removed its live stream")
		}
		server.Db(ctx, func(conn server.PgConn) {
			var count int
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM pending_task WHERE function_name=$1`, legacyColdPageMirrorFunction).Scan(&count))
			if count != 0 {
				t.Fatal("rolled-back financial attempt retained durable mirror work")
			}
		})
		page, err := FlushLegacySettlements(ctx, int(id[15])%LegacySettlementShardCount, nil, 1)
		if err != nil || page.Completed != 1 {
			t.Fatal("rollback prevented later ordinary settlement", page, err)
		}
		requireRedisExpiryClock(t, ctx, "11")
		if _, _, ok := GetStream(ctx, id); ok {
			t.Fatal("committed page failed to remove the original stream")
		}
	})
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

func TestLegacySettlementPostBatchLostClockReplyIsNotReplayed(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		first, id, second, secondIds := legacyHeadRevisitFixture(t, ctx, 1)
		testingResetClock(ctx)
		defer testingResetClock(ctx)
		hook := &legacyPostClockReplyHook{}
		server.RedisDoOnce(ctx, func(client server.RedisClient) { client.AddHook(hook) })
		marked := context.WithValue(ctx, legacyPostClockReplyKey{}, hook)
		page, err := FlushLegacySettlements(marked, int(id[15])%LegacySettlementShardCount, nil, 2)
		if err != nil || page.Completed != 2 || page.Failed != 0 || hook.calls.Load() != 1 {
			t.Fatal("lost optional reply changed finances or repeated the batch", page, err, hook.calls.Load())
		}
		requireRedisExpiryClock(t, ctx, "22")
		requireLegacySettlementTestState(t, ctx, first, id, false, true, 989, 0)
		requireLegacySettlementTestState(t, ctx, second, secondIds[0], false, true, 999989, 0)
		requireLegacyProviderDurability(t, ctx, first, id, 11)
		requireLegacyProviderDurability(t, ctx, second, secondIds[0], 11)
		if replay, err := FlushLegacySettlements(marked, int(id[15])%LegacySettlementShardCount, nil, 2); err != nil || replay.Visited != 0 || hook.calls.Load() != 1 {
			t.Fatal("financial replay retried an ambiguous clock publication", replay, err)
		}
		requireRedisExpiryClock(t, ctx, "22")
	})
}

func TestLegacySettlementPostBatchBoundsDistinctMirrors(t *testing.T) {
	ctx, _ := withLegacySettlementPostBatch(t.Context())
	batch := ctx.Value(legacySettlementPostBatchKey{}).(*legacySettlementPostBatch)
	ids := make([]server.Id, LegacySettlementPageLimit+1)
	for index := range ids {
		ids[index] = server.NewId()
	}
	legacySettlementMirrorPost(ctx, ids)()
	legacySettlementMirrorPost(ctx, ids)()
	if len(batch.mirrorBalanceIdSet) != LegacySettlementPageLimit {
		t.Fatal("page mirror memory escaped its distinct-balance ceiling")
	}
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
