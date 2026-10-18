// Scheduled net-escrow repair bounds the work of every reservation census
// statement. A cache-miss balance whose live legacy history exceeds the census
// bound is deferred, and the rest of its page is still reconciled exactly.
package model

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/urnetwork/server"
)

// Holds transfer_contract from the moment a census statement naming the
// over-bound balance is sent until that statement ends, so it can finish only
// at its PostgreSQL statement fence, as a production census of a balance with
// about 10^5 live legacy rows did. Other statements are not obstructed. Its
// methods are safe for concurrent use.
type netEscrowCensusFence struct {
	balanceId server.Id
	blocker   pgx.Tx
	stateLock sync.Mutex
	engaged   int
	ended     []error
}

// Marks the census statement whose end releases the fence.
type netEscrowCensusFenceKey struct{}

// Takes the table lock on the blocker before the marked census is sent. The
// blocker's own statements are traced too and return before taking stateLock.
func (self *netEscrowCensusFence) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if data.SQL != netEscrowReservationPageSQL || len(data.Args) == 0 {
		return ctx
	}
	if balanceIds, ok := data.Args[0].([]server.Id); !ok || !slices.Contains(balanceIds, self.balanceId) {
		return ctx
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.engaged++
	if self.engaged == 1 {
		if _, err := self.blocker.Exec(ctx, `LOCK TABLE transfer_contract IN ACCESS EXCLUSIVE MODE`); err != nil {
			self.ended = append(self.ended, err)
		}
	}
	return context.WithValue(ctx, netEscrowCensusFenceKey{}, true)
}

// Records how the marked census ended and releases the table lock.
func (self *netEscrowCensusFence) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	if marked, _ := ctx.Value(netEscrowCensusFenceKey{}).(bool); marked {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.ended = append(self.ended, data.Err)
		if err := self.blocker.Rollback(context.Background()); err != nil {
			self.ended = append(self.ended, err)
		}
	}
}

// Engagements and statement ends observed so far.
func (self *netEscrowCensusFence) snapshot() (int, []error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.engaged, slices.Clone(self.ended)
}

// One page holds an over-bound balance (one live legacy row past the bound), a
// balance exactly at the bound and a small one, all cache misses. Unbounded,
// the page's single census names the over-bound balance and ends at its
// two-minute fence, which aborts the whole pass (the production failure). The
// bounded pass never names that balance in a census, keeps its mirror and
// drift untouched, and still repairs the other two mirrors exactly.
func TestNetEscrowReconcileDefersCensusBeyondBound(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		// An unbounded census can end only at its two-minute server fence.
		ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
		defer cancel()
		heavy := seedAdmissionCacheHistory(t, ctx, netEscrowReconcileCandidateLimit+1, 8)
		edge := seedAdmissionCacheHistory(t, ctx, netEscrowReconcileCandidateLimit, 8)
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
		var drift map[server.Id]ByteCount
		var count int
		var passErr error
		server.HandleError(func() { drift, count = ReconcileCachedNetEscrow(ctx) }, func(err error) { passErr = err })
		finish()
		engaged, ended := fence.snapshot()
		if passErr != nil || engaged != 0 {
			t.Fatal("scheduled pass sent the over-bound balance to a census and ended at its fence", passErr, engaged, ended)
		}
		if count != 2 || drift[light.sourceNetworkId] != -3 || drift[edge.sourceNetworkId] != -ByteCount(netEscrowReconcileCandidateLimit) {
			t.Fatal("bounded pass lost the exact repair of the rest of its page", count, drift)
		}
		if Testing_NetEscrowByteCount(ctx, light.balanceId) != 3 || Testing_NetEscrowByteCount(ctx, edge.balanceId) != ByteCount(netEscrowReconcileCandidateLimit) {
			t.Fatal("bounded pass did not publish exact mirrors for the censused balances")
		}
		if _, drifted := drift[heavy.sourceNetworkId]; drifted || Testing_NetEscrowByteCount(ctx, heavy.balanceId) != sentinel {
			t.Fatal("deferred balance was published or reported as drift", drift)
		}
		if delta := testutil.ToFloat64(netEscrowRefreshSnapshots.WithLabelValues("deferred")) - deferredBefore; delta != 1 {
			t.Fatal("deferred census was not counted exactly once", delta)
		}
	})
}

// Census statements keep page order within both the balance and row budgets of
// each path's bound; an over-limit or unprobed balance is deferred instead.
func TestNetEscrowReconcileCensusPlanBoundsStatements(t *testing.T) {
	for _, bound := range []netEscrowCensusBound{netEscrowReconcileCensusBound, netEscrowRefreshCensusBound} {
		var balanceIds, wantDeferred []server.Id
		counts := map[server.Id]int{}
		add := func(n int, count int, probed bool) {
			for range n {
				balanceId := server.NewId()
				balanceIds = append(balanceIds, balanceId)
				if probed {
					counts[balanceId] = count
				}
				if !probed || bound.candidateLimit < count {
					wantDeferred = append(wantDeferred, balanceId)
				}
			}
		}
		add(netEscrowReconcileStatementBalances+5, 0, true)
		add(1, bound.candidateLimit+1, true)
		add(1, 0, false)
		add(2*bound.statementRows/bound.candidateLimit+3, bound.candidateLimit, true)
		add(3, 1, true)
		statements, deferred := planNetEscrowCensus(balanceIds, counts, bound)
		if !slices.Equal(deferred, wantDeferred) {
			t.Fatal("census plan deferred the wrong balances", bound, len(deferred), len(wantDeferred))
		}
		var planned []server.Id
		for _, statement := range statements {
			rowCount := 0
			for _, balanceId := range statement {
				rowCount += counts[balanceId]
			}
			if len(statement) == 0 || netEscrowReconcileStatementBalances < len(statement) || bound.statementRows < rowCount {
				t.Fatal("census statement exceeded its balance or row budget", bound, len(statement), rowCount)
			}
			planned = append(planned, statement...)
		}
		want := withoutNetEscrowBalances(balanceIds, wantDeferred)
		if !slices.Equal(planned, want) || len(statements) != 4 {
			t.Fatal("census plan lost, repeated or reordered a probed balance", bound, len(planned), len(want), len(statements))
		}
	}
}
