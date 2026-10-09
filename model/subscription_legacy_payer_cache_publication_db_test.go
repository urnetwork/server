// The real modern dispatcher must supply the same full-index publisher used by
// the cold-cache controls. Setup invalidates proof but never primes the cache.
package model

import (
	"crypto/sha256"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// Full registration and its subsequent due consumer run through production
// entry points while the intent remains under its ordinary financial owner.
func TestLegacyPayerDispatcherColdRegistrationPublishesProof(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		fixture, id := legacySettlementTestIntent(t, ctx)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE legacy_settlement_intent
 SET payer_network_id=NULL WHERE contract_id=$1`, id))
		})
		resource, err := server.Vault.SimpleResource(server.DefaultPgVaultResourceName)
		server.Raise(err)
		raw, err := resource.BytesBoundedE(ctx, 16*1024)
		server.Raise(err)
		identity := sha256.Sum256(raw)
		func() {
			legacySettlementPayerIndexes.stateLock.Lock()
			defer legacySettlementPayerIndexes.stateLock.Unlock()
			if legacySettlementPayerIndexes.refreshing {
				t.Fatal("cold dispatcher control requires no outstanding catalog owner")
			}
			legacySettlementPayerIndexes.expires = time.Time{}
			legacySettlementPayerIndexes.ready = false
		}()
		dispatched, readiness, err := DispatchLegacySettlementPayersWithReadiness(ctx, int(id[15])%LegacySettlementShardCount, nil, nil)
		if err != nil || readiness == nil || readiness.Outcome != "ready" || readiness.Cached || dispatched.Registered != 1 || dispatched.RegistrationFailed {
			t.Fatal("cold modern dispatcher did not register its exact durable intent", dispatched, readiness, err)
		}
		if !legacySettlementPayerIndexes.readyObservation(ctx, identity, time.Now()) {
			t.Fatal("actual registration callback did not publish its full-index proof")
		}
		reused := legacySettlementPayerDueIndexObservation(ctx)
		if reused.Outcome != "ready" || !reused.Cached {
			t.Fatal("actual due consumer repeated the catalog after modern registration", reused)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var exact bool
			server.Raise(conn.QueryRow(ctx, `SELECT payer_network_id=$2 AND failure_code='none'
 FROM legacy_settlement_intent WHERE contract_id=$1`, id, fixture.sourceNetworkId).Scan(&exact))
			if !exact {
				t.Fatal("schema proof publication changed the registered financial obligation")
			}
		})
		requireLegacySettlementTestState(t, ctx, fixture, id, true, false, 1000, 100)
	})
}
