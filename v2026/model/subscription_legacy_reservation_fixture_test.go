// Fixtures for contracts that hold a legacy (unmarked) reservation, shared by
// settlement, expiry and provider-total tests. Such a contract settles inline,
// through the original atomic debit/outcome path.
package model

import (
	"context"
	"testing"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/task"
)

// A legacy reservation of 100 bytes closed with two 11-byte finals. The second
// final settles it in its own close.
func legacySettlementTestContract(t testing.TB, ctx context.Context) (netEscrowOrderingTestFixture, server.Id) {
	t.Helper()
	f := newNetEscrowOrderingTestFixture(t, ctx)
	e, posts := createNetEscrowOrderingTestContract(ctx, f, 100)
	server.RunPosts(ctx, posts...)
	server.Raise(CloseContract(ctx, e.ContractId, f.sourceId, 11, false))
	server.Raise(CloseContract(ctx, e.ContractId, f.destinationId, 11, false))
	return f, e.ContractId
}

// An open legacy reservation of 100 bytes with two 11-byte final reports. The
// destination's genuine report owner commits without its settlement
// continuation, so no close has settled the reservation yet.
func legacySettlementTestOpenFinals(t testing.TB, ctx context.Context) (netEscrowOrderingTestFixture, server.Id) {
	t.Helper()
	f := newNetEscrowOrderingTestFixture(t, ctx)
	e, posts := createNetEscrowOrderingTestContract(ctx, f, 100)
	server.RunPosts(ctx, posts...)
	server.Raise(CloseContract(ctx, e.ContractId, f.sourceId, 11, false))
	server.Tx(ctx, func(tx server.PgTx) {
		applied, _, err := applyContractCloseReportInTx(ctx, tx, e.ContractId, f.destinationId, 11, false, nil)
		server.Raise(err)
		if !applied {
			t.Fatal("destination final report did not commit")
		}
	}, server.TxReadCommitted)
	return f, e.ContractId
}

// A legacy reservation's settlement left no financial prefix: no outcome,
// usage proof or payout row.
func requireLegacySettlementNoFinancialPrefix(t testing.TB, ctx context.Context, id server.Id) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		var intact bool
		server.Raise(conn.QueryRow(ctx, `SELECT outcome IS NULL AND provider_usage IS NULL
			AND NOT EXISTS(SELECT 1 FROM transfer_escrow_sweep WHERE contract_id=$1)
			FROM transfer_contract WHERE contract_id=$1`, id).Scan(&intact))
		if !intact {
			t.Fatal("refused settlement changed proof, outcome or payout ledger")
		}
	})
}

// Seeds an exact legacy reservation with a chosen identity, then lets public
// CloseContract create both reports; the second final settles it inline.
func newLegacyReservationTestContract(t testing.TB, ctx context.Context, f netEscrowOrderingTestFixture, id server.Id, reserved, used ByteCount) server.Id {
	t.Helper()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract
 (contract_id,source_network_id,source_id,destination_network_id,destination_id,payer_network_id,transfer_byte_count,usage_origin_is_source)
 VALUES($1,$2,$3,$4,$5,$2,$6,true)`, id, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId, reserved))
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_escrow(contract_id,balance_id,balance_byte_count) VALUES($1,$2,$3)`, id, f.balanceId, reserved))
	})
	refreshNetEscrow(ctx, []server.Id{f.balanceId})
	server.Raise(CloseContract(ctx, id, f.sourceId, used, false))
	server.Raise(CloseContract(ctx, id, f.destinationId, used, false))
	return id
}

