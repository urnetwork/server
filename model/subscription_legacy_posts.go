// One page coalesces optional projections after its financial visits have committed.
package model

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/urnetwork/server"
)

type legacySettlementPostBatchKey struct{}

// Only confirmed transaction posts add work concurrently. The page joins them
// before draining this owner once. Clock and stream entries are bounded by the
// financial visit ceiling; at most that many distinct mirror balances are kept.
// Every omitted or failed mirror already has its durable task in the same commit.
type legacySettlementPostBatch struct {
	stateLock          sync.Mutex
	clockByteCounts    []ByteCount
	streamContractIds  []server.Id
	mirrorBalanceIdSet map[server.Id]bool
}

// A shard's payer and chronological traversals share one owner. A direct
// chronological page constructs its own; direct single-contract calls keep the
// existing synchronous posts. No worker outlives the page.
// The best-effort clock can lag committed accounting until this page finishes.
// Process exit before that drain can lose the page's queued clock increments;
// its aggregate backfill cannot establish exact per-contract delivery. Financial
// completion remains the committed outcome, debit and durable payout authority.
func withLegacySettlementPostBatch(ctx context.Context) (context.Context, func()) {
	if batch, _ := ctx.Value(legacySettlementPostBatchKey{}).(*legacySettlementPostBatch); batch != nil {
		return ctx, func() {}
	}
	batch := &legacySettlementPostBatch{mirrorBalanceIdSet: map[server.Id]bool{}}
	return context.WithValue(ctx, legacySettlementPostBatchKey{}, batch), func() { batch.finish(ctx) }
}

// Capture one committed destination report without adding a Redis wait between visits.
func legacySettlementClockPost(ctx context.Context, byteCount ByteCount) server.PostFunction {
	batch, _ := ctx.Value(legacySettlementPostBatchKey{}).(*legacySettlementPostBatch)
	if batch == nil {
		return observeLegacySettlementPost(ctx, legacySettlementClock, clockTransferPost(ctx, byteCount))
	}
	return func() any {
		func() {
			batch.stateLock.Lock()
			defer batch.stateLock.Unlock()
			batch.clockByteCounts = append(batch.clockByteCounts, byteCount)
		}()
		traceLegacySettlement(ctx, "clock_post", "batched")
		return nil
	}
}

// Coalesce optional warm mirrors; the financial commit already owns cold repair.
func legacySettlementMirrorPost(ctx context.Context, balanceIds []server.Id) server.PostFunction {
	batch, _ := ctx.Value(legacySettlementPostBatchKey{}).(*legacySettlementPostBatch)
	if batch == nil {
		return observeLegacySettlementPost(ctx, legacySettlementMirror, func() any {
			refreshCachedLegacyNetEscrow(ctx, balanceIds)
			return nil
		})
	}
	return func() any {
		func() {
			batch.stateLock.Lock()
			defer batch.stateLock.Unlock()
			for _, id := range balanceIds {
				if len(batch.mirrorBalanceIdSet) < LegacySettlementPageLimit {
					batch.mirrorBalanceIdSet[id] = true
				}
			}
		}()
		traceLegacySettlement(ctx, "mirror_post", "batched")
		return nil
	}
}

// Queue only the committed contract identity; the final batch reads current Redis state.
func legacySettlementStreamPost(ctx context.Context, contractId server.Id) server.PostFunction {
	batch, _ := ctx.Value(legacySettlementPostBatchKey{}).(*legacySettlementPostBatch)
	if batch == nil {
		return observeLegacySettlementPost(ctx, legacySettlementStream, func() any {
			RemoveFromStream(ctx, contractId)
			return nil
		})
	}
	return func() any {
		func() {
			batch.stateLock.Lock()
			defer batch.stateLock.Unlock()
			batch.streamContractIds = append(batch.streamContractIds, contractId)
		}()
		traceLegacySettlement(ctx, "stream_post", "batched")
		return nil
	}
}

// The clock keeps individual integer increments in one non-retrying pipeline:
// no in-memory sum can overflow and an ambiguous partial reply is never replayed.
// Its existing bounded detached lifetime also covers a committed prefix when
// the financial page is canceled. A mirror batch has one detached second and
// durable recovery; stream cleanup retains its cancellation and expiry policy.
func (self *legacySettlementPostBatch) finish(ctx context.Context) {
	var clocks []ByteCount
	var streams, balances []server.Id
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		clocks, streams = self.clockByteCounts, self.streamContractIds
		for id := range self.mirrorBalanceIdSet {
			balances = append(balances, id)
		}
		self.clockByteCounts, self.streamContractIds = nil, nil
		clear(self.mirrorBalanceIdSet)
	}()
	var posts []server.PostFunction
	if len(clocks) > 0 {
		posts = append(posts, observeLegacySettlementPost(ctx, legacySettlementClock, func() any {
			bounded, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			defer cancel()
			server.RedisDoOnce(bounded, func(r server.RedisClient) {
				pipe := r.Pipeline()
				for _, count := range clocks {
					pipe.IncrBy(bounded, clockTransferByteCountRedisKey, count)
				}
				_, err := pipe.Exec(bounded)
				server.Raise(err)
			})
			return nil
		}))
	}
	if len(balances) > 0 {
		slices.SortFunc(balances, server.Id.Cmp)
		posts = append(posts, observeLegacySettlementPost(ctx, legacySettlementMirror, func() any {
			refreshCachedLegacyNetEscrow(context.WithoutCancel(ctx), balances)
			return nil
		}))
	}
	if len(streams) > 0 {
		posts = append(posts, observeLegacySettlementPost(ctx, legacySettlementStream, func() any {
			removeFromStreams(ctx, streams)
			return nil
		}))
	}
	if len(posts) > 0 {
		defer enterLegacySettlementTiming(ctx, legacySettlementJoinedPosts)()
		server.RunPosts(context.WithoutCancel(ctx), posts...)
	}
}
