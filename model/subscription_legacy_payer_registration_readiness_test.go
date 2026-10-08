// Registration reuses exact live schema proof without repeating a refused read.
package model

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// The real missing-key registration must survive a transient second probe
// refusal. Misses retain the same chronological cutoff and cancel semantics.
func TestLegacyPayerRegistrationReusesOnlyLiveFullIndexProof(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		fixture, id := legacySettlementTestIntent(t, ctx)
		shard := int(id[15]) % LegacySettlementShardCount
		now := server.NowUtc().Truncate(time.Microsecond)
		due := now.Add(-58 * time.Hour)
		after := &LegacySettlementCursor{NextAttemptTime: now.Add(-120 * time.Hour),
			ContractId: server.NewId(), PassEndTime: now.Add(-84 * time.Hour)}
		resource, err := server.Vault.SimpleResource(server.DefaultPgVaultResourceName)
		server.Raise(err)
		raw, err := resource.BytesBoundedE(ctx, 16*1024)
		server.Raise(err)
		identity := sha256.Sum256(raw)
		other := sha256.Sum256([]byte("synthetic different database resource"))
		instant := time.Unix(100, 0)
		cache := &legacySettlementPayerIndexCache{}
		checks := 0
		if !cache.load(ctx, identity, func() time.Time { return instant }, func(readCtx context.Context) (bool, error) {
			checks++
			return readLegacySettlementPayerIndexes(readCtx)
		}) || checks != 1 {
			t.Fatal("fixture did not establish one exact full-index proof")
		}
		for _, test := range []struct {
			name       string
			identity   [sha256.Size]byte
			ready      bool
			at         time.Time
			canceled   bool
			freshReady bool
			want       int
			readCalls  int
		}{
			{name: "uncached transient refusal", identity: identity, at: instant, readCalls: 1},
			{name: "live matching proof", identity: identity, ready: true, at: instant, want: 1},
			{name: "exact expiry", identity: identity, ready: true, at: instant.Add(legacySettlementPayerIndexCacheLifetime), readCalls: 1},
			{name: "different resource", identity: other, ready: true, at: instant, readCalls: 1},
			{name: "negative proof", identity: identity, at: instant, readCalls: 1},
			{name: "canceled parent", identity: identity, ready: true, at: instant, canceled: true, readCalls: 1},
			{name: "fresh check after miss", identity: identity, at: instant, freshReady: true, want: 1, readCalls: 1},
		} {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE legacy_settlement_intent
 SET payer_network_id=NULL,next_attempt_time=$2 WHERE contract_id=$1`, id, due))
			})
			cache.ready = test.ready
			deadline := cache.expires
			request, cancel := context.WithCancel(ctx)
			if test.canceled {
				cancel()
			}
			calls := 0
			var next *LegacySettlementCursor
			registered := 0
			var registrationErr error
			server.HandleError(func() {
				next, registered = registerLegacySettlementPayerDispatchPageWithReadiness(request, shard, after,
					func(readCtx context.Context) bool {
						return cache.readyObservation(readCtx, test.identity, test.at)
					}, func(readCtx context.Context) (bool, error) {
						calls++
						until, ok := readCtx.Deadline()
						if !ok || until.After(time.Now().Add(legacySettlementPayerIndexBudget)) {
							t.Fatal("registration lost its original bounded catalog context")
						}
						if readCtx.Err() != nil {
							return false, readCtx.Err()
						}
						if test.freshReady {
							return readLegacySettlementPayerIndexes(readCtx)
						}
						return false, context.DeadlineExceeded
					})
			}, func(err error) { registrationErr = err })
			cancel()
			if calls != test.readCalls || registered != test.want || cache.expires != deadline || checks != 1 {
				t.Fatalf("%s: registration/probe/expiry changed: calls=%d registered=%d", test.name, calls, registered)
			}
			if test.canceled {
				if !errors.Is(registrationErr, context.Canceled) {
					t.Fatalf("%s: parent cancellation did not remain an error: %v", test.name, registrationErr)
				}
			} else if registrationErr != nil || (test.want == 1 && next != after) || (test.want == 0 && next != nil) {
				t.Fatalf("%s: registration changed cursor custody or fallback: %v", test.name, registrationErr)
			}
			server.Db(ctx, func(conn server.PgConn) {
				var registeredExactly, missing, unchanged bool
				server.Raise(conn.QueryRow(ctx, `SELECT COALESCE(payer_network_id=$2,false),payer_network_id IS NULL,
 next_attempt_time=$3 AND failure_code='none' AND outcome='settled'
 FROM legacy_settlement_intent WHERE contract_id=$1`, id, fixture.sourceNetworkId, due).Scan(&registeredExactly, &missing, &unchanged))
				if registeredExactly != (test.want == 1) || missing != (test.want == 0) || !unchanged {
					t.Fatalf("%s: wrong payer registration or changed intent eligibility", test.name)
				}
			})
			requireLegacySettlementTestState(t, ctx, fixture, id, true, false, 1000, 100)
		}
	})
}
