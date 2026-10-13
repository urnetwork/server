// The prober allocator's complete fallback locks within the grant set its
// allocation admitted, never a set rediscovered at a later clock.
package model

import (
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// The allocation admits the ownership of every grant current at its clock
// before any speculative attempt (403b9c55), and a transaction admits its set
// once: extra keys fail closed. A grant that becomes current while the
// speculative attempts run is outside that set. Explicit clocks place the
// late grant's start thirty seconds after the allocation's clock and thirty
// seconds before the fallback runs, so no elapsed time decides the order.
func TestDynamicProberFallbackLocksOnlyAdmittedGrants(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		clients := newEscrowSelectionTestClients(t, ctx)
		setDynamicProberIdentityForTest(t, ctx, clients)
		allocationTime := server.NowUtc().Add(-time.Minute)
		// The one admitted grant is fully reserved, so its speculative attempt
		// is rejected and the allocation reaches the complete fallback.
		reserved := addDynamicProberGrantForTest(ctx, clients.payerNetworkId,
			allocationTime.Add(-time.Hour), allocationTime.Add(2*time.Hour), 1024)
		clients.reserve(ctx, reserved.BalanceId, 1024)
		late := addDynamicProberGrantForTest(ctx, clients.payerNetworkId,
			allocationTime.Add(30*time.Second), allocationTime.Add(2*time.Hour), 1024)
		var balances []*escrowTransferBalance
		err := server.HandleError(func() {
			server.Tx(ctx, func(tx server.PgTx) {
				balances = loadTransferEscrowBalances(ctx, tx, clients.payerNetworkId, clients.payerId, allocationTime, 1024)
			}, server.TxReadCommitted, server.OptNoRetry())
		})
		if err != nil {
			t.Fatalf("fallback rediscovered a grant outside its admitted set: %v", err)
		}
		// The late grant funds the next allocation, whose clock includes it.
		if len(balances) != 1 || balances[0].balanceId != reserved.BalanceId || balances[0].balanceByteCount != 0 {
			t.Fatalf("fallback did not return exactly the admitted, fully reserved grant: %d grants", len(balances))
		}
		escrow, err := createTransferEscrow(ctx, clients.payerNetworkId, clients.payerId,
			clients.providerNetworkId, clients.providerId, 1024)
		if err != nil || escrow == nil || len(escrow.Balances) != 1 || escrow.Balances[0].BalanceId != late.BalanceId {
			t.Fatalf("next allocation did not fund from the late grant: %v", err)
		}
	})
}
