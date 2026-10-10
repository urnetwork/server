// Financial attribution uses the same escrow evidence as close ownership,
// including the cohort path that reads its own retained participant headers.
package model

import (
	"bytes"
	"context"
	"testing"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/task"
)

// Two source-funded companions force the cohort path. Their absent payer
// cache must not reverse billing or overwrite the retained usage direction.
func TestContractCloseOwnerCohortCompanionsPayActualProvider(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		requireContractCloseOwnerTestSchema(t, ctx)
		fixture := newNetEscrowOrderingTestFixture(t, ctx)
		var anchor *TransferEscrow
		var posts []func() any
		contractIds := make([]server.Id, 0, 2)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET net_revenue_nano_cents=2000 WHERE balance_id=$1`, fixture.balanceId))
			var nextPosts []func() any
			var err error
			anchor, nextPosts, err = createTransferEscrowInTx(ctx, tx,
				fixture.destinationNetworkId, fixture.destinationId, fixture.sourceNetworkId, fixture.sourceId,
				fixture.sourceNetworkId, 100, nil)
			server.Raise(err)
			posts = append(posts, nextPosts...)
			for range 2 {
				companion, companionPosts, err := createTransferEscrowInTx(ctx, tx,
					fixture.sourceNetworkId, fixture.sourceId, fixture.destinationNetworkId, fixture.destinationId,
					fixture.sourceNetworkId, 100, &anchor.ContractId)
				server.Raise(err)
				posts = append(posts, companionPosts...)
				contractIds = append(contractIds, companion.ContractId)
				server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET payer_network_id=NULL WHERE contract_id=$1`, companion.ContractId))
			}
		}, server.TxReadCommitted, server.OptNoRetry())
		server.RunPosts(ctx, posts...)
		for _, contractId := range contractIds {
			server.Raise(CloseContract(ctx, contractId, fixture.sourceId, 11, false))
			server.Raise(CloseContract(ctx, contractId, fixture.destinationId, 11, false))
			requireLegacySettlementTestState(t, ctx, fixture, contractId, true, false, 1000, 300)
		}
		reports := legacyFinancialCohortReports(ctx, contractIds)
		owner := ContractCloseOwner{Kind: ContractCloseOwnerPayerNetwork, Id: fixture.sourceNetworkId}
		ownedCtx := context.WithValue(ctx, legacySettlementCloseScopeKey{}, owner)
		attempts, err := flushLegacySettlementCohort(ownedCtx, contractIds)
		if err != nil || len(attempts) != len(contractIds) {
			t.Fatal("source-funded companions did not complete one cohort", attempts, err)
		}
		for index, attempt := range attempts {
			if attempt.contractId != contractIds[index] || !attempt.completed || attempt.busy || attempt.fallback || attempt.deadlineFallback {
				t.Fatal("companion cohort escaped through individual settlement or a busy owner", attempt)
			}
			requireLegacySettlementTestState(t, ctx, fixture, attempt.contractId, false, true, 978, 100)
		}
		requireLegacySettlementTestState(t, ctx, fixture, anchor.ContractId, false, false, 978, 100)
		if !bytes.Equal(reports, legacyFinancialCohortReports(ctx, contractIds)) {
			t.Fatal("resolved cohort payer changed original close reports")
		}
		server.Db(ctx, func(conn server.PgConn) {
			for _, contractId := range contractIds {
				var retained bool
				var providerBytes, providerRevenue int64
				server.Raise(conn.QueryRow(ctx, `SELECT
 payer_network_id IS NULL AND companion_contract_id=$2 AND usage_origin_is_source=false,
 COALESCE((SELECT sum(payout_byte_count) FROM transfer_escrow_sweep
   WHERE contract_id=$1 AND network_id=$3 AND destination_id=$4),0),
 COALESCE((SELECT sum(payout_net_revenue_nano_cents) FROM transfer_escrow_sweep
   WHERE contract_id=$1 AND network_id=$3 AND destination_id=$4),0)
 FROM transfer_contract WHERE contract_id=$1`, contractId, anchor.ContractId,
					fixture.destinationNetworkId, fixture.destinationId).Scan(&retained, &providerBytes, &providerRevenue))
				if !retained || providerBytes != 11 || providerRevenue != 11 {
					t.Fatal("companion billing used its endpoint fallback or changed retained metadata", contractId, retained, providerBytes, providerRevenue)
				}
			}
			for _, expected := range []struct {
				networkId server.Id
				amount    int64
			}{
				{networkId: fixture.destinationNetworkId, amount: 22},
				{networkId: fixture.sourceNetworkId, amount: 0},
			} {
				var sweptBytes, sweptRevenue, durableBytes, durableRevenue int64
				server.Raise(conn.QueryRow(ctx, `WITH unapplied AS (
 SELECT allocation FROM pending_task
 CROSS JOIN LATERAL jsonb_array_elements(args_json::jsonb->'totals') AS allocation
 WHERE function_name=$3 AND NOT COALESCE((args_json::jsonb->>'applied')::boolean,false)
 AND (allocation->>'network_id')::uuid=$2
 ) SELECT
 COALESCE((SELECT sum(payout_byte_count) FROM transfer_escrow_sweep WHERE contract_id=ANY($1) AND network_id=$2),0),
 COALESCE((SELECT sum(payout_net_revenue_nano_cents) FROM transfer_escrow_sweep WHERE contract_id=ANY($1) AND network_id=$2),0),
 COALESCE((SELECT provided_byte_count FROM account_balance WHERE network_id=$2),0)
   + COALESCE((SELECT sum((allocation->>'bytes')::bigint) FROM unapplied),0),
 COALESCE((SELECT provided_net_revenue_nano_cents FROM account_balance WHERE network_id=$2),0)
   + COALESCE((SELECT sum((allocation->>'revenue')::bigint) FROM unapplied),0)`,
					contractIds, expected.networkId, task.NewTaskTarget(ApplyLegacyProviderTotals).TargetFunctionName()).Scan(
					&sweptBytes, &sweptRevenue, &durableBytes, &durableRevenue))
				if sweptBytes != expected.amount || sweptRevenue != expected.amount || durableBytes != expected.amount || durableRevenue != expected.amount {
					t.Fatal("cohort payer and provider accounting diverged", expected.networkId, sweptBytes, sweptRevenue, durableBytes, durableRevenue)
				}
			}
		})
	})
}
