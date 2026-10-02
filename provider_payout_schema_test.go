package server

import (
	"context"
	"testing"
)

func TestProviderTransitionSchemaCapabilityRejectsDisabledOrChangedGuard(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		if err := RequireProviderPayoutSchema(ctx); err != nil {
			t.Fatal("current migrated capability refused", err)
		}
		for _, relation := range []struct{ table, trigger string }{
			{table: "account_payment", trigger: "account_payment_submission_basis_guard"},
			{table: "account_payment_bonus", trigger: "account_payment_bonus_guard"},
		} {
			Db(ctx, func(conn PgConn) {
				RaisePgResult(conn.Exec(ctx, `ALTER TABLE `+relation.table+` DISABLE TRIGGER `+relation.trigger))
			})
			if err := RequireProviderPayoutSchema(ctx); err == nil {
				t.Fatal("disabled payout guard reported ready", relation.trigger)
			}
			Db(ctx, func(conn PgConn) {
				RaisePgResult(conn.Exec(ctx, `ALTER TABLE `+relation.table+` ENABLE TRIGGER `+relation.trigger))
			})
		}
		Db(ctx, func(conn PgConn) {
			RaisePgResult(conn.Exec(ctx, `CREATE OR REPLACE FUNCTION guard_account_payment_submission_basis() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN RETURN NEW; END$$`))
		})
		if err := RequireProviderPayoutSchema(ctx); err == nil {
			t.Fatal("replaced no-op financial guard reported ready")
		}
	})
}
