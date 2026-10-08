// Capability observations use the caller's uncommitted database state.
package server

import (
	"errors"
	"testing"
)

// An uncommitted guard change must be visible on the allocation owner.
func TestProviderPayoutSchemaInTxObservesCallerGuardState(t *testing.T) {
	env := DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		rollbackErr := errors.New("synthetic payout schema rollback")
		panicValue := HandleError(func() {
			Tx(ctx, func(tx PgTx) {
				RaisePgResult(tx.Exec(ctx, `ALTER TABLE account_payment_bonus DISABLE TRIGGER account_payment_bonus_guard`))
				if err := RequireProviderPayoutSchemaInTx(ctx, tx); err == nil {
					t.Fatal("schema check missed its caller's uncommitted guard change")
				}
				panic(rollbackErr)
			})
		})
		err, ok := panicValue.(error)
		if !ok || !errors.Is(err, rollbackErr) {
			t.Fatal("fixture did not roll back its guard change", panicValue)
		}
		if err := RequireProviderPayoutSchema(ctx); err != nil {
			t.Fatal("schema guard change escaped rollback", err)
		}
	})
}

// Preparing authority inside the caller is visible there and disappears on rollback.
func TestProviderPayoutEarningPolicyInTxObservesCallerBoundary(t *testing.T) {
	env := DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		data, policy := boundaryTestPolicy(t)
		defer Config.PushSimpleResource("sn.yml", data)()
		identity, digest, err := providerPayoutEarningIdentity(policy)
		if err != nil {
			t.Fatal(err)
		}
		rollbackErr := errors.New("synthetic payout boundary rollback")
		panicValue := HandleError(func() {
			Tx(ctx, func(tx PgTx) {
				RaisePgResult(tx.Exec(ctx, `INSERT INTO provider_payout_boundary
					(singleton,earning_identity,identity_sha256,initial_config_sha256) VALUES(true,$1,$2,$3)`,
					identity, digest, policy.ConfigSha256))
				observed, err := LoadProviderPayoutEarningPolicyInTx(ctx, tx)
				if err != nil || observed == nil || observed.ConfigSha256 != policy.ConfigSha256 {
					t.Fatal("policy check missed its caller's uncommitted earning authority", observed, err)
				}
				panic(rollbackErr)
			})
		})
		err, ok := panicValue.(error)
		if !ok || !errors.Is(err, rollbackErr) {
			t.Fatal("fixture did not roll back its boundary", panicValue)
		}
		if _, err := LoadProviderPayoutEarningPolicy(ctx); !errors.Is(err, ErrProviderEarningBoundaryUnprepared) {
			t.Fatal("earning authority escaped rollback", err)
		}
	})
}
