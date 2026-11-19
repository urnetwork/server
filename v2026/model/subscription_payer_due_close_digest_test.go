package model

import (
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// The close-due digest names the payer's due set at a bound: it repeats while
// the set stands, and changes when a contract of the set is retired or the
// bound moves.
func TestDigestPayerDueContractsNamesTheDueSet(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := WithProviderWorkSessionSource(t.Context(), nil)
		now := server.NowUtc().Truncate(time.Second)
		mintedAt := now.Add(-30 * time.Hour)
		deadline := mintedAt.Add(DefaultContractExpiration)
		f := newPayerDueCloseFixture(t, ctx, "synthetic-digest")
		ids := f.addContracts(ctx, f.endedBalanceId, mintedAt, 3, deadline, false)
		f.addContracts(ctx, f.currentBalanceId, now.Add(-10*time.Minute), 1, now.Add(50*time.Minute), false)

		count, digest, err := DigestPayerDueContracts(ctx, f.payerNetworkId, now)
		if err != nil || count != len(ids) || len(digest) != 16 {
			t.Fatal("the digest did not count the due set", count, digest, err)
		}
		if _, again, err := DigestPayerDueContracts(ctx, f.payerNetworkId, now); err != nil || again != digest {
			t.Fatal("the digest of an unchanged set changed", again, digest, err)
		}
		if _, moved, err := DigestPayerDueContracts(ctx, f.payerNetworkId, now.Add(-time.Second)); err != nil || moved == digest {
			t.Fatal("the digest did not bind its bound", moved, err)
		}
		if _, err := ReconcileContractAtDeadline(ctx, ids[0], deadline); err != nil {
			t.Fatal(err)
		}
		count, changed, err := DigestPayerDueContracts(ctx, f.payerNetworkId, now)
		if err != nil || count != len(ids)-1 || changed == digest {
			t.Fatal("the digest did not change when a contract of the set was retired", count, changed, err)
		}
	})
}
