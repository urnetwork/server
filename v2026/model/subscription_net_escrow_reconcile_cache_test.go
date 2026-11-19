// Reconciliation repairs Redis from committed PostgreSQL authority. These
// controls keep history unavailable at the exact warm-page boundary and retain
// invalidation, dry-run and separate reservation-owner behavior.
package model

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/urnetwork/server/v2026"
)

// The current page must publish its cached amount before the next discovery
// query takes its escrow relation lock. The baseline instead waits in the full
// census before publication; no timing-based speedup assertion is needed.
func TestNetEscrowReconcileWarmPageAvoidsHistory(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
		defer cancel()
		f := seedAdmissionCacheHistory(t, ctx, 10001, 8)
		ids := []server.Id{f.balanceId}
		warm := readMirrorNetEscrowSnapshots(ctx, ids)
		if len(warm) != 1 || warm[f.balanceId].reserved != 10001 {
			t.Fatal("history fixture did not warm its exact committed snapshot")
		}
		Testing_DeleteNetEscrow(ctx, f.balanceId)

		conn := acquireContractLifecycleTestConnection(t, ctx)
		defer conn.Release()
		held, err := conn.Begin(ctx)
		server.Raise(err)
		defer held.Rollback(context.Background())
		server.RaisePgResult(held.Exec(ctx, `LOCK TABLE transfer_escrow IN ACCESS EXCLUSIVE MODE`))
		type result struct {
			drift map[server.Id]ByteCount
			count int
			err   error
		}
		done := make(chan result, 1)
		go func() {
			r := result{}
			server.HandleError(func() { r.drift, r.count = ReconcileCachedNetEscrow(ctx) }, func(err error) { r.err = err })
			done <- r
		}()

		observedWait, censusWait := false, false
		waitCtx, waitCancel := context.WithTimeout(ctx, 5*time.Second)
		defer waitCancel()
		tick := time.NewTicker(20 * time.Millisecond)
		defer tick.Stop()
		for !observedWait && waitCtx.Err() == nil {
			server.Db(ctx, func(observer server.PgConn) {
				server.Raise(observer.QueryRow(ctx, `
					SELECT count(*) > 0,
						COALESCE(bool_or(position('SELECT requested_balance.balance_id,' IN activity.query)>0),false)
					FROM pg_locks held JOIN pg_stat_activity activity ON activity.pid=held.pid
					WHERE held.relation='transfer_escrow'::regclass AND NOT held.granted
						AND activity.datname=current_database() AND activity.state='active'
						AND activity.wait_event='relation'`).Scan(&observedWait, &censusWait))
			})
			if !observedWait {
				select {
				case <-tick.C:
				case <-waitCtx.Done():
				}
			}
		}
		publishedWhileHistoryHeld := Testing_NetEscrowByteCount(ctx, f.balanceId)
		server.Raise(held.Rollback(ctx))
		select {
		case r := <-done:
			if r.err != nil || r.count != 1 || r.drift[f.sourceNetworkId] != -10001 {
				t.Fatal("reconciliation did not join with the exact one-balance repair")
			}
		case <-ctx.Done():
			t.Fatal("reconciliation did not join after history was released")
		}
		if !observedWait || censusWait || publishedWhileHistoryHeld != 10001 {
			t.Fatalf("warm reconciliation revisited history: wait=%t census=%t published=%d", observedWait, censusWait, publishedWhileHistoryHeld)
		}
		t.Log("current warm page published 10001 legacy reservations while historical escrow access was held")
	})
}

