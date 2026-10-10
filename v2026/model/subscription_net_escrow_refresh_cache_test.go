package model

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/urnetwork/server/v2026"
)

// A later admission can satisfy a delayed settlement mirror at its exact current revision.
func TestNetEscrowDelayedSettlementUsesCurrentDurableCacheAndAvoidsHistory(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		f := seedAdmissionCacheHistory(t, ctx, 10001, 8)
		contract, _ := createNetEscrowOrderingTestContract(ctx, f, 1)
		delayedPosts := settleNetEscrowOrderingTestContract(ctx, contract.ContractId)
		if len(delayedPosts) != 2 {
			t.Fatalf("zero-use settlement posts=%d, want metadata and mirror", len(delayedPosts))
		}
		server.RunPosts(ctx, delayedPosts[0])
		_, _ = createNetEscrowOrderingTestContract(ctx, f, 1)
		var exactCache bool
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `
				SELECT cached.revision=revision.revision AND cached.reserved_byte_count=10002
				FROM transfer_balance_net_escrow_snapshot cached
				JOIN transfer_balance_net_escrow_revision revision USING(balance_id)
				WHERE balance_id=$1`, f.balanceId).Scan(&exactCache))
		})
		if !exactCache {
			t.Fatal("later admission did not commit a current exact cache snapshot")
		}

		blocker := acquireContractLifecycleTestConnection(t, ctx)
		defer blocker.Release()
		held, err := blocker.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer held.Rollback(context.Background())
		server.RaisePgResult(held.Exec(ctx, `LOCK TABLE transfer_escrow IN ACCESS EXCLUSIVE MODE`))
		done := make(chan struct{})
		go func() {
			defer close(done)
			server.RunPosts(ctx, delayedPosts[1])
		}()

		observedHistoryWait := false
		finished := false
		limit := time.NewTimer(3 * time.Second)
		defer limit.Stop()
		tick := time.NewTicker(20 * time.Millisecond)
		defer tick.Stop()
	observe:
		for {
			select {
			case <-done:
				finished = true
				break observe
			case <-limit.C:
				break observe
			case <-tick.C:
				server.Db(ctx, func(conn server.PgConn) {
					server.Raise(conn.QueryRow(ctx, `
						SELECT EXISTS (
							SELECT 1 FROM pg_locks AS held
							JOIN pg_stat_activity AS activity ON activity.pid=held.pid
							WHERE held.relation='transfer_escrow'::regclass AND NOT held.granted
								AND activity.datname=current_database()
								AND activity.state='active' AND activity.wait_event='relation'
								AND position('SELECT requested_balance.balance_id,' IN activity.query)>0
						)`).Scan(&observedHistoryWait))
				})
				if observedHistoryWait {
					break observe
				}
			}
		}
		server.Raise(held.Rollback(ctx))
		select {
		case <-done:
		case <-ctx.Done():
			t.Fatal("delayed mirror did not join after releasing the history lock")
		}
		if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != 10002 {
			t.Fatalf("delayed mirror reserved %d, want current committed 10002", got)
		}
		t.Logf("exact_cache_current=%t history_relation_wait=%t mirror_finished_while_history_held=%t", exactCache, observedHistoryWait, finished)
		if observedHistoryWait || !finished {
			t.Fatal("delayed settlement mirror revisited historical escrow despite a current durable cache snapshot")
		}
	})
}

