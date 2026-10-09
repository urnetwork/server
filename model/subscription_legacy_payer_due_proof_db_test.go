// Actual payer execution publishes due-only authority even when optional
// registration cannot prove its separate missing-key index.
package model

import (
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

func TestLegacyPayerDueProofActualEntryKeepsFullIndexAuthoritySeparate(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		fixture, id := legacySettlementTestIntent(t, ctx)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DROP INDEX legacy_settlement_intent_payer_missing`))
		})
		for _, cache := range []*legacySettlementPayerIndexCache{&legacySettlementPayerIndexes, &legacySettlementPayerDueIndexes} {
			func() {
				cache.stateLock.Lock()
				defer cache.stateLock.Unlock()
				if cache.refreshing {
					t.Fatal("cold due entry requires no outstanding catalog owner")
				}
				cache.ready, cache.expires = false, time.Time{}
			}()
		}
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		result, err := ApplyLegacyPayerSettlements(&LegacyPayerSettlementArgs{Private: true, PayerNetworkId: fixture.sourceNetworkId}, owner)
		if err != nil || result.Completed != 1 {
			t.Fatal("actual payer failed without the optional missing-key index", result, err)
		}
		identity, err := legacySettlementPayerIndexResourceIdentity(ctx)
		server.Raise(err)
		if !legacySettlementPayerDueIndexes.readyObservation(ctx, identity, time.Now()) || legacySettlementPayerIndexes.readyObservation(ctx, identity, time.Now()) {
			t.Fatal("actual due entry failed to publish its separate authority")
		}
		cached := legacySettlementPayerDueIndexObservation(ctx)
		if cached.Outcome != "ready" || !cached.Cached || legacySettlementPayerIndexesReady(ctx) {
			t.Fatal("due proof was not reused or authorized the missing registration index", cached)
		}
		requireLegacySettlementTestState(t, ctx, fixture, id, false, true, 989, 0)
		requireLegacyProviderDurability(t, ctx, fixture, id, 11)
	})
}