// Scheduled repair retains a live expired balance, reuses its current cache,
// and rebuilds it after one contract changes without erasing its neighbor.
// The exact operator dry run never warms PostgreSQL or changes Redis.
func TestNetEscrowReconcileCacheNoncurrentInvalidation(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
		defer cancel()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		first, _ := createNetEscrowOrderingTestContract(ctx, f, 17)
		createNetEscrowOrderingTestContract(ctx, f, 23)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET start_time=now()-interval '2 hours',end_time=now()-interval '1 hour' WHERE balance_id=$1`, f.balanceId))
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM transfer_balance_net_escrow_snapshot WHERE balance_id=$1`, f.balanceId))
		})
		drift, count := ReconcileNetEscrow(ctx, false)
		var cached bool
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM transfer_balance_net_escrow_snapshot WHERE balance_id=$1)`, f.balanceId).Scan(&cached))
		})
		if count != 1 || drift[f.sourceNetworkId] != -40 || cached || Testing_NetEscrowByteCount(ctx, f.balanceId) != 0 {
			t.Fatal("dry run changed a mirror or failed to retain the expired live reservation")
		}
		for _, step := range []struct {
			name   string
			mutate bool
			warm   bool
			want   ByteCount
			reload float64
		}{
			{"cold fleet", false, false, 40, 1},
			{"warm fleet", false, true, 40, 0},
			{"invalidated fleet", true, false, 42, 1},
			{"warm new revision", false, true, 42, 0},
		} {
			if step.mutate {
				server.Tx(ctx, func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_escrow SET balance_byte_count=19 WHERE contract_id=$1`, first.ContractId))
				})
			}
			if step.warm {
				// A targeted post supplies this optional current snapshot; the
				// fleet reader must not manufacture cache rows for cold balances.
				readMirrorNetEscrowSnapshots(ctx, []server.Id{f.balanceId})
			}
			Testing_DeleteNetEscrow(ctx, f.balanceId)
			beforeReload := testutil.ToFloat64(netEscrowRefreshSnapshots.WithLabelValues("reloaded"))
			beforeReuse := testutil.ToFloat64(netEscrowRefreshSnapshots.WithLabelValues("reused"))
			drift, count = ReconcileCachedNetEscrow(ctx)
			actualDrift := drift[f.sourceNetworkId]
			reloads := testutil.ToFloat64(netEscrowRefreshSnapshots.WithLabelValues("reloaded")) - beforeReload
			reused := testutil.ToFloat64(netEscrowRefreshSnapshots.WithLabelValues("reused")) - beforeReuse
			if count != 1 || actualDrift != -step.want || Testing_NetEscrowByteCount(ctx, f.balanceId) != step.want || reloads != step.reload || reused != 1-step.reload {
				t.Fatalf("%s did not preserve exact cached authority: count=%d reloads=%v reused=%v", step.name, count, reloads, reused)
			}
			if step.reload == 1 {
				server.Db(ctx, func(conn server.PgConn) {
					if len(readCachedNetEscrowSnapshots(ctx, conn, []server.Id{f.balanceId})) != 0 {
						t.Fatal("fleet reconciliation wrote a cold or stale snapshot cache")
					}
				})
			}
		}
	})
}

// Operator audit and repair remain independent of the optional snapshot. A
// wrong amount with a matching revision must not make matching Redis drift
// appear healthy in either the fleet or per-network entry point.
func TestNetEscrowReconcileOperatorIgnoresCorruptedCache(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		createNetEscrowOrderingTestContract(ctx, f, 17)
		createNetEscrowOrderingTestContract(ctx, f, 23)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance_net_escrow_snapshot SET reserved_byte_count=999 WHERE balance_id=$1`, f.balanceId))
		})
		for _, network := range []bool{false, true} {
			server.Redis(ctx, func(r server.RedisClient) {
				server.Raise(r.Set(ctx, netEscrowKey(f.balanceId), 999, time.Hour).Err())
			})
			for _, apply := range []bool{false, true} {
				var drift ByteCount
				var count int
				if network {
					drift, count = ReconcileNetEscrowForNetwork(ctx, f.sourceNetworkId, apply)
				} else {
					byNetwork, total := ReconcileNetEscrow(ctx, apply)
					drift, count = byNetwork[f.sourceNetworkId], total
				}
				wantMirror := ByteCount(999)
				if apply {
					wantMirror = 40
				}
				if count != 1 || drift != 959 || Testing_NetEscrowByteCount(ctx, f.balanceId) != wantMirror {
					t.Fatalf("operator trusted corrupt cache: network=%t apply=%t count=%d drift=%d", network, apply, count, drift)
				}
			}
		}
	})
}

// A page can outlive a balance deletion. The existing tombstone fallback must
// publish zero at its newer revision and reject a delayed pre-deletion page.
func TestNetEscrowReconcileCacheDeletedCapturedBalance(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		createNetEscrowOrderingTestContract(ctx, f, 17)
		ids := []server.Id{f.balanceId}
		before, deferred := readReconcileNetEscrowSnapshots(ctx, ids, true)
		if len(deferred) != 0 {
			t.Fatal("a one-contract balance exceeded the scheduled census bound")
		}
		reconcileNetEscrowBatch(ctx, before, ids, true)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM transfer_balance WHERE balance_id=$1`, f.balanceId))
		})
		after, deferred := readReconcileNetEscrowSnapshots(ctx, ids, true)
		if len(deferred) != 0 || len(after) != 1 || after[f.balanceId].reserved != 0 || after[f.balanceId].endTime != nil || after[f.balanceId].revision <= before[f.balanceId].revision {
			t.Fatal("deleted captured balance reused its abandoned cache amount")
		}
		reconcileNetEscrowBatch(ctx, after, ids, true)
		reconcileNetEscrowBatch(ctx, before, ids, true)
		if Testing_NetEscrowByteCount(ctx, f.balanceId) != 0 {
			t.Fatal("delayed captured page resurrected a deleted reservation")
		}
	})
}
