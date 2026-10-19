// A cold historical census cannot own the next financial visit in a due page.
package model

import (
	"context"
	"runtime"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/task"
)

const legacyColdPageMirrorFunction = "github.com/urnetwork/server/v2026/model.ApplyLegacyNetEscrowMirror"

type legacyColdPageResult struct {
	page LegacySettlementFlushResult
	err  error
}

// The sole fixture interception is an updatable view over the unchanged escrow
// table. Its volatile predicate waits only in the exact census query; every
// ordinary ownership point read and financial write still reaches the table.
// No production hook, replacement settlement, sleep, or short timeout is used.
func installLegacyColdPageCensusBarrier(t testing.TB, ctx context.Context) func() {
	t.Helper()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `
            ALTER TABLE transfer_escrow RENAME TO synthetic_cold_page_escrow;
            CREATE FUNCTION synthetic_cold_page_census_gate() RETURNS boolean
            LANGUAGE plpgsql VOLATILE AS $$
            BEGIN
                IF position('SELECT requested_balance.balance_id,' IN current_query()) > 0 THEN
                    PERFORM pg_advisory_xact_lock(731031);
                END IF;
                RETURN true;
            END $$;
            CREATE VIEW transfer_escrow AS
                SELECT * FROM synthetic_cold_page_escrow
                WHERE synthetic_cold_page_census_gate()`))
	}, server.TxReadCommitted, server.OptNoRetry())
	return func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		server.Tx(cleanup, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(cleanup, `
                DROP VIEW transfer_escrow;
                ALTER TABLE synthetic_cold_page_escrow RENAME TO transfer_escrow;
                DROP FUNCTION synthetic_cold_page_census_gate()`))
		}, server.TxReadCommitted, server.OptNoRetry())
	}
}

// Explicit native lock state, rather than absence for an elapsed interval,
// establishes the baseline's stop at the census. On the successor the page
// result is the positive proof; the held lock remains a live obstacle.
func legacyColdPageWait(t testing.TB, ctx context.Context, observer server.PgTx,
	blocker int32, done <-chan legacyColdPageResult) (legacyColdPageResult, bool) {
	t.Helper()
	for {
		select {
		case result := <-done:
			return result, false
		default:
		}
		var blocked bool
		server.Raise(observer.QueryRow(ctx, `SELECT EXISTS (
            SELECT 1 FROM pg_locks WHERE locktype='advisory' AND NOT granted
            AND classid=0 AND objid=731031 AND objsubid=1
            AND $1::int=ANY(pg_blocking_pids(pid)))`, blocker).Scan(&blocked))
		if blocked {
			return legacyColdPageResult{}, true
		}
		select {
		case <-ctx.Done():
			t.Fatalf("neither page completion nor cold census lock was observed: %v", ctx.Err())
		default:
			runtime.Gosched()
		}
	}
}

