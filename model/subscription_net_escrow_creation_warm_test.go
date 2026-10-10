package model

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/urnetwork/server"
)

// Delayed callbacks run in a fixed order after a completed cache deletion.
// The first old callback must warm its exact committed amount; every later
// callback can then avoid history, independent of goroutine scheduling.
func TestNetEscrowCreationColdPostWarmsCurrentHistoryOnce(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
		defer cancel()
		for _, historyCount := range []int{1, 4096, 65536} {
			f := seedAdmissionCacheHistory(t, ctx, historyCount, 20)
			var delayedPosts []server.PostFunction
			for range 20 {
				_, posts := createNetEscrowOrderingTestContract(ctx, f, 1)
				delayedPosts = append(delayedPosts, posts...)
			}
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `DELETE FROM transfer_balance_net_escrow_snapshot WHERE balance_id=$1`, f.balanceId))
			})
			beforeReload := testutil.ToFloat64(netEscrowCreationSnapshots.WithLabelValues("reloaded"))
			beforeReuse := testutil.ToFloat64(netEscrowCreationSnapshots.WithLabelValues("reused"))
			for _, post := range delayedPosts {
				server.RunPosts(ctx, post)
			}
			reloads := testutil.ToFloat64(netEscrowCreationSnapshots.WithLabelValues("reloaded")) - beforeReload
			reused := testutil.ToFloat64(netEscrowCreationSnapshots.WithLabelValues("reused")) - beforeReuse
			if reloads != 1 || reused != 19 {
				t.Fatalf("history=%d sequential creation callbacks reloaded=%v reused=%v, want1/19", historyCount, reloads, reused)
			}
			want := ByteCount(historyCount + 20)
			ids := []server.Id{f.balanceId}
			exact := openEscrowReservedForBalances(ctx, ids)[f.balanceId]
			cached, present := settlementCacheSnapshot(ctx, ids)[f.balanceId]
			if !present || cached.revision != exact.revision || cached.reserved != want || exact.reserved != want {
				t.Fatal("creation callback did not preserve the exact committed cache revision and amount")
			}
			if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != want {
				t.Fatalf("history=%d Redis reservation=%d want%d", historyCount, got, want)
			}
			if got, reads := lockedAdmissionCacheTestRead(ctx, f); reads != 0 || got.reserved != want || got.revision != exact.revision {
				t.Fatalf("history=%d locked admission reloaded=%d reserved=%d", historyCount, reads, got.reserved)
			}
			// Keep the real server transaction visible to the ownership guard.
			// A PgTx counting wrapper cannot borrow that concrete authority.
			beforeFundingReload := testutil.ToFloat64(netEscrowAdmissionSnapshots.WithLabelValues("reloaded"))
			beforeFundingReuse := testutil.ToFloat64(netEscrowAdmissionSnapshots.WithLabelValues("reused"))
			var rejected *TransferEscrow
			var admissionErr error
			server.Tx(ctx, func(tx server.PgTx) {
				rejected, _, admissionErr = createTransferEscrowInTx(ctx, tx,
					f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId,
					f.sourceNetworkId, 1, nil)
			}, server.TxReadCommitted, server.OptNoRetry())
			fundingReloads := testutil.ToFloat64(netEscrowAdmissionSnapshots.WithLabelValues("reloaded")) - beforeFundingReload
			fundingReuses := testutil.ToFloat64(netEscrowAdmissionSnapshots.WithLabelValues("reused")) - beforeFundingReuse
			insufficient := admissionErr != nil && strings.Contains(admissionErr.Error(), "Insufficient balance")
			if rejected != nil || admissionErr == nil || !insufficient || fundingReloads != 0 || fundingReuses != 1 {
				t.Fatalf("exhausted admission contract_present=%t error_present=%t error_type=%T insufficient=%t history_reloads=%v cache_reuses=%v want=false/true/true/0/1",
					rejected != nil, admissionErr != nil, admissionErr, insufficient, fundingReloads, fundingReuses)
			}

			// An older writer still invalidates the warmed result. One later
			// callback reloads the new committed amount and warms that revision.
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_escrow SET balance_byte_count=7
					WHERE balance_id=$1 AND contract_id=md5($1::uuid::text||'-cache-contract-1')::uuid`, f.balanceId))
			})
			beforeReload = testutil.ToFloat64(netEscrowCreationSnapshots.WithLabelValues("reloaded"))
			for _, post := range delayedPosts {
				server.RunPosts(ctx, post)
			}
			if delta := testutil.ToFloat64(netEscrowCreationSnapshots.WithLabelValues("reloaded")) - beforeReload; delta != 1 {
				t.Fatalf("history=%d legacy invalidation reloads=%v want1", historyCount, delta)
			}
			want += 6
			if got, reads := lockedAdmissionCacheTestRead(ctx, f); reads != 0 || got.reserved != want {
				t.Fatalf("history=%d updated admission reloaded=%d reserved=%d want%d", historyCount, reads, got.reserved, want)
			}
			if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != want {
				t.Fatal("legacy invalidation did not reach the fenced Redis mirror")
			}
			t.Logf("history=%d sequential_callbacks=20 first_census=1 warm_reuse=19 legacy_invalidation_census=1 admission_history_reads=0", historyCount)
		}
	})
}

// Optional warming must skip an independently held grant owner. The callback
// still publishes the committed amount to Redis; a later callback can warm it
// once that owner releases. The holder's acknowledged channel fixes ordering.
func TestNetEscrowCreationColdPostSkipsBusyCacheOwner(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		f := seedAdmissionCacheHistory(t, ctx, 4096, 8)
		_, delayedPosts := createNetEscrowOrderingTestContract(ctx, f, 1)
		_, _ = createNetEscrowOrderingTestContract(ctx, f, 1)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM transfer_balance_net_escrow_snapshot WHERE balance_id=$1`, f.balanceId))
		})
		holderCtx, holderCancel := context.WithTimeout(ctx, 10*time.Second)
		defer holderCancel()
		ready := make(chan struct{})
		release := make(chan struct{})
		done := make(chan struct{})
		failures := make(chan any, 1)
		var releaseOnce sync.Once
		go func() {
			defer close(done)
			defer func() {
				if failure := recover(); failure != nil {
					failures <- failure
				}
			}()
			server.Tx(holderCtx, func(tx server.PgTx) {
				admitted, err := tryTransferBalanceOwnershipInTx(holderCtx, tx, []server.Id{f.balanceId})
				server.Raise(err)
				if !admitted {
					panic("synthetic grant holder was not admitted")
				}
				close(ready)
				select {
				case <-release:
				case <-holderCtx.Done():
					panic(holderCtx.Err())
				}
			}, server.TxReadCommitted, server.OptNoRetry())
		}()
		defer func() {
			releaseOnce.Do(func() { close(release) })
			<-done
		}()
		select {
		case <-ready:
		case <-done:
			t.Fatal("synthetic grant holder ended before acknowledgement")
		}
		before := testutil.ToFloat64(netEscrowCreationSnapshots.WithLabelValues("reloaded"))
		server.RunPosts(ctx, delayedPosts...)
		select {
		case <-done:
			t.Fatal("callback did not finish while the independent grant owner remained held")
		default:
		}
		if delta := testutil.ToFloat64(netEscrowCreationSnapshots.WithLabelValues("reloaded")) - before; delta != 1 {
			t.Fatalf("busy owner callback completed exact reloads=%v want1", delta)
		}
		if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != 4098 {
			t.Fatal("busy optional cache owner prevented exact Redis publication")
		}
		if len(settlementCacheSnapshot(ctx, []server.Id{f.balanceId})) != 0 {
			t.Fatal("callback published the optional cache without grant ownership")
		}
		releaseOnce.Do(func() { close(release) })
		<-done
		select {
		case failure := <-failures:
			t.Fatalf("synthetic holder failed: %v", failure)
		default:
		}
		server.RunPosts(ctx, delayedPosts...)
		if got, reads := lockedAdmissionCacheTestRead(ctx, f); reads != 0 || got.reserved != 4098 {
			t.Fatal("callback did not warm the cache after the owner released")
		}
	})
}

