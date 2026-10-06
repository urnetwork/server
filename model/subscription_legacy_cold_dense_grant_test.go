package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/task"
)

// Bulk setup is synthetic; every outcome, debit, sweep, provider allocation and
// queued mirror below is produced by the ordinary financial worker. One grant
// shared by 1,000 due intents must not create 1,000 historical mirror owners.
func TestLegacySettlementColdDenseGrantCoalescesThousandCloses(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
		defer cancel()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		const count = 1000
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance
                SET start_balance_byte_count=4000,balance_byte_count=4000,net_revenue_nano_cents=8000
                WHERE balance_id=$1`, f.balanceId))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract
				(contract_id,source_network_id,source_id,destination_network_id,destination_id,payer_network_id,transfer_byte_count,usage_origin_is_source)
				SELECT (substr(md5($5::uuid::text||'-dense-cold-'||n),1,30)||'01')::uuid,$1,$2,$3,$4,$1,2,true
                FROM generate_series(1,$6)n`, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId, f.balanceId, count))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_escrow(contract_id,balance_id,balance_byte_count)
                SELECT (substr(md5($1::uuid::text||'-dense-cold-'||n),1,30)||'01')::uuid,$1,2
                FROM generate_series(1,$2)n`, f.balanceId, count))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_close(contract_id,party,used_transfer_byte_count,close_time,checkpoint)
                SELECT (substr(md5($1::uuid::text||'-dense-cold-'||n),1,30)||'01')::uuid,p.party,1,now() AT TIME ZONE 'UTC',false
                FROM generate_series(1,$2)n CROSS JOIN (VALUES ('source'),('destination')) p(party)`, f.balanceId, count))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO legacy_settlement_intent(contract_id,shard,outcome,clear_dispute,next_attempt_time)
                SELECT (substr(md5($1::uuid::text||'-dense-cold-'||n),1,30)||'01')::uuid,1,'settled',false,
                    timestamp '2010-01-01'+n*interval '1 second'
                FROM generate_series(1,$2)n`, f.balanceId, count))
		})
		legacy, posts := createNetEscrowOrderingTestContract(ctx, f, 23)
		server.RunPosts(ctx, posts...)
		redis := createRedisAdmissionTest(ctx, f, 31)
		var redisExpiry float64
		server.Redis(ctx, func(r server.RedisClient) {
			var err error
			redisExpiry, err = r.ZScore(ctx, redisContractReservationKeys(f.balanceId)[2], redis.ContractId.String()).Result()
			server.Raise(err)
		})
		ids := []server.Id{}
		server.Db(ctx, func(conn server.PgConn) {
			rows, err := conn.Query(ctx, `SELECT contract_id FROM legacy_settlement_intent WHERE shard=1 ORDER BY next_attempt_time,contract_id`)
			server.WithPgResult(rows, err, func() {
				for rows.Next() {
					var id server.Id
					server.Raise(rows.Scan(&id))
					ids = append(ids, id)
				}
			})
		})
		if len(ids) != count {
			t.Fatal("synthetic dense grant did not have exactly 1,000 due intents")
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM transfer_balance_net_escrow_snapshot WHERE balance_id=$1`, f.balanceId))
		})
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
		done := make(chan legacyColdPageResult, 1)
		began := time.Now()
		go func() {
			page, err := flushLegacySettlementsPage(ctx, bounded, 1, nil, 64, flushLegacySettlementWithGrantWait)
			done <- legacyColdPageResult{page: page, err: err}
		}()
		first, blocked := legacyColdPageWait(t, ctx, held, blocker, done)
		if blocked {
			cancelPage(errLegacySettlementPageBudget)
			server.Raise(held.Rollback(ctx))
			select {
			case <-done:
			case <-ctx.Done():
				t.Fatal("dense grant baseline census failed to release")
			}
			t.Fatal("dense grant foreground entered historical census")
		}
		page, err := first.page, first.err
		completed, pages := 0, 0
		for {
			if err != nil || page.Completed != page.Visited || page.Completed == 0 || page.BusyOrGone != 0 || page.Failed != 0 {
				t.Fatalf("dense grant page failed: %+v err=%v", page, err)
			}
			completed += page.Completed
			pages++
			if page.Cursor == nil {
				break
			}
			if pages >= 16 {
				t.Fatal("bounded cold-free pages did not finish fixed 1,000 cohort")
			}
			page, err = flushLegacySettlementsPage(ctx, bounded, 1, page.Cursor, 64, flushLegacySettlementWithGrantWait)
		}
		if completed != count || pages != 16 {
			t.Fatalf("dense grant progress=%d pages=%d, want1000/16", completed, pages)
		}
		elapsed := time.Since(began)
		if len(settlementCacheSnapshot(ctx, []server.Id{f.balanceId})) != 0 {
			t.Fatal("dense foreground fabricated a cold snapshot")
		}
		assertFinancial := func() {
			t.Helper()
			server.Db(ctx, func(conn server.PgConn) {
				var exact bool
				server.Raise(conn.QueryRow(ctx, `WITH unapplied AS (
                    SELECT allocation FROM pending_task
                    CROSS JOIN LATERAL jsonb_array_elements(args_json::jsonb->'totals') AS allocation
                    WHERE function_name=$4 AND (args_json::jsonb->>'applied')::boolean=false
                        AND (allocation->>'network_id')::uuid=$3
                ) SELECT
                    (SELECT count(*) FROM transfer_contract WHERE contract_id=ANY($1) AND outcome='settled')=1000
                    AND NOT EXISTS(SELECT 1 FROM legacy_settlement_intent WHERE contract_id=ANY($1))
                    AND (SELECT count(*) FROM transfer_escrow WHERE contract_id=ANY($1) AND settled)=1000
                    AND (SELECT balance_byte_count FROM transfer_balance WHERE balance_id=$2)=3000
                    AND (SELECT sum(payout_byte_count) FROM transfer_escrow_sweep WHERE contract_id=ANY($1))=1000
                    AND (SELECT sum(payout_net_revenue_nano_cents) FROM transfer_escrow_sweep WHERE contract_id=ANY($1))=1000
                    AND COALESCE((SELECT provided_byte_count FROM account_balance WHERE network_id=$3),0)
                        +COALESCE((SELECT sum((allocation->>'bytes')::bigint) FROM unapplied),0)=1000
                    AND COALESCE((SELECT provided_net_revenue_nano_cents FROM account_balance WHERE network_id=$3),0)
                        +COALESCE((SELECT sum((allocation->>'revenue')::bigint) FROM unapplied),0)=1000`,
					ids, f.balanceId, f.destinationNetworkId, task.NewTaskTarget(ApplyLegacyProviderTotals).TargetFunctionName()).Scan(&exact))
				if !exact {
					t.Fatal("dense grant debit, provider bytes or nano-cents did not conserve")
				}
			})
		}
		assertFinancial()
		var ownerId server.Id
		server.Db(ctx, func(conn server.PgConn) {
			var owners int
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM pending_task WHERE function_name=$1`, legacyColdPageMirrorFunction).Scan(&owners))
			if owners != 1 {
				t.Fatalf("1,000 same-grant closes produced %d mirror owners, want1", owners)
			}
			server.Raise(conn.QueryRow(ctx, `SELECT task_id FROM pending_task
                WHERE function_name=$1 AND (args_json::jsonb->>'balance_id')::uuid=$2`, legacyColdPageMirrorFunction, f.balanceId).Scan(&ownerId))
		})
		t.Logf("dense cold grant: completed=%d pages=%d mirror_owners=1 elapsed=%s; elapsed is observational, not an asserted speedup", completed, pages, elapsed)
		server.Raise(held.Rollback(ctx))
		settings := task.DefaultTaskWorkerSettings()
		settings.ClaimRegisteredTargetsOnly = true
		worker := task.NewTaskWorker(ctx, settings)
		defer worker.Close()
		worker.AddTargets(task.NewTaskTargetWithPost(ApplyLegacyNetEscrowMirror, ApplyLegacyNetEscrowMirrorPost))
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET run_at=$1,release_time=$1 WHERE task_id=$2`, time.Time{}, ownerId))
		})
		finished, retried, postRetried, err := worker.EvalTasks(1)
		if err != nil || len(finished) != 1 || finished[0] != ownerId || len(retried) != 0 || len(postRetried) != 0 {
			t.Fatal("coalesced dense mirror owner failed ordinary finalization", err)
		}
		for _, index := range []int{0, count / 2, count - 1} {
			complete, busy, _, err := flushLegacySettlement(ctx, ids[index])
			if err != nil || complete || !busy {
				t.Fatal("dense completed intent retry reclaimed its financial owner")
			}
		}
		assertFinancial()
		if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != 54 {
			t.Fatalf("dense mirror lost surviving legacy23 + Redis31 neighbors: %d", got)
		}
		server.Redis(ctx, func(r server.RedisClient) {
			keys := redisContractReservationKeys(f.balanceId)
			amount, err := r.HGet(ctx, keys[1], redis.ContractId.String()).Int64()
			server.Raise(err)
			expiry, err := r.ZScore(ctx, keys[2], redis.ContractId.String()).Result()
			server.Raise(err)
			if amount != 31 || expiry != redisExpiry {
				t.Fatal("dense legacy repair altered the native neighbor token")
			}
		})
		server.Db(ctx, func(conn server.PgConn) {
			var exact bool
			server.Raise(conn.QueryRow(ctx, `SELECT
                NOT EXISTS(SELECT 1 FROM pending_task WHERE function_name=$1)
                AND EXISTS(SELECT 1 FROM finished_task WHERE task_id=$2)
                AND (SELECT outcome IS NULL FROM transfer_contract WHERE contract_id=$3)
                AND (SELECT NOT settled AND NOT redis_reserved AND balance_byte_count=23
                    FROM transfer_escrow WHERE contract_id=$3 AND balance_id=$4)`,
				legacyColdPageMirrorFunction, ownerId, legacy.ContractId, f.balanceId).Scan(&exact))
			if !exact {
				t.Fatal("dense finalization/retry recreated an owner or changed neighbor")
			}
		})
	})
}
