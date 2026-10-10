// Committed close posts refresh balance mirrors through the bounded census. A
// grant over the refresh bound keeps its last published mirror instead of a
// per-close census of its whole history; every other grant refreshes exactly.
package model

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/urnetwork/server"
)

// The real owner-turn refresh: each member's deadline post records its
// balance, then the batch refreshes every recorded balance once. One grant is
// a row past netEscrowRefreshCensusRowBound, one is exactly at it, one is small.
// While a census names the over-bound grant, netEscrowCensusFence holds
// transfer_contract, so that census can end only when it is canceled, as the
// unbounded production census of a grant with 10^5 live legacy rows did.
func TestNetEscrowOwnerTurnRefreshDefersCensusBeyondBound(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		// An unbounded census can end only at its mirror deadline or fence.
		ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
		defer cancel()
		heavy := seedAdmissionCacheHistory(t, ctx, netEscrowRefreshCensusRowBound+1, 8)
		edge := seedAdmissionCacheHistory(t, ctx, netEscrowRefreshCensusRowBound, 8)
		light := seedAdmissionCacheHistory(t, ctx, 3, 20)
		const sentinel = ByteCount(7)
		server.Redis(ctx, func(r server.RedisClient) {
			server.Raise(r.Set(ctx, netEscrowKey(heavy.balanceId), int64(sentinel), time.Hour).Err())
		})
		fence := &netEscrowCensusFence{balanceId: heavy.balanceId}
		scope, err := server.NewTestPgQueryScope(ctx, fence)
		server.Raise(err)
		blocker, err := server.AcquireMaintenanceDbConn(ctx)
		if err != nil {
			server.Raise(scope.Close())
			t.Fatal(err)
		}
		// One cleanup order serves every exit: end the blocker's transaction,
		// return its connection, then retire the traced pools.
		finished := false
		finish := func() {
			if finished {
				return
			}
			finished = true
			if fence.blocker != nil {
				_ = fence.blocker.Rollback(context.Background())
			}
			blocker.Release()
			if err := scope.Close(); err != nil {
				t.Error("traced pool cleanup", err)
			}
		}
		defer finish()
		server.RaisePgResult(blocker.Exec(ctx, `SET default_transaction_read_only=off`))
		fence.blocker, err = blocker.Begin(ctx)
		server.Raise(err)
		deferredBefore := testutil.ToFloat64(netEscrowRefreshSnapshots.WithLabelValues("deferred"))
		refresh := &deadlineNetEscrowRefreshBatch{balanceIdSet: map[server.Id]bool{}}
		postCtx := context.WithValue(ctx, deadlineNetEscrowRefreshBatchKey{}, refresh)
		server.RunPosts(ctx, deadlineNetEscrowRefreshPost(postCtx, []server.Id{heavy.balanceId}),
			deadlineNetEscrowRefreshPost(postCtx, []server.Id{edge.balanceId}),
			deadlineNetEscrowRefreshPost(postCtx, []server.Id{light.balanceId}))
		refresh.finish(ctx)
		finish()
		engaged, ended := fence.snapshot()
		if engaged != 0 {
			t.Fatal("owner-turn refresh sent the over-bound balance to a census", engaged, ended)
		}
		if mirror := Testing_NetEscrowByteCount(ctx, light.balanceId); mirror != 3 {
			t.Fatal("owner-turn refresh did not publish the small grant exactly", mirror)
		}
		if mirror := Testing_NetEscrowByteCount(ctx, edge.balanceId); mirror != ByteCount(netEscrowRefreshCensusRowBound) {
			t.Fatal("owner-turn refresh did not publish the grant at the bound exactly", mirror)
		}
		if mirror := Testing_NetEscrowByteCount(ctx, heavy.balanceId); mirror != sentinel {
			t.Fatal("deferred balance mirror was rewritten", mirror)
		}
		if delta := testutil.ToFloat64(netEscrowRefreshSnapshots.WithLabelValues("deferred")) - deferredBefore; delta != 1 {
			t.Fatal("deferred refresh was not counted exactly once", delta)
		}
	})
}