// Construct the same mixed reservation states directly. Bulk outcome UPDATEs
// test transition-trigger work rather than the two read plans below. All
// ordinary insert guards/revision triggers remain enabled, and real admissions
// still create the two live contracts used by the callback control.
func seedCreationWarmMixedHistory(t testing.TB, ctx context.Context, count int) netEscrowOrderingTestFixture {
	t.Helper()
	f := newNetEscrowOrderingTestFixture(t, ctx)
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET start_balance_byte_count=$2,balance_byte_count=$2 WHERE balance_id=$1`, f.balanceId, count+8))
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract
			(contract_id,source_network_id,source_id,destination_network_id,destination_id,payer_network_id,transfer_byte_count,outcome,close_time)
			SELECT md5($5::uuid::text||'-cache-contract-'||n)::uuid,$1,$2,$3,$4,$1,1,
				CASE WHEN n%4=0 THEN 'canceled' ELSE NULL END,
				CASE WHEN n%4=0 THEN now() AT TIME ZONE 'UTC' ELSE NULL END
			FROM generate_series(1,$6)n`, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId, f.balanceId, count))
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_escrow(contract_id,balance_id,balance_byte_count,redis_reserved)
			SELECT md5($1::uuid::text||'-cache-contract-'||n)::uuid,$1,
				CASE WHEN n%4=1 THEN 0 ELSE 1 END,n%4=2
			FROM generate_series(1,$2)n`, f.balanceId, count))
		var terminal, zero, redis, open int
		server.Raise(tx.QueryRow(ctx, `SELECT
			count(*) FILTER(WHERE contract.outcome='canceled' AND contract.close_time IS NOT NULL AND NOT escrow.settled AND escrow.balance_byte_count=1 AND NOT escrow.redis_reserved),
			count(*) FILTER(WHERE contract.outcome IS NULL AND NOT escrow.settled AND escrow.balance_byte_count=0 AND NOT escrow.redis_reserved),
			count(*) FILTER(WHERE contract.outcome IS NULL AND NOT escrow.settled AND escrow.balance_byte_count=1 AND escrow.redis_reserved),
			count(*) FILTER(WHERE contract.outcome IS NULL AND NOT escrow.settled AND escrow.balance_byte_count=1 AND NOT escrow.redis_reserved)
			FROM transfer_escrow AS escrow JOIN transfer_contract AS contract USING(contract_id)
			WHERE escrow.balance_id=$1`, f.balanceId).Scan(&terminal, &zero, &redis, &open))
		if terminal != count/4 || zero != count/4 || redis != count/4 || open != count/4 {
			t.Fatalf("mixed history geometry terminal=%d zero=%d redis=%d open=%d want_each=%d", terminal, zero, redis, open, count/4)
		}
	})
	return f
}

// Requested-balance bounds do not bound history beneath a hot balance. Measure
// the unchanged exact census and the warm callback's four metadata points at
// increasing history sizes, without planner coercion or a wall-clock gate.
func TestNetEscrowCreationHotHistoryCurrentAndWarmPlans(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
		defer cancel()
		for _, historyCount := range []int{16, 4096, 65536} {
			f := seedCreationWarmMixedHistory(t, ctx, historyCount)
			first, posts := createNetEscrowOrderingTestContract(ctx, f, 1)
			_, _ = createNetEscrowOrderingTestContract(ctx, f, 1)
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `DELETE FROM transfer_balance_net_escrow_snapshot WHERE balance_id=$1`, f.balanceId))
			})
			server.RunPosts(ctx, posts...)
			want := ByteCount(historyCount/4 + 2)
			if exact := openEscrowReservedForBalances(ctx, []server.Id{f.balanceId})[f.balanceId]; exact.reserved != want {
				t.Fatal("mixed hot-history exact census changed terminal/zero/Redis semantics")
			}
			server.Db(ctx, func(conn server.PgConn) {
				server.RaisePgResult(conn.Exec(ctx, `ANALYZE transfer_escrow; ANALYZE transfer_contract;
					ANALYZE transfer_balance; ANALYZE transfer_balance_net_escrow_revision;
					ANALYZE transfer_balance_net_escrow_snapshot`))
				for _, mode := range []string{"force_custom_plan", "force_generic_plan"} {
					func() {
						tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadOnly})
						server.Raise(err)
						defer tx.Rollback(context.WithoutCancel(ctx))
						configureNetEscrowReservationPageTimeout(ctx, tx, netEscrowReservationPageStatementTimeout)
						server.RaisePgResult(tx.Exec(ctx, `SELECT set_config('plan_cache_mode',$1,true)`, mode))
						for _, warm := range []bool{false, true} {
							query := netEscrowReservationPageSQL
							parameters := "(uuid[])"
							argument := fmt.Sprintf("('{%s}'::uuid[])", f.balanceId)
							if warm {
								query = netEscrowCreatedSnapshotSQL
								parameters = "(uuid[],uuid)"
								argument = fmt.Sprintf("('{%s}'::uuid[],'%s'::uuid)", f.balanceId, first.ContractId)
							}
							server.RaisePgResult(tx.Exec(ctx, "PREPARE creation_hot_history"+parameters+" AS "+query))
							var raw []byte
							server.Raise(tx.QueryRow(ctx, "EXPLAIN (ANALYZE,BUFFERS,TIMING OFF,FORMAT JSON) EXECUTE creation_hot_history"+argument).Scan(&raw))
							t.Logf("history=%d mode=%s warm=%t plan=%s", historyCount, mode, warm, raw)
							server.RaisePgResult(tx.Exec(ctx, "DEALLOCATE creation_hot_history"))
							var plans []censusBoundsPlan
							server.Raise(json.Unmarshal(raw, &plans))
							if len(plans) != 1 || plans[0].Plan.Rows != 1 || plans[0].Plan.Loops != 1 {
								t.Fatal("hot-history plan did not return one requested balance")
							}
							escrowNodes, contractLoops, visitedRows := 0, float64(0), float64(0)
							var visit func(censusBoundsPlanNode)
							visit = func(n censusBoundsPlanNode) {
								if n.Relation == "transfer_escrow" {
									escrowNodes++
									visitedRows += (n.Rows + n.Removed) * n.Loops
								}
								if n.Relation == "transfer_contract" {
									contractLoops += n.Loops
								}
								for _, child := range n.Plans {
									visit(child)
								}
							}
							visit(plans[0].Plan)
							if warm && (escrowNodes != 0 || contractLoops > 1) {
								t.Fatal("warm creation callback planned history access")
							}
							if !warm && (escrowNodes == 0 || contractLoops < float64(historyCount/2+2)) {
								t.Fatal("current census control did not exercise all positive legacy history")
							}
							t.Logf("history=%d mode=%s warm=%t escrow_nodes=%d visited_escrow_rows=%.0f contract_probes=%.0f buffers=%.0f execution_ms=%.3f",
								historyCount, mode, warm, escrowNodes, visitedRows, contractLoops,
								plans[0].Plan.Hit+plans[0].Plan.Read, plans[0].ExecutionTime)
						}
					}()
				}
			})
		}
	})
}