// The baseline wrapper supplies nil; it must fail at the observed first census
// before a new mirror target is needed. The successor wrapper supplies the real
// registered target and its ordinary finalization hook, without replacing them.
func testLegacySettlementDensePageDoesNotJoinColdMirror(t *testing.T, mirrorTarget task.Target) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
		defer cancel()
		head, headId, tail, tailIds := legacyHeadRevisitFixture(t, ctx, 128)
		ids := append([]server.Id{headId}, tailIds...)
		balances := []server.Id{head.balanceId, tail.balanceId}
		fixtures := []netEscrowOrderingTestFixture{head, tail}
		legacyNeighbors := make([]server.Id, 2)
		redisNeighbors := make([]server.Id, 2)
		redisExpiry := make([]float64, 2)
		for index, f := range fixtures {
			legacy, posts := createNetEscrowOrderingTestContract(ctx, f, 23)
			server.RunPosts(ctx, posts...)
			legacyNeighbors[index] = legacy.ContractId
			redisNeighbors[index] = createRedisAdmissionTest(ctx, f, 31).ContractId
			server.Redis(ctx, func(r server.RedisClient) {
				var err error
				redisExpiry[index], err = r.ZScore(ctx, redisContractReservationKeys(f.balanceId)[2], redisNeighbors[index].String()).Result()
				server.Raise(err)
			})
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET net_revenue_nano_cents=2*start_balance_byte_count WHERE balance_id=ANY($1)`, balances))
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM transfer_balance_net_escrow_snapshot WHERE balance_id=ANY($1)`, balances))
		})
		if len(settlementCacheSnapshot(ctx, balances)) != 0 {
			t.Fatal("fixture did not make both independent grants cold")
		}
		restoreEscrow := installLegacyColdPageCensusBarrier(t, ctx)
		defer restoreEscrow()
		conn := acquireContractLifecycleTestConnection(t, ctx)
		defer conn.Release()
		held, err := conn.Begin(ctx)
		server.Raise(err)
		defer held.Rollback(context.Background())
		server.RaisePgResult(held.Exec(ctx, `SELECT pg_advisory_xact_lock(731031)`))
		blocker := contractLifecycleTestBackendPid(t, ctx, held)
		bounded, cancelPage := context.WithCancelCause(ctx)
		defer cancelPage(context.Canceled)
		firstDone := make(chan legacyColdPageResult, 1)
		shard := int(headId[15]) % LegacySettlementShardCount
		go func() {
			page, err := flushLegacySettlementsPage(ctx, bounded, shard, nil, 64, flushLegacySettlementWithGrantWait)
			firstDone <- legacyColdPageResult{page: page, err: err}
		}()
		first, blocked := legacyColdPageWait(t, ctx, held, blocker, firstDone)
		if blocked {
			var firstCommitted, nextUntouched bool
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT
                    (SELECT outcome='settled' FROM transfer_contract WHERE contract_id=$1)
                        AND NOT EXISTS(SELECT 1 FROM legacy_settlement_intent WHERE contract_id=$1)
                        AND (SELECT balance_byte_count=989 FROM transfer_balance WHERE balance_id=$3),
                    (SELECT outcome IS NULL FROM transfer_contract WHERE contract_id=$2)
                        AND EXISTS(SELECT 1 FROM legacy_settlement_intent WHERE contract_id=$2)
                        AND (SELECT balance_byte_count=1000000 FROM transfer_balance WHERE balance_id=$4)`,
					headId, tailIds[0], head.balanceId, tail.balanceId).Scan(&firstCommitted, &nextUntouched))
			})
			cancelPage(errLegacySettlementPageBudget)
			server.Raise(held.Rollback(ctx))
			select {
			case first = <-firstDone:
			case <-ctx.Done():
				t.Fatalf("baseline census release did not join: %v", ctx.Err())
			}
			t.Fatalf("cold census held the page after its first financial commit: first_committed=%t next_independent_untouched=%t page=%+v err=%v",
				firstCommitted, nextUntouched, first.page, first.err)
		}
		if first.err != nil || first.page.Visited != 64 || first.page.Completed != 64 || first.page.BusyOrGone != 0 || first.page.Failed != 0 || first.page.Cursor == nil {
			t.Fatalf("dense first page failed with census barrier held: %+v err=%v", first.page, first.err)
		}
		pages := []LegacySettlementFlushResult{first.page}
		cursor := first.page.Cursor
		for cursor != nil && len(pages) < 4 {
			page, err := flushLegacySettlementsPage(ctx, bounded, shard, cursor, 64, flushLegacySettlementWithGrantWait)
			if err != nil || page.BusyOrGone != 0 || page.Failed != 0 || page.Completed != page.Visited {
				t.Fatalf("continued dense page failed with census barrier held: %+v err=%v", page, err)
			}
			pages = append(pages, page)
			cursor = page.Cursor
		}
		if len(pages) != 3 || pages[1].Completed != 64 || pages[2].Completed != 1 || cursor != nil {
			t.Fatalf("finite dense cohort did not finish in three exact pages: %+v", pages)
		}
		if len(settlementCacheSnapshot(ctx, balances)) != 0 {
			t.Fatal("foreground invented a cold cache authority without census")
		}
		assertFinancial := func() {
			t.Helper()
			for index, f := range fixtures {
				selected := []server.Id{headId}
				start := int64(1000)
				if index == 1 {
					selected, start = tailIds, 1000000
				}
				want := int64(len(selected)) * 11
				server.Db(ctx, func(conn server.PgConn) {
					var terminal, pending, metadata int
					var credit, swept, revenue, provided, providedRevenue, queued, queuedRevenue int64
					server.Raise(conn.QueryRow(ctx, `WITH unapplied AS (
                        SELECT allocation FROM pending_task
                        CROSS JOIN LATERAL jsonb_array_elements(args_json::jsonb->'totals') AS allocation
                        WHERE function_name=$4 AND (args_json::jsonb->>'applied')::boolean=false
                            AND (allocation->>'network_id')::uuid=$3
                    ) SELECT
                        (SELECT count(*) FROM transfer_contract WHERE contract_id=ANY($1) AND outcome='settled'),
                        (SELECT count(*) FROM legacy_settlement_intent WHERE contract_id=ANY($1)),
                        (SELECT count(*) FROM transfer_escrow WHERE contract_id=ANY($1) AND settled),
                        (SELECT balance_byte_count FROM transfer_balance WHERE balance_id=$2),
                        (SELECT COALESCE(sum(payout_byte_count),0) FROM transfer_escrow_sweep WHERE contract_id=ANY($1)),
                        (SELECT COALESCE(sum(payout_net_revenue_nano_cents),0) FROM transfer_escrow_sweep WHERE contract_id=ANY($1)),
                        COALESCE((SELECT provided_byte_count FROM account_balance WHERE network_id=$3),0),
                        COALESCE((SELECT provided_net_revenue_nano_cents FROM account_balance WHERE network_id=$3),0),
                        COALESCE((SELECT sum((allocation->>'bytes')::bigint) FROM unapplied),0),
                        COALESCE((SELECT sum((allocation->>'revenue')::bigint) FROM unapplied),0)`,
						selected, f.balanceId, f.destinationNetworkId, task.NewTaskTarget(ApplyLegacyProviderTotals).TargetFunctionName()).Scan(
						&terminal, &pending, &metadata, &credit, &swept, &revenue, &provided, &providedRevenue, &queued, &queuedRevenue))
					if terminal != len(selected) || pending != 0 || metadata != len(selected) || credit != start-want || swept != want || revenue != want || provided+queued != want || providedRevenue+queuedRevenue != want {
						t.Fatalf("dense financial conservation failed: terminal=%d pending=%d metadata=%d credit=%d swept=%d revenue=%d provider=%d+%d/%d+%d want=%d",
							terminal, pending, metadata, credit, swept, revenue, provided, queued, providedRevenue, queuedRevenue, want)
					}
					var neighbors bool
					server.Raise(conn.QueryRow(ctx, `SELECT
                        (SELECT outcome IS NULL FROM transfer_contract WHERE contract_id=$1)
                        AND (SELECT NOT settled AND NOT redis_reserved AND balance_byte_count=23 FROM transfer_escrow WHERE contract_id=$1 AND balance_id=$3)
                        AND (SELECT outcome IS NULL FROM transfer_contract WHERE contract_id=$2)
                        AND (SELECT NOT settled AND redis_reserved AND balance_byte_count=31 FROM transfer_escrow WHERE contract_id=$2 AND balance_id=$3)`,
						legacyNeighbors[index], redisNeighbors[index], f.balanceId).Scan(&neighbors))
					if !neighbors {
						t.Fatal("settlement or repair changed a surviving neighbor")
					}
				})
			}
		}
		assertFinancial()
		ownerIds := []server.Id{}
		server.Db(ctx, func(conn server.PgConn) {
			rows, err := conn.Query(ctx, `SELECT task_id FROM pending_task WHERE function_name=$1 ORDER BY task_id`, legacyColdPageMirrorFunction)
			server.WithPgResult(rows, err, func() {
				for rows.Next() {
					var id server.Id
					server.Raise(rows.Scan(&id))
					ownerIds = append(ownerIds, id)
				}
			})
			var exact bool
			server.Raise(conn.QueryRow(ctx, `SELECT count(*)=2 AND count(DISTINCT args_json::jsonb->>'balance_id')=2
                AND bool_and((args_json::jsonb->>'balance_id')::uuid=ANY($2))
                FROM pending_task WHERE function_name=$1`, legacyColdPageMirrorFunction, balances).Scan(&exact))
			if !exact || len(ownerIds) != 2 {
				t.Fatal("129 settlements did not coalesce to exactly two balance owners")
			}
		})
		if mirrorTarget == nil || mirrorTarget.TargetFunctionName() != legacyColdPageMirrorFunction {
			t.Fatal("successor must supply its real mirror owner and finalization target")
		}
		settings := task.DefaultTaskWorkerSettings()
		settings.ClaimRegisteredTargetsOnly = true
		worker := task.NewTaskWorker(ctx, settings)
		defer worker.Close()
		worker.AddTargets(mirrorTarget)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET run_at=$1,release_time=$1 WHERE task_id=ANY($2)`, time.Time{}, ownerIds))
		})
		type ownerResult struct {
			finished, retried, postRetried []server.Id
			err                            error
		}
		ownerDone := make(chan ownerResult, 1)
		go func() {
			finished, retried, postRetried, err := worker.EvalTasks(2)
			ownerDone <- ownerResult{finished, retried, postRetried, err}
		}()
		// This is the positive control for the same barrier avoided above:
		// the durable owner really reaches census before release.
		requireContractLifecycleBlockedBy(t, ctx, held, blocker)
		assertFinancial()
		server.Raise(held.Rollback(ctx))
		var owners ownerResult
		select {
		case owners = <-ownerDone:
		case <-ctx.Done():
			t.Fatalf("released durable mirror owner did not finish: %v", ctx.Err())
		}
		if owners.err != nil || len(owners.finished) != 2 || len(owners.retried) != 0 || len(owners.postRetried) != 0 {
			t.Fatalf("durable mirror repair did not finish its finite owners: %+v", owners)
		}
		assertMirrors := func() {
			t.Helper()
			for index, f := range fixtures {
				if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != 54 {
					t.Fatalf("mirror lost surviving legacy23 + Redis31 neighbors: %d", got)
				}
				server.Redis(ctx, func(r server.RedisClient) {
					keys := redisContractReservationKeys(f.balanceId)
					amount, err := r.HGet(ctx, keys[1], redisNeighbors[index].String()).Int64()
					server.Raise(err)
					expiry, err := r.ZScore(ctx, keys[2], redisNeighbors[index].String()).Result()
					server.Raise(err)
					if amount != 31 || expiry != redisExpiry[index] {
						t.Fatal("legacy mirror repair changed native reservation amount or lease")
					}
				})
			}
		}
		assertMirrors()
		for _, id := range ids {
			completed, busy, _, err := flushLegacySettlement(ctx, id)
			if err != nil || completed || !busy {
				t.Fatal("dense completed intent replay reclaimed financial ownership")
			}
		}
		projectLegacyProviderTotalsForTest(t, ctx)
		assertFinancial()
		assertMirrors()
		server.Db(ctx, func(conn server.PgConn) {
			var exact bool
			server.Raise(conn.QueryRow(ctx, `SELECT
                (SELECT count(*) FROM finished_task WHERE task_id=ANY($1))=2
                AND NOT EXISTS(SELECT 1 FROM pending_task WHERE function_name=$2)`, ownerIds, legacyColdPageMirrorFunction).Scan(&exact))
			if !exact {
				t.Fatal("mirror task finalization or financial replay lost the durable boundary")
			}
		})
	})
}
