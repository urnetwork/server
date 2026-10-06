package model

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/task"
)

// An expired grant can still fund its committed legacy reservations. Queue
// density cannot supply missing escrow or turn that accounting refusal into a
// lock refusal. All funding is fixed before execution; the refused row is never
// repaired or credited by this test.
func TestLegacySettlementDenseExpiredGrantSeparatesPartialFunding(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
		defer cancel()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		const predecessors = 129
		const count = predecessors + 2
		ids := make([]server.Id, count)
		reservations, reports := make([]int64, count), make([]int64, count)
		due := make([]time.Time, count)
		prefix := server.NewId()
		oldest := time.Date(2010, time.January, 1, 0, 0, 0, 0, time.UTC)
		for index := range ids {
			ids[index] = prefix
			binary.BigEndian.PutUint32(ids[index][11:15], uint32(index+1))
			ids[index][15] = 1
			reservations[index], reports[index] = 2, 1
			due[index] = oldest.Add(time.Duration(index) * time.Second)
		}
		refused, funded := ids[predecessors], ids[predecessors+1]
		reservations[predecessors], reports[predecessors] = 1, 11
		reservations[predecessors+1], reports[predecessors+1] = 17, 11
		server.Tx(ctx, func(tx server.PgTx) {
			// The ordinary fixture supplied1,000 bytes. Set its price and expiry,
			// retaining that exact credit throughout the financial experiment.
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET
				start_time=timestamp '2009-01-01',end_time=timestamp '2009-12-31',net_revenue_nano_cents=2000
				WHERE balance_id=$1`, f.balanceId))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract
				(contract_id,source_network_id,source_id,destination_network_id,destination_id,payer_network_id,transfer_byte_count,usage_origin_is_source,create_time)
				SELECT id,$2,$3,$4,$5,$2,reserved,true,timestamp '2009-06-01'
				FROM unnest($1::uuid[],$6::bigint[]) AS seed(id,reserved)`,
				ids, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId, reservations))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_escrow(contract_id,balance_id,balance_byte_count)
				SELECT id,$2,reserved FROM unnest($1::uuid[],$3::bigint[]) AS seed(id,reserved)`, ids, f.balanceId, reservations))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_close(contract_id,party,used_transfer_byte_count,close_time,checkpoint)
				SELECT id,party,used,timestamp '2010-01-01',false FROM unnest($1::uuid[],$2::bigint[]) AS seed(id,used)
				CROSS JOIN (VALUES ('source'),('destination')) AS parties(party)`, ids, reports))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO legacy_settlement_intent(contract_id,shard,outcome,clear_dispute,next_attempt_time)
				SELECT id,1,'settled',false,due FROM unnest($1::uuid[],$2::timestamp[]) AS seed(id,due)`, ids, due))
		})
		refreshNetEscrow(ctx, []server.Id{f.balanceId})
		beforeRefused, beforeFunded := legacyDrainTestReports(ctx, refused), legacyDrainTestReports(ctx, funded)
		requireRefusal := func() {
			t.Helper()
			completed, busy, gate, err := flushLegacySettlement(ctx, refused)
			if !errors.Is(err, errContractInsufficientEscrow) || completed || busy || gate != legacySettlementBusyNone {
				t.Fatalf("partial funding gained completion or lock-refusal authority: completed=%v busy=%v gate=%d error=%v", completed, busy, gate, err)
			}
			requireLegacyDrainNoFinancialPrefix(t, ctx, refused)
		}
		// This exact callback proves its real accounting error independently of
		// any aggregate page count or queue traversal.
		requireRefusal()
		var cursor *LegacySettlementCursor
		visited, completed, failed, pageCount := 0, 0, 0, 0
		for {
			page, err := FlushLegacySettlements(ctx, 1, cursor, 64)
			if err != nil || page.BusyOrGone != 0 {
				t.Fatalf("unlocked financial cohort invented contention: busy=%d error=%v", page.BusyOrGone, err)
			}
			visited += page.Visited
			completed += page.Completed
			failed += page.Failed
			pageCount++
			if page.Cursor == nil {
				break
			}
			if pageCount >= count || page.Visited == 0 {
				t.Fatal("bounded financial traversal stopped advancing")
			}
			cursor = page.Cursor
		}
		if visited != count || completed != count-1 || failed != 1 || pageCount < 3 {
			t.Fatalf("dense traversal confused accounting with progress: visited=%d completed=%d failed=%d pages=%d", visited, completed, failed, pageCount)
		}
		const spent = int64(predecessors + 11)
		requireState := func() {
			t.Helper()
			server.Db(ctx, func(conn server.PgConn) {
				var exact bool
				server.Raise(conn.QueryRow(ctx, `WITH unapplied AS (
					SELECT allocation FROM pending_task
					CROSS JOIN LATERAL jsonb_array_elements(args_json::jsonb->'totals') AS allocation
					WHERE function_name=$7 AND (args_json::jsonb->>'applied')::boolean=false
					AND (allocation->>'network_id')::uuid=$4
				) SELECT
					(SELECT balance_byte_count=$5 AND start_balance_byte_count=1000 AND end_time=timestamp '2009-12-31'
					 FROM transfer_balance WHERE balance_id=$2)
					AND (SELECT count(*) FROM transfer_contract WHERE contract_id=ANY($1) AND outcome='settled')=$6
					AND (SELECT count(*) FROM transfer_escrow_sweep WHERE contract_id=ANY($1))=$6
					AND (SELECT sum(payout_byte_count) FROM transfer_escrow_sweep WHERE contract_id=ANY($1))=$8
					AND (SELECT sum(payout_net_revenue_nano_cents) FROM transfer_escrow_sweep WHERE contract_id=ANY($1))=$8
					AND (SELECT count(*) FROM legacy_settlement_intent WHERE contract_id=ANY($1))=1
					AND (SELECT failure_code='accounting' AND next_attempt_time>statement_timestamp() AT TIME ZONE 'UTC'
					 FROM legacy_settlement_intent WHERE contract_id=$3)
					AND (SELECT NOT settled AND balance_byte_count=1 AND COALESCE(payout_byte_count,0)=0
					 FROM transfer_escrow WHERE contract_id=$3 AND balance_id=$2)
					AND COALESCE((SELECT provided_byte_count FROM account_balance WHERE network_id=$4),0)
					 +COALESCE((SELECT sum((allocation->>'bytes')::bigint) FROM unapplied),0)=$8
					AND COALESCE((SELECT provided_net_revenue_nano_cents FROM account_balance WHERE network_id=$4),0)
					 +COALESCE((SELECT sum((allocation->>'revenue')::bigint) FROM unapplied),0)=$8`,
					ids, f.balanceId, refused, f.destinationNetworkId, 1000-spent, count-1,
					task.NewTaskTarget(ApplyLegacyProviderTotals).TargetFunctionName(), spent).Scan(&exact))
				if !exact {
					t.Fatal("dense refusal/funded completion changed payer, provider or reservation conservation")
				}
			})
			requireLegacyDrainNoFinancialPrefix(t, ctx, refused)
			if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != 1 {
				t.Fatal("funded closes released the refused reservation", got)
			}
			if !bytes.Equal(beforeRefused, legacyDrainTestReports(ctx, refused)) || !bytes.Equal(beforeFunded, legacyDrainTestReports(ctx, funded)) {
				t.Fatal("financial execution changed original usage reports")
			}
		}
		requireState()
		requireRefusal()
		if again, err := FlushLegacySettlements(ctx, 1, nil, 64); err != nil || again.Visited != 0 {
			t.Fatal("accounting hold was blindly retried by the due selector", again.Visited, err)
		}
		if complete, busy, _, err := flushLegacySettlement(ctx, funded); err != nil || complete || !busy {
			t.Fatal("funded replay claimed its financial outcome twice", complete, busy, err)
		}
		requireState()
		t.Logf("expired_grant_fixed_credit=1000 dense_predecessors=%d genuine_accounting_refusals=1 completed=%d paid_bytes=%d remaining_credit=%d retained_escrow=1 replay_unchanged=true", predecessors, completed, spent, 1000-spent)
	})
}
