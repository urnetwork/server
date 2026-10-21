package model

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/task"
)

// One real transaction supplies the busy cause. Releasing only its grant lock
// lets the same fixed funded cohort settle. SQL deltas observe the actual busy,
// successful and empty-replay paths; no scheduler race or sleep creates them.
func TestLegacySettlementHeldGrantQueryWork(t *testing.T) {
	if os.Getenv("URN_LEGACY_REQUIRE_PGSS") != "1" {
		t.Skip("isolated statement statistics are required for query-work control")
	}
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
		defer cancel()
		if os.Getenv("URN_LEGACY_REQUIRE_PGSS") == "1" {
			server.Db(ctx, func(conn server.PgConn) {
				server.RaisePgResult(conn.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS pg_stat_statements`))
			})
		}
		f := newNetEscrowOrderingTestFixture(t, ctx)
		const count = 64
		ids := make([]server.Id, count)
		due := make([]time.Time, count)
		prefix := server.NewId()
		for index := range ids {
			ids[index] = prefix
			binary.BigEndian.PutUint32(ids[index][11:15], uint32(index+1))
			ids[index][15] = 1
			due[index] = time.Date(2010, time.January, 1, 0, 0, index, 0, time.UTC)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET net_revenue_nano_cents=2000 WHERE balance_id=$1`, f.balanceId))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract
				(contract_id,source_network_id,source_id,destination_network_id,destination_id,payer_network_id,transfer_byte_count,usage_origin_is_source)
				SELECT id,$2,$3,$4,$5,$2,2,true FROM unnest($1::uuid[]) AS seed(id)`,
				ids, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_escrow(contract_id,balance_id,balance_byte_count)
				SELECT id,$2,2 FROM unnest($1::uuid[]) AS seed(id)`, ids, f.balanceId))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_close(contract_id,party,used_transfer_byte_count,close_time,checkpoint)
				SELECT id,party,1,statement_timestamp() AT TIME ZONE 'UTC',false FROM unnest($1::uuid[]) AS seed(id)
				CROSS JOIN (VALUES ('source'),('destination')) AS parties(party)`, ids))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO legacy_settlement_intent(contract_id,shard,outcome,clear_dispute,next_attempt_time)
				SELECT id,1,'settled',false,due FROM unnest($1::uuid[],$2::timestamp[]) AS seed(id,due)`, ids, due))
		})
		refreshNetEscrow(ctx, []server.Id{f.balanceId})
		readReports := func() []byte {
			var reports []byte
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT jsonb_agg(jsonb_build_array(contract_id,party,used_transfer_byte_count,checkpoint,close_time)
					ORDER BY contract_id,party) FROM contract_close WHERE contract_id=ANY($1)`, ids).Scan(&reports))
			})
			return reports
		}
		originalReports := readReports()
		const mirrorTarget = "github.com/urnetwork/server/v2026/model.ApplyLegacyNetEscrowMirror"
		providerTarget := task.NewTaskTarget(ApplyLegacyProviderTotals).TargetFunctionName()
		requireState := func(settled bool) {
			t.Helper()
			wantCompleted, wantCredit, wantReserved, wantPaid, wantMirrors := 0, int64(1000), ByteCount(128), int64(0), 0
			if settled {
				wantCompleted, wantCredit, wantReserved, wantPaid, wantMirrors = count, 1000-count, 0, count, 1
			}
			server.Db(ctx, func(conn server.PgConn) {
				var exact bool
				server.Raise(conn.QueryRow(ctx, `WITH unapplied AS (
					SELECT allocation FROM pending_task
					CROSS JOIN LATERAL jsonb_array_elements(args_json::jsonb->'totals') AS allocation
					WHERE function_name=$8 AND (args_json::jsonb->>'applied')::boolean=false
					AND (allocation->>'network_id')::uuid=$3
				) SELECT
					(SELECT balance_byte_count=$4 AND start_balance_byte_count=1000 FROM transfer_balance WHERE balance_id=$2)
					AND (SELECT count(*) FROM transfer_contract WHERE contract_id=ANY($1) AND outcome='settled')=$5
					AND (SELECT count(*) FROM legacy_settlement_intent WHERE contract_id=ANY($1))=$6
					AND (SELECT count(*) FROM transfer_escrow WHERE contract_id=ANY($1) AND settled)=$5
					AND (SELECT count(*) FROM transfer_escrow_sweep WHERE contract_id=ANY($1))=$5
					AND (SELECT COALESCE(sum(payout_byte_count),0) FROM transfer_escrow_sweep WHERE contract_id=ANY($1))=$7
					AND (SELECT COALESCE(sum(payout_net_revenue_nano_cents),0) FROM transfer_escrow_sweep WHERE contract_id=ANY($1))=$7
					AND (SELECT count(*) FROM pending_task WHERE function_name=$9)=$10
					AND (SELECT count(*) FROM pending_task WHERE function_name=$8)=$5
					AND COALESCE((SELECT provided_byte_count FROM account_balance WHERE network_id=$3),0)
					 +COALESCE((SELECT sum((allocation->>'bytes')::bigint) FROM unapplied),0)=$7
					AND COALESCE((SELECT provided_net_revenue_nano_cents FROM account_balance WHERE network_id=$3),0)
					 +COALESCE((SELECT sum((allocation->>'revenue')::bigint) FROM unapplied),0)=$7`,
					ids, f.balanceId, f.destinationNetworkId, wantCredit, wantCompleted, count-wantCompleted,
					wantPaid, providerTarget, mirrorTarget, wantMirrors).Scan(&exact))
				if !exact {
					t.Fatal("held or released grant changed exact financial ownership", settled)
				}
			})
			if Testing_NetEscrowByteCount(ctx, f.balanceId) != wantReserved || !bytes.Equal(originalReports, readReports()) {
				t.Fatal("grant barrier changed reservations or original reports", settled)
			}
		}
		requireState(false)
		conn := acquireContractLifecycleTestConnection(t, ctx)
		defer conn.Release()
		held, err := conn.Begin(ctx)
		server.Raise(err)
		defer held.Rollback(context.Background())
		server.RaisePgResult(held.Exec(ctx, `SELECT balance_id FROM transfer_balance WHERE balance_id=$1 FOR UPDATE`, f.balanceId))
		beforeBusy := legacyTargetSqlSnapshot(t, ctx)
		began := time.Now()
		busy, err := FlushLegacySettlements(ctx, 1, nil, count)
		busyWall := time.Since(began)
		afterBusy := legacyTargetSqlSnapshot(t, ctx)
		if err != nil || busy.Visited != count || busy.BusyGrantSetMismatch != count || busy.BusyOrGone != count || busy.Completed != 0 || busy.Failed != 0 || busy.BusyIntentUnavailable+busy.BusyContractUnavailable != 0 || busy.HeadVisited != 0 || busy.HeadGrantWaitAttempted != 0 {
			t.Fatalf("held grant did not cause exactly64 bounded grant refusals: visited=%d grant_busy=%d completed=%d failed=%d error=%v", busy.Visited, busy.BusyGrantSetMismatch, busy.Completed, busy.Failed, err)
		}
		requireState(false)
		// No credit, escrow, report, intent or timing value changes here.
		server.Raise(held.Rollback(ctx))
		beforeReleased := legacyTargetSqlSnapshot(t, ctx)
		began = time.Now()
		released, err := FlushLegacySettlements(ctx, 1, nil, count)
		releasedWall := time.Since(began)
		afterReleased := legacyTargetSqlSnapshot(t, ctx)
		if err != nil || released.Visited != count || released.Completed != count || released.BusyOrGone != 0 || released.Failed != 0 || released.HeadVisited != 0 || released.HeadGrantWaitAttempted != 0 {
			t.Fatalf("lock release alone failed to settle its same64 funded rows: visited=%d completed=%d busy=%d failed=%d error=%v", released.Visited, released.Completed, released.BusyOrGone, released.Failed, err)
		}
		requireState(true)
		beforeReplay := legacyTargetSqlSnapshot(t, ctx)
		replayed, err := FlushLegacySettlements(ctx, 1, nil, count)
		afterReplay := legacyTargetSqlSnapshot(t, ctx)
		if err != nil || replayed.Visited != 0 {
			t.Fatal("empty due replay repeated a funded financial transition", replayed.Visited, err)
		}
		requireState(true)
		busySql := legacyTargetSqlDelta(t, beforeBusy, afterBusy)
		releasedSql := legacyTargetSqlDelta(t, beforeReleased, afterReleased)
		replaySql := legacyTargetSqlDelta(t, beforeReplay, afterReplay)
		for _, family := range []string{"intent_ownership", "contract_ownership", "grant_membership"} {
			legacyTargetRequireSqlFamily(t, busySql, family, count, count)
		}
		legacyTargetRequireSqlFamily(t, busySql, "grant_ownership", count, 0)
		for _, family := range []string{"outcome_write", "grant_debit"} {
			legacyTargetRequireSqlFamily(t, busySql, family, 0, 0)
		}
		for _, family := range []string{"intent_ownership", "contract_ownership", "grant_membership", "grant_ownership", "outcome_write", "grant_debit"} {
			legacyTargetRequireSqlFamily(t, releasedSql, family, count, count)
			legacyTargetRequireSqlFamily(t, replaySql, family, 0, 0)
		}
		out := map[string]any{
			"kind": "legacy_held_grant_query_work_v1", "source_profile": os.Getenv("URN_LEGACY_SOURCE_PROFILE"), "cohort": count,
			"held":         map[string]any{"visited": busy.Visited, "grant_busy": busy.BusyGrantSetMismatch, "completed": busy.Completed, "wall_ns": busyWall.Nanoseconds(), "phases": busy.Timings, "sql": busySql},
			"released":     map[string]any{"visited": released.Visited, "completed": released.Completed, "wall_ns": releasedWall.Nanoseconds(), "phases": released.Timings, "sql": releasedSql},
			"replay":       map[string]any{"visited": replayed.Visited, "sql": replaySql},
			"conservation": map[string]any{"payer_debit": count, "provider_bytes": count, "provider_revenue_nano_cents": count, "remaining_credit": 1000 - count, "remaining_escrow": 0, "reports_unchanged": true},
			"qualifiers":   []string{"The test owns the grant-lock cause; release changes only that lock.", "Both pages start at a nil cursor, so no head wait or failed statement is needed to manufacture the busy result.", "No wall-time threshold or artificial sleep determines correctness.", "Setup, grant acquisition/release and financial/report oracles lie outside SQL windows.", "Phase families and nested SQL execution times overlap. SQL statements are not client network round trips.", "Native fixture query costs do not attribute Main query owners or latency."},
		}
		raw, err := json.Marshal(out)
		server.Raise(err)
		t.Logf("legacy_held_grant_query_work_metrics=%s", raw)
	})
}