// Twenty delayed settlement mirrors share 10,001 surviving reservations. A
// missing cache is the exact-history control; overtaking admission supplies the
// current committed cached amount. Every callback reads exactly one balance.
func TestNetEscrowRefreshCacheConcurrentMirrorCost(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
		defer cancel()
		for _, missing := range []bool{true, false} {
			f := seedAdmissionCacheHistory(t, ctx, 10001, 100)
			mirrors := []func() any{}
			for range 20 {
				contract, _ := createNetEscrowOrderingTestContract(ctx, f, 1)
				posts := settleNetEscrowOrderingTestContract(ctx, contract.ContractId)
				if len(posts) != 2 {
					t.Fatalf("zero-use settlement posts=%d, want metadata and mirror", len(posts))
				}
				server.RunPosts(ctx, posts[0])
				mirrors = append(mirrors, posts[1])
			}
			_, _ = createNetEscrowOrderingTestContract(ctx, f, 1)
			if missing {
				server.Tx(ctx, func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(ctx, `DELETE FROM transfer_balance_net_escrow_snapshot WHERE balance_id=$1`, f.balanceId))
				})
			}
			beforeReload := testutil.ToFloat64(netEscrowRefreshSnapshots.WithLabelValues("reloaded"))
			beforeReuse := testutil.ToFloat64(netEscrowRefreshSnapshots.WithLabelValues("reused"))
			var wg sync.WaitGroup
			start := make(chan struct{})
			began := time.Now()
			for _, mirror := range mirrors {
				wg.Add(1)
				go func() { defer wg.Done(); <-start; server.RunPosts(ctx, mirror) }()
			}
			close(start)
			wg.Wait()
			reloads := testutil.ToFloat64(netEscrowRefreshSnapshots.WithLabelValues("reloaded")) - beforeReload
			reused := testutil.ToFloat64(netEscrowRefreshSnapshots.WithLabelValues("reused")) - beforeReuse
			// Concurrent cold readers may all start before the first guarded
			// warmup commits; later readers may reuse it. Both schedules retain
			// exactly twenty complete reads, and a warm start requires no census.
			if reused+reloads != 20 || (!missing && reloads != 0) || (missing && (reloads < 1 || reloads > 20)) {
				t.Fatalf("missing=%t completed reloads=%v reused=%v", missing, reloads, reused)
			}
			if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != 10002 {
				t.Fatalf("concurrent delayed settlement mirrors published%d, want10002", got)
			}
			t.Logf("missing_cache=%t completed_one_balance_mirrors=20 exact_history_reads=%v elapsed=%s", missing, reloads, time.Since(began))
		}
	})
}

// A matching committed revision remains the only reuse authority. Legacy
// writes, outcomes and deletions fall back; uncommitted mutations cannot escape,
// and an older mirror cannot replace a newer one.
func TestNetEscrowRefreshCacheInvalidationRollbackAndOrdering(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
		defer cancel()
		for _, change := range []string{"missing", "legacy", "terminal", "deleted", "rollback"} {
			f := newNetEscrowOrderingTestFixture(t, ctx)
			contract, _ := createNetEscrowOrderingTestContract(ctx, f, 17)
			ids := []server.Id{f.balanceId}
			stale := readMirrorNetEscrowSnapshots(ctx, ids)
			want := ByteCount(17)
			reloads := float64(1)
			if change == "rollback" {
				conn := acquireContractLifecycleTestConnection(t, ctx)
				tx, err := conn.Begin(ctx)
				server.Raise(err)
				server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_escrow SET balance_byte_count=23 WHERE contract_id=$1`, contract.ContractId))
				got := readMirrorNetEscrowSnapshots(ctx, ids)[f.balanceId]
				server.Raise(tx.Rollback(ctx))
				conn.Release()
				if got.reserved != 17 || got.revision != stale[f.balanceId].revision {
					t.Fatal("uncommitted reservation escaped current snapshot read")
				}
				reloads = 0
			} else {
				server.Tx(ctx, func(tx server.PgTx) {
					switch change {
					case "missing":
						server.RaisePgResult(tx.Exec(ctx, `DELETE FROM transfer_balance_net_escrow_snapshot WHERE balance_id=$1`, f.balanceId))
					case "legacy":
						server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_escrow SET balance_byte_count=23 WHERE contract_id=$1`, contract.ContractId))
						want = 23
					case "terminal":
						server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET outcome='canceled',close_time=now() WHERE contract_id=$1`, contract.ContractId))
						want = 0
					case "deleted":
						server.RaisePgResult(tx.Exec(ctx, `DELETE FROM transfer_balance WHERE balance_id=$1`, f.balanceId))
						want = 0
					}
				})
			}
			before := testutil.ToFloat64(netEscrowRefreshSnapshots.WithLabelValues("reloaded"))
			refreshNetEscrow(ctx, ids)
			if delta := testutil.ToFloat64(netEscrowRefreshSnapshots.WithLabelValues("reloaded")) - before; delta != reloads {
				t.Fatalf("%s exact reads=%v, want%v", change, delta, reloads)
			}
			reconcileNetEscrowBatch(ctx, stale, ids, true)
			if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != want {
				t.Fatalf("%s reservation after older publication=%d, want%d", change, got, want)
			}
			releaseNetEscrowForContract(ctx, contract.ContractId)
			if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != want {
				t.Fatalf("%s quarantine refresh=%d, want%d", change, got, want)
			}
		}
	})
}
