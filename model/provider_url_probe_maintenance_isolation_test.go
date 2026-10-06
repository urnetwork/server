// A provider waiting for exact completion expiry cannot block unrelated work.
package model

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// The pigeonhole-sized fixture includes two clients in the same logical shard.
// Other clients stay paced into the future and cannot hide missing admission.
func testingUrlMaintenancePeers(t testing.TB, now time.Time, shardCount int) (server.Id, server.Id, int) {
	t.Helper()
	testingUrlCompletionClients(t, now.Add(-5*time.Hour), shardCount+1)
	clientIdTs := map[int][]server.Id{}
	server.Db(t.Context(), func(conn server.PgConn) {
		rows, err := conn.Query(t.Context(), `SELECT client_id,slot_id%$1
			FROM provider_egress_probe_cycle ORDER BY client_id`, shardCount)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				var clientId server.Id
				var shardIndex int
				server.Raise(rows.Scan(&clientId, &shardIndex))
				clientIdTs[shardIndex] = append(clientIdTs[shardIndex], clientId)
			}
		})
	})
	for shardIndex := range shardCount {
		if clients := clientIdTs[shardIndex]; len(clients) >= 2 {
			return clients[0], clients[1], shardIndex
		}
	}
	t.Fatal("fixture has no same-shard pair")
	return server.Id{}, server.Id{}, 0
}

// A held cycle lock deterministically keeps one expired receipt pending. The
// other due provider must still receive its own exact-count, server-issued claim.
func TestUrlCompletedPriorityLockedExpiryDoesNotBlockIndependentProvider(t *testing.T) {
	for _, scenario := range []struct {
		shardCount int
		wasReady   bool
	}{{1, false}, {1, true}, {3, false}, {3, true}} {
		t.Run(fmt.Sprintf("shards%d/expired_ready%t", scenario.shardCount, scenario.wasReady), func(t *testing.T) {
			server.DefaultTestEnv().Run(t, func(t testing.TB) {
				ctx := t.Context()
				now := server.NowUtc().Truncate(time.Microsecond)
				testingUrlCompletionPriority(t, now.Add(-8*time.Hour))
				blockedId, independentId, shardIndex := testingUrlMaintenancePeers(t, now, scenario.shardCount)
				first := testingClaimUrlCompletion(t, ctx, blockedId, now.Add(-5*time.Hour))
				testingCompleteUrlClaim(t, ctx, first, now.Add(-5*time.Hour+time.Second), "tunnel_failed")
				server.Tx(ctx, func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(ctx, `UPDATE provider_egress_probe_cycle SET next_attempt_at=
						CASE WHEN client_id=$1 THEN $3::timestamp-interval '1 minute'
						WHEN client_id=$2 THEN $3::timestamp-interval '30 seconds'
						ELSE $3::timestamp+interval '1 day' END`, blockedId, independentId, now))
					// The clock can expire a previously promoted count without a
					// pacing write that would invalidate its readiness flag.
					server.RaisePgResult(tx.Exec(ctx, `UPDATE provider_egress_probe_cycle
						SET completed_priority_ready=$2 WHERE client_id=$1`, blockedId, scenario.wasReady))
				})
				var result ProviderUrlProbeDueResult
				server.Tx(ctx, func(lockTx server.PgTx) {
					var locked server.Id
					server.Raise(lockTx.QueryRow(ctx, `SELECT client_id FROM provider_egress_probe_cycle
						WHERE client_id=$1 FOR UPDATE`, blockedId).Scan(&locked))
					claimCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
					defer cancel()
					result = ClaimProviderUrlProbeDueWithStatus(claimCtx, now, 1, shardIndex, scenario.shardCount)
					if result.PriorityMaintenancePending || len(result.Providers) != 1 || result.Providers[0].ClientId != independentId {
						t.Fatalf("locked expired provider blocked independent due work: %+v", result)
					}
					provider := result.Providers[0]
					if !result.CompletedRunPriorityReady || provider.CompletedRunCount == nil || *provider.CompletedRunCount != 0 ||
						provider.ClaimOrdinal != 1 || !provider.ClaimedAt.Equal(now) || provider.RunsNeeded != ProviderUrlProbeRunTarget {
						t.Fatalf("independent provider lost exact priority or claim/quota authority: %+v", provider)
					}
					blocked := testingReadUrlCompletionCycle(t, ctx, blockedId)
					if blocked.count != 1 || blocked.ordinal != first.ClaimOrdinal || blocked.expiry == nil || !blocked.expiry.Before(now) {
						t.Fatalf("locked provider's expired receipt was bypassed or changed: %+v", blocked)
					}
				})
				// Once the conflicting owner releases, bounded maintenance admits
				// that provider with its retired count and preserves the first lease.
				recovered := ClaimProviderUrlProbeDueWithStatus(ctx, now, 2, shardIndex, scenario.shardCount)
				if recovered.PriorityMaintenancePending || len(recovered.Providers) != 1 || recovered.Providers[0].ClientId != blockedId ||
					recovered.Providers[0].CompletedRunCount == nil || *recovered.Providers[0].CompletedRunCount != 0 ||
					recovered.Providers[0].ClaimOrdinal != first.ClaimOrdinal+1 {
					t.Fatalf("released provider did not regain exact admission: %+v", recovered)
				}
				for _, clientId := range []server.Id{blockedId, independentId} {
					cycle := testingReadUrlCompletionCycle(t, ctx, clientId)
					if cycle.history != 0 || cycle.successes != 0 || cycle.errors != 0 || !cycle.next.Equal(now.Add(ProviderEgressProbeAttemptBackoff)) {
						t.Fatalf("maintenance admission fabricated measurement credit or changed claim lease: %+v", cycle)
					}
				}
			})
		})
	}
}
