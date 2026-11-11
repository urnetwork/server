package model

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// One provider network's share of a contract, as settlement summed it for the
// lifetime provider totals before 2026-11-11.
type contractPayout struct {
	payoutByteCount ByteCount
	payout          NanoCents
}

// The producer that settlement ran before 2026-11-11 (BILLING-SPLIT.md),
// byte for byte: one ApplyLegacyProviderTotals row per contract, under its
// run-once queue owner. Settlement no longer queues these rows. The per-task
// reader and the drain still consume the rows already queued on Main and those
// older binaries queue during a rollout, so their tests produce the same rows
// here.
func queueLegacyProviderTotalsInTx(ctx context.Context, tx server.PgTx, contractId server.Id, payouts map[server.Id]*contractPayout) server.Id {
	payload := legacyProviderTotalsPayload{Private: true, Version: 1, ContractId: contractId}
	for networkId, payout := range payouts {
		payload.Totals = append(payload.Totals, legacyProviderTotal{NetworkId: networkId, Bytes: payout.payoutByteCount, Revenue: payout.payout})
	}
	slices.SortFunc(payload.Totals, func(a, b legacyProviderTotal) int { return a.NetworkId.Cmp(b.NetworkId) })
	data, err := json.Marshal(payload)
	server.Raise(err)
	owner := session.NewLocalClientSession(ctx, "", nil)
	defer owner.Cancel()
	// The drain removes the row before the per-task reader may claim it
	// (subscription_provider_totals_drain.go).
	scheduled, taskId := task.ScheduleTaskInTxIfAbsent(tx, ApplyLegacyProviderTotals, json.RawMessage(data), owner,
		task.RunOnce("legacy_provider_totals", contractId), task.RunAt(server.NowUtc().Add(legacyProviderTotalsDeferral)),
		task.MaxTime(10*time.Second), task.RequireQueueOwnership(tx))
	if !scheduled {
		// Never merge a second allocation into an existing task's immutable
		// arguments. The contract outcome must admit exactly one insertion.
		server.Raise(errors.New("legacy provider total task already exists"))
	}
	return taskId
}

// A provider network's earnings are its sweeps. Settlement credits no
// lifetime provider totals since 2026-11-11 (BILLING-SPLIT.md), so the
// account balance's provided totals stay zero while the network's sweeps hold
// the exact bytes and, within revenueTolerance nanocents, the revenue the
// totals used to show.
func assertProviderSweepTotalsForTest(t testing.TB, clientSession *session.ClientSession, wantBytes ByteCount, wantRevenue NanoCents, revenueTolerance int64) {
	t.Helper()
	balance := GetAccountBalance(clientSession).Balance
	if balance.ProvidedByteCount != 0 || balance.ProvidedNetRevenue != 0 {
		t.Fatalf("settlement credited lifetime provider totals %d/%d", balance.ProvidedByteCount, balance.ProvidedNetRevenue)
	}
	var bytes ByteCount
	var revenue NanoCents
	server.Db(clientSession.Ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(clientSession.Ctx, `SELECT COALESCE(sum(payout_byte_count),0)::bigint, COALESCE(sum(payout_net_revenue_nano_cents),0)::bigint
			FROM transfer_escrow_sweep WHERE network_id=$1`, clientSession.ByJwt.NetworkId).Scan(&bytes, &revenue))
	})
	if bytes != wantBytes || revenue-wantRevenue < -revenueTolerance || revenueTolerance < revenue-wantRevenue {
		t.Fatalf("provider sweeps %d bytes, %d nanocents; want %d bytes, %d nanocents (within %d)", bytes, revenue, wantBytes, wantRevenue, revenueTolerance)
	}
}

// Queues the contract's sweeps to its provider networks as one provider totals
// row, as settlement did before 2026-11-11, under the row's queue owner.
func queueLegacyProviderTotalsFromSweepsForTest(t testing.TB, ctx context.Context, contractId server.Id) server.Id {
	t.Helper()
	payouts := map[server.Id]*contractPayout{}
	server.Db(ctx, func(conn server.PgConn) {
		rows, err := conn.Query(ctx, `SELECT network_id, sum(payout_byte_count)::bigint, sum(payout_net_revenue_nano_cents)::bigint
			FROM transfer_escrow_sweep WHERE contract_id=$1 GROUP BY network_id`, contractId)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				var networkId server.Id
				payout := &contractPayout{}
				server.Raise(rows.Scan(&networkId, &payout.payoutByteCount, &payout.payout))
				payouts[networkId] = payout
			}
		})
	})
	if len(payouts) == 0 {
		t.Fatal("contract has no sweep to queue provider totals for")
	}
	var taskId server.Id
	server.OwnedTx(ctx, []server.PgOwnershipKey{task.RunOnceOwnershipKey(task.RunOnce("legacy_provider_totals", contractId))}, func(tx server.PgTx) {
		taskId = queueLegacyProviderTotalsInTx(ctx, tx, contractId, payouts)
	}, server.TxReadCommitted)
	return taskId
}

// The consumer's conservation for a row an older binary queued: the sweep
// holds the contract's exact amounts, and the destination network's account
// plus its unapplied payloads hold the same, before and after the row applies.
func requireLegacyProviderBacklogDurability(t testing.TB, ctx context.Context, f netEscrowOrderingTestFixture, id server.Id, bytes, revenue int64) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		var swept, sweptRevenue, provided, providedRevenue, pendingBytes, pendingRevenue int64
		server.Raise(conn.QueryRow(ctx, `WITH unapplied AS (
            SELECT allocation
            FROM pending_task
            CROSS JOIN LATERAL jsonb_array_elements(args_json::jsonb->'totals') AS allocation
            WHERE function_name=$3 AND (args_json::jsonb->>'applied')::boolean=false
                AND (allocation->>'network_id')::uuid=$2
        ) SELECT COALESCE((SELECT sum(payout_byte_count) FROM transfer_escrow_sweep WHERE contract_id=$1 AND network_id=$2),0),
            COALESCE((SELECT sum(payout_net_revenue_nano_cents) FROM transfer_escrow_sweep WHERE contract_id=$1 AND network_id=$2),0),
            COALESCE((SELECT provided_byte_count FROM account_balance WHERE network_id=$2),0),
            COALESCE((SELECT provided_net_revenue_nano_cents FROM account_balance WHERE network_id=$2),0),
            COALESCE((SELECT sum((allocation->>'bytes')::bigint) FROM unapplied),0),
            COALESCE((SELECT sum((allocation->>'revenue')::bigint) FROM unapplied),0)`,
			id, f.destinationNetworkId, task.NewTaskTarget(ApplyLegacyProviderTotals).TargetFunctionName()).Scan(
			&swept, &sweptRevenue, &provided, &providedRevenue, &pendingBytes, &pendingRevenue))
		if swept != bytes || sweptRevenue != revenue || provided+pendingBytes != bytes || providedRevenue+pendingRevenue != revenue {
			t.Fatalf("queued provider accounting swept=%d/%d provided=%d/%d pending=%d/%d want=%d/%d",
				swept, sweptRevenue, provided, providedRevenue, pendingBytes, pendingRevenue, bytes, revenue)
		}
	})
}
