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
func TestLegacySettlementPayoutAccountRequiresAppliedProjection(t *testing.T) {
	providerTotalsTestEnv(t, func(t testing.TB, ctx context.Context) {
		f, id := legacySettlementTestIntent(t, ctx)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET net_revenue_nano_cents=2000 WHERE balance_id=$1`, f.balanceId))
		})
		completed, busy, _, err := flushLegacySettlement(ctx, id)
		if err != nil || !completed || busy {
			t.Fatalf("payout projection fixture settlement completed=%t busy=%t err=%v", completed, busy, err)
		}
		requireLegacyProviderDurability(t, ctx, f, id, 11, 11)
		if got := contractPayoutTestAccountAmount(t, ctx, f.destinationNetworkId); got != (contractPayoutTestAmount{}) {
			t.Fatalf("unapplied earnings masqueraded as account totals: %v", got)
		}

		var projectionId server.Id
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT task_id FROM pending_task WHERE run_once_key=$1`,
				task.RunOnce("legacy_provider_totals", id).String()).Scan(&projectionId))
		})
		owner := task.GetTasks(ctx, projectionId)[projectionId]
		if owner == nil {
			t.Fatal("settlement lost its durable projection owner")
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
		requireLegacyProviderDurability(t, ctx, f, id, 11, 11)
		if got := contractPayoutTestAccountAmount(t, ctx, f.destinationNetworkId); got != (contractPayoutTestAmount{}) {
			t.Fatalf("failed projection acquired account totals: %v", got)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DROP TRIGGER synthetic_payout_projection_failure ON account_balance;
				DROP FUNCTION synthetic_payout_projection_failure()`))
		})

		want := map[server.Id]contractPayoutTestAmount{f.destinationNetworkId: {byteCount: 11, payout: 11}}
		assertContractPayoutTestAccounts(t, ctx, []server.Id{f.sourceNetworkId, f.destinationNetworkId}, want)
		requireProviderTotalsTestState(t, ctx, projectionId, f.destinationNetworkId, true, 11, 11)
		if _, _, err := target.RunSpecific(ctx, owner); err != nil {
			t.Fatalf("applied account projection replay failed: %v", err)
		}
		assertContractPayoutTestAccounts(t, ctx, []server.Id{f.sourceNetworkId, f.destinationNetworkId}, want)
		requireLegacyProviderDurability(t, ctx, f, id, 11, 11)
	})
}
