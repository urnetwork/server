package model

import (
	"context"
	"testing"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/task"
)

// A committed sweep is earned money, but cannot stand in for a successfully
// applied account projection. A rejected owner must remain visible as missing
// account totals, and replay of that same owner must not repeat its increment.
// Settlement queues no projection since 2026-11-11 (BILLING-SPLIT.md); the row
// here is one an older binary queued, which the per-task reader still applies.
func TestLegacySettlementPayoutAccountRequiresAppliedProjection(t *testing.T) {
	providerTotalsTestEnv(t, func(t testing.TB, ctx context.Context) {
		f, id := legacySettlementTestOpenFinals(t, ctx)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET net_revenue_nano_cents=2000 WHERE balance_id=$1`, f.balanceId))
		})
		if err := SettleEscrow(ctx, id, ContractOutcomeSettled); err != nil {
			t.Fatalf("payout projection fixture settlement failed: %v", err)
		}
		requireLegacyProviderDurability(t, ctx, f, id, 11, 11)

		projectionId := queueLegacyProviderTotalsFromSweepsForTest(t, ctx, id)
		requireLegacyProviderBacklogDurability(t, ctx, f, id, 11, 11)
		if got := contractPayoutTestAccountAmount(t, ctx, f.destinationNetworkId); got != (contractPayoutTestAmount{}) {
			t.Fatalf("unapplied earnings masqueraded as account totals: %v", got)
		}
		owner := task.GetTasks(ctx, projectionId)[projectionId]
		if owner == nil {
			t.Fatal("queued projection lost its durable owner")
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `CREATE FUNCTION synthetic_payout_projection_failure() RETURNS trigger LANGUAGE plpgsql AS
				'BEGIN RAISE EXCEPTION ''synthetic payout projection refusal''; END';
				CREATE TRIGGER synthetic_payout_projection_failure BEFORE INSERT OR UPDATE ON account_balance
				FOR EACH ROW EXECUTE FUNCTION synthetic_payout_projection_failure()`))
		})
		target := task.NewTaskTarget(ApplyLegacyProviderTotals)
		if _, _, err := target.RunSpecific(ctx, owner); err == nil {
			t.Fatal("failed account projection reported success")
		}
		requireProviderTotalsTestState(t, ctx, projectionId, f.destinationNetworkId, false, 0, 0)
		requireLegacyProviderBacklogDurability(t, ctx, f, id, 11, 11)
		if got := contractPayoutTestAccountAmount(t, ctx, f.destinationNetworkId); got != (contractPayoutTestAmount{}) {
			t.Fatalf("failed projection acquired account totals: %v", got)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DROP TRIGGER synthetic_payout_projection_failure ON account_balance;
				DROP FUNCTION synthetic_payout_projection_failure()`))
		})

		want := contractPayoutTestAmount{byteCount: 11, payout: 11}
		projectLegacyProviderTotalsForTest(t, ctx)
		if got := contractPayoutTestAccountAmount(t, ctx, f.destinationNetworkId); got != want {
			t.Fatalf("applied projection account totals = %v, want %v", got, want)
		}
		requireProviderTotalsTestState(t, ctx, projectionId, f.destinationNetworkId, true, 11, 11)
		if _, _, err := target.RunSpecific(ctx, owner); err != nil {
			t.Fatalf("applied account projection replay failed: %v", err)
		}
		if got := contractPayoutTestAccountAmount(t, ctx, f.destinationNetworkId); got != want {
			t.Fatalf("replayed projection account totals = %v, want %v", got, want)
		}
		if got := contractPayoutTestAccountAmount(t, ctx, f.sourceNetworkId); got != (contractPayoutTestAmount{}) {
			t.Fatalf("payer acquired provider totals: %v", got)
		}
		requireLegacyProviderBacklogDurability(t, ctx, f, id, 11, 11)
	})
}
