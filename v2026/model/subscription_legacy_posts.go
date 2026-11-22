// One owner coalesces optional projections after its financial visits have committed.
package model

import (
	"context"
	"sync"
	"time"

	"github.com/urnetwork/server/v2026"
)

type legacySettlementPostBatchKey struct{}

// Only confirmed transaction posts add work concurrently. The owner joins them
// before draining this batch once. Clock and stream entries are bounded by the
// owner's financial visits.
type legacySettlementPostBatch struct {
	stateLock         sync.Mutex
	clockByteCounts   []ByteCount
	streamContractIds []server.Id
}

// The turns of one deadline owner share a batch (withContractDeadlineOwnerPosts).
// Direct single-contract calls keep their own synchronous posts. No worker
// outlives the owner. The best-effort clock can lag committed accounting until
// the owner finishes. Process exit before that drain can lose the queued clock
// increments; its aggregate backfill cannot establish exact per-contract
// delivery. Financial completion remains the committed outcome, debit and
// durable payout authority.
func withLegacySettlementPostBatch(ctx context.Context) (context.Context, func()) {
	if batch, _ := ctx.Value(legacySettlementPostBatchKey{}).(*legacySettlementPostBatch); batch != nil {
		return ctx, func() {}
	}
	batch := &legacySettlementPostBatch{}
	return context.WithValue(ctx, legacySettlementPostBatchKey{}, batch), func() { batch.finish(ctx) }
}

// Capture one committed destination report without adding a Redis wait between visits.
func legacySettlementClockPost(ctx context.Context, byteCount ByteCount) server.PostFunction {
	batch, _ := ctx.Value(legacySettlementPostBatchKey{}).(*legacySettlementPostBatch)
	if batch == nil {
		return clockTransferPost(ctx, byteCount)
	}
	return func() any {
		batch.stateLock.Lock()
		defer batch.stateLock.Unlock()
		batch.clockByteCounts = append(batch.clockByteCounts, byteCount)
		return nil
	}
}

// A foreground legacy settlement's optional warm mirror publishes only current
// durable cache entries; its financial commit already queued the cold repair.
func legacySettlementMirrorPost(ctx context.Context, balanceIds []server.Id) server.PostFunction {
	return func() any {
		refreshCachedLegacyNetEscrow(ctx, balanceIds)
		return nil
	}
}

// Queue only the committed contract identity; the final batch reads current Redis state.
func legacySettlementStreamPost(ctx context.Context, contractId server.Id) server.PostFunction {
	batch, _ := ctx.Value(legacySettlementPostBatchKey{}).(*legacySettlementPostBatch)
	if batch == nil {
		return func() any {
			RemoveFromStream(ctx, contractId)
			return nil
		}
	}
	return func() any {
		batch.stateLock.Lock()
		defer batch.stateLock.Unlock()
		batch.streamContractIds = append(batch.streamContractIds, contractId)
		return nil
	}
}

// The clock keeps individual integer increments in one non-retrying pipeline:
// no in-memory sum can overflow and an ambiguous partial reply is never replayed.
// Its bounded detached lifetime also covers a committed prefix when the owner
// is canceled. Stream cleanup retains its cancellation and expiry policy.
func (self *legacySettlementPostBatch) finish(ctx context.Context) {
	var clocks []ByteCount
	var streams []server.Id
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		clocks, streams = self.clockByteCounts, self.streamContractIds
		self.clockByteCounts, self.streamContractIds = nil, nil
	}()
	var posts []server.PostFunction
	if len(clocks) > 0 {
		posts = append(posts, func() any {
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
		})
	}
	if len(streams) > 0 {
		posts = append(posts, func() any {
			removeFromStreams(ctx, streams)
			return nil
		})
	}
	if len(posts) > 0 {
		server.RunPosts(context.WithoutCancel(ctx), posts...)
	}
}
