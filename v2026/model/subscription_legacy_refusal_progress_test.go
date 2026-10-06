package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// A refused intent keeps its entire reservation. Its retry cursor must still
// allow healthy work using the same payer grant, without waiting on a held
// shared financial row or borrowing value from the rejected contract.
func TestLegacyRefusalPreservesDebtAndSamePayerSuccessorProgress(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f, refused := legacySettlementTestIntent(t, ctx)
		good, posts := createNetEscrowOrderingTestContract(ctx, f, 100)
		server.RunPosts(ctx, posts...)
		goodID := good.ContractId
		goodID[15] = refused[15]
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET contract_id=$2 WHERE contract_id=$1`, good.ContractId, goodID))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_escrow SET contract_id=$2 WHERE contract_id=$1`, good.ContractId, goodID))
		})
		server.Raise(CloseContract(ctx, goodID, f.sourceId, 11, false))
		server.Raise(CloseContract(ctx, goodID, f.destinationId, 11, false))
		// Seed the already-observed over-grant report shape. This is a queue
		// ownership control, not evidence of how any Main report was produced.
		oldest := server.NowUtc().Add(-2 * time.Minute)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE contract_close SET used_transfer_byte_count=400
                WHERE contract_id=$1 AND party='destination'`, refused))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE legacy_settlement_intent SET next_attempt_time=$2 WHERE contract_id=$1`, refused, oldest))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE legacy_settlement_intent SET next_attempt_time=$2 WHERE contract_id=$1`, goodID, oldest.Add(time.Second)))
		})
		shard := int(refused[15]) % LegacySettlementShardCount
		conn := acquireContractLifecycleTestConnection(t, ctx)
		defer conn.Release()
		held, err := conn.Begin(ctx)
		server.Raise(err)
		defer held.Rollback(context.Background())
		for _, table := range []string{"transfer_balance", "transfer_balance_net_escrow_revision", "transfer_balance_net_escrow_snapshot"} {
			server.RaisePgResult(held.Exec(ctx, "SELECT balance_id FROM "+table+" WHERE balance_id=$1 FOR UPDATE", f.balanceId))
		}
		bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
		blocked, err := FlushLegacySettlements(bounded, shard, nil, 64)
		cancel()
		if err != nil || blocked.Visited != 2 || blocked.BusyOrGone != 2 || blocked.Failed != 0 || blocked.Completed != 0 {
			t.Fatalf("held shared owner was not skipped: visited=%d busy=%d failed=%d completed=%d err=%v", blocked.Visited, blocked.BusyOrGone, blocked.Failed, blocked.Completed, err)
		}
		requireLegacySettlementTestState(t, ctx, f, refused, true, false, 1000, 200)
		server.Raise(held.Rollback(ctx))
		failure, err := FlushLegacySettlements(ctx, shard, nil, 64)
		if err != nil || failure.Visited != 1 || failure.Failed != 1 || failure.Completed != 0 || failure.Cursor == nil || failure.Cursor.ContractId != refused {
			t.Fatalf("rejection did not preserve the resumable cursor: visited=%d failed=%d completed=%d err=%v", failure.Visited, failure.Failed, failure.Completed, err)
		}
		requireLegacySettlementTestState(t, ctx, f, refused, true, false, 1000, 200)
		progress, err := FlushLegacySettlements(ctx, shard, failure.Cursor, 64)
		if err != nil || progress.Visited != 1 || progress.Completed != 1 || progress.Failed != 0 || progress.Cursor != nil {
			t.Fatalf("rejected head stranded healthy same-payer successor: visited=%d completed=%d failed=%d err=%v", progress.Visited, progress.Completed, progress.Failed, err)
		}
		requireLegacySettlementTestState(t, ctx, f, refused, true, false, 989, 100)
		requireLegacySettlementTestState(t, ctx, f, goodID, false, true, 989, 100)
		server.Db(ctx, func(conn server.PgConn) {
			var code string
			var future bool
			var refusedPayout, goodPayout int64
			server.Raise(conn.QueryRow(ctx, `SELECT failure_code,next_attempt_time>clock_timestamp()+interval '14 minutes',
                COALESCE((SELECT sum(payout_byte_count) FROM transfer_escrow_sweep WHERE contract_id=$1),0),
                COALESCE((SELECT sum(payout_byte_count) FROM transfer_escrow_sweep WHERE contract_id=$2),0)
                FROM legacy_settlement_intent WHERE contract_id=$1`, refused, goodID).Scan(&code, &future, &refusedPayout, &goodPayout))
			if code != "accounting" || !future || refusedPayout != 0 || goodPayout != 11 {
				t.Fatal("rejection debt, retry or independent payout changed")
			}
		})
		idle, err := FlushLegacySettlements(ctx, shard, nil, 64)
		if err != nil || idle.Visited != 0 {
			t.Fatal("unchanged accounting refusal entered a hot retry")
		}
	})
}