// Each party's retained report, as one comparable value.
func contractCloseTestReports(ctx context.Context, id server.Id) []byte {
	var reports []byte
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT jsonb_agg(jsonb_build_array(party,used_transfer_byte_count,checkpoint,close_time) ORDER BY party) FROM contract_close WHERE contract_id=$1`, id).Scan(&reports))
	})
	return reports
}

// The contract's outcome and escrow metadata, the payer's grant credit and its
// projected reservation.
func requireLegacySettlementTestState(t testing.TB, ctx context.Context, f netEscrowOrderingTestFixture, id server.Id, terminal bool, credit, reserved ByteCount) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		var gotTerminal, settled bool
		var gotCredit ByteCount
		server.Raise(conn.QueryRow(ctx, `SELECT outcome IS NOT NULL,(SELECT settled FROM transfer_escrow WHERE contract_id=$1 AND balance_id=$2),
          (SELECT balance_byte_count FROM transfer_balance WHERE balance_id=$2)
          FROM transfer_contract WHERE contract_id=$1`, id, f.balanceId).Scan(&gotTerminal, &settled, &gotCredit))
		if gotTerminal != terminal || settled != terminal || gotCredit != credit {
			t.Fatalf("legacy state terminal=%t metadata=%t credit=%d", gotTerminal, settled, gotCredit)
		}
	})
	if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != reserved {
		t.Fatalf("legacy reservation=%d want=%d", got, reserved)
	}
}

// Account totals are now projected separately from the financial commit. Tests
// that inspect the applied account explicitly run the real task target first;
// this helper leaves each applied pending row for the worker lifecycle controls.
func projectLegacyProviderTotalsForTest(t testing.TB, ctx context.Context) {
	t.Helper()
	target := task.NewTaskTarget(ApplyLegacyProviderTotals)
	taskIds := []server.Id{}
	server.Db(ctx, func(conn server.PgConn) {
		rows, err := conn.Query(ctx, `SELECT task_id FROM pending_task
            WHERE function_name=$1 AND NOT COALESCE((args_json::jsonb->>'applied')::boolean,false)
            ORDER BY task_id`, target.TargetFunctionName())
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				var taskId server.Id
				server.Raise(rows.Scan(&taskId))
				taskIds = append(taskIds, taskId)
			}
		})
	})
	pending := task.GetTasks(ctx, taskIds...)
	for _, taskId := range taskIds {
		owner := pending[taskId]
		if owner == nil {
			t.Fatal("legacy provider projection lost its pending owner")
		}
		if _, _, err := target.RunSpecific(ctx, owner); err != nil {
			t.Fatalf("legacy provider total projection failed: %v", err)
		}
	}
}

// The bytes one contract swept to the destination network.
func requireLegacyProviderSwept(t testing.TB, ctx context.Context, f netEscrowOrderingTestFixture, id server.Id, bytes int64) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		var swept int64
		server.Raise(conn.QueryRow(ctx, `SELECT COALESCE(sum(payout_byte_count),0) FROM transfer_escrow_sweep
            WHERE contract_id=$1 AND network_id=$2`, id, f.destinationNetworkId).Scan(&swept))
		if swept != bytes {
			t.Fatalf("contract swept %d bytes to its provider, want %d", swept, bytes)
		}
	})
}

// This remains assertion-only: lost acknowledgements and omitted posts must
// leave exact totals durable without executing their projection. One statement
// observes the account plus unapplied payloads consistently across an apply commit.
// The account and its unapplied payloads are the destination network's, so id
// must be the only contract that pays that network.
func requireLegacyProviderDurability(t testing.TB, ctx context.Context, f netEscrowOrderingTestFixture, id server.Id, bytes int64, expectedRevenue ...int64) {
	t.Helper()
	requireLegacyProviderCohortDurability(t, ctx, f, []server.Id{id}, bytes, expectedRevenue...)
}

// requireLegacyProviderDurability for several contracts that pay the same
// destination network: their combined sweep is its whole durable total.
func requireLegacyProviderCohortDurability(t testing.TB, ctx context.Context, f netEscrowOrderingTestFixture, ids []server.Id, bytes int64, expectedRevenue ...int64) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		var swept, provided, sweptRevenue, providedRevenue int64
		var pendingBytes, pendingRevenue int64
		server.Raise(conn.QueryRow(ctx, `WITH unapplied AS (
            SELECT allocation
            FROM pending_task
            CROSS JOIN LATERAL jsonb_array_elements(args_json::jsonb->'totals') AS allocation
            WHERE function_name=$3 AND (args_json::jsonb->>'applied')::boolean=false
                AND (allocation->>'network_id')::uuid=$2
        ) SELECT COALESCE((SELECT sum(payout_byte_count) FROM transfer_escrow_sweep
            WHERE contract_id=ANY($1) AND network_id=$2),0),COALESCE((SELECT provided_byte_count FROM account_balance WHERE network_id=$2),0),
            COALESCE((SELECT sum(payout_net_revenue_nano_cents) FROM transfer_escrow_sweep WHERE contract_id=ANY($1) AND network_id=$2),0),
            COALESCE((SELECT provided_net_revenue_nano_cents FROM account_balance WHERE network_id=$2),0),
            COALESCE((SELECT sum((allocation->>'bytes')::bigint) FROM unapplied),0),
            COALESCE((SELECT sum((allocation->>'revenue')::bigint) FROM unapplied),0)`,
			ids, f.destinationNetworkId, task.NewTaskTarget(ApplyLegacyProviderTotals).TargetFunctionName()).Scan(
			&swept, &provided, &sweptRevenue, &providedRevenue, &pendingBytes, &pendingRevenue))
		revenue := int64(0)
		if len(expectedRevenue) > 0 {
			revenue = expectedRevenue[0]
		}
		if swept != bytes || provided+pendingBytes != bytes || sweptRevenue != revenue || providedRevenue+pendingRevenue != revenue {
			t.Fatalf("durable provider accounting swept=%d provided=%d pending=%d revenue=%d/%d/%d want=%d/%d", swept, provided, pendingBytes, sweptRevenue, providedRevenue, pendingRevenue, bytes, revenue)
		}
	})
	server.Redis(ctx, func(r server.RedisClient) {
		if value := r.Get(ctx, accountBalanceNetPayoutByteCountKey(f.destinationNetworkId)).Val(); value != "" && value != "0" {
			t.Fatal("worker duplicated its durable provider contribution in Redis")
		}
	})
}
