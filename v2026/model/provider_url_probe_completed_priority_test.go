// Priority never bypasses the existing quota, security, or logical slot gates.
package model

import (
	"fmt"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// A locked expiry is still pending. A real first receipt waits for that same
// cycle lock, then expiry retires only the old turn, retaining the fresh turn.
func TestUrlCompletedPriorityLockedMaintenanceAndReceipt(t *testing.T) {
	for _, shardCount := range []int{1, 3} {
		t.Run(fmt.Sprintf("shards%d", shardCount), func(t *testing.T) {
			server.DefaultTestEnv().Run(t, func(t testing.TB) {
				ctx := t.Context()
				now := server.NowUtc().Truncate(time.Microsecond)
				testingUrlCompletionPriority(t, now.Add(-8*time.Hour))
				clientId := testingUrlCompletionClients(t, now.Add(-5*time.Hour), 1)[0]
				first := ClaimProviderUrlProbeDue(ctx, now.Add(-5*time.Hour), 1, 0, 1)[0]
				testingCompleteUrlClaim(t, ctx, first, now.Add(-5*time.Hour+time.Second), "tunnel_failed")
				var shardIndex int
				server.Tx(ctx, func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(ctx, `UPDATE provider_egress_probe_cycle SET claim_ordinal=2 WHERE client_id=$1`, clientId))
					server.RaisePgResult(tx.Exec(ctx, `INSERT INTO provider_url_probe_run(client_id,claim_ordinal,claimed_at) VALUES($1,2,$2)`, clientId, now))
					server.Raise(tx.QueryRow(ctx, `SELECT slot_id%$2 FROM provider_egress_probe_cycle WHERE client_id=$1`, clientId, shardCount).Scan(&shardIndex))
				})
				completed := make(chan error, 1)
				server.Tx(ctx, func(lockTx server.PgTx) {
					var locked server.Id
					server.Raise(lockTx.QueryRow(ctx, `SELECT client_id FROM provider_egress_probe_cycle WHERE client_id=$1 FOR UPDATE`, clientId).Scan(&locked))
					go func() {
						_, err := CompleteProviderUrlProbeRun(ctx, ProviderUrlProbeCompletion{
							ClientId: clientId, ClaimOrdinal: 2, CompletedAt: now, ProbeFailure: "health_not_run",
						}, now)
						completed <- err
					}()
					result := ClaimProviderUrlProbeDueWithStatus(ctx, now, 1, shardIndex, shardCount)
					if !result.PriorityMaintenancePending || len(result.Providers) != 0 {
						t.Fatalf("locked expiry was reported as finished: %+v", result)
					}
				})
				select {
				case err := <-completed:
					if err != nil {
						t.Fatal(err)
					}
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				server.Tx(ctx, func(tx server.PgTx) {
					var changed int
					server.Raise(tx.QueryRow(ctx, providerUrlProbeExpirySql(shardIndex, shardCount), now, providerUrlProbeExpiryClients).Scan(&changed))
					if changed != 1 {
						t.Fatalf("expiry changed %d providers", changed)
					}
				})
				cycle := testingReadUrlCompletionCycle(t, ctx, clientId)
				if cycle.count != 1 || cycle.ordinal != 2 || cycle.expiry == nil || !cycle.expiry.Equal(now.Add(4*time.Hour)) {
					t.Fatalf("expiry lost the concurrent fresh receipt: %+v", cycle)
				}
				server.Db(ctx, func(conn server.PgConn) {
					var counted, retired int
					server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*) FILTER (WHERE counted),COUNT(*) FILTER (WHERE NOT counted)
						FROM provider_url_probe_run WHERE client_id=$1`, clientId).Scan(&counted, &retired))
					if counted != 1 || retired != 1 {
						t.Fatalf("receipt TID retirement changed identities: counted=%d retired=%d", counted, retired)
					}
				})
			})
		})
	}
}

// Completion priority preserves measured quota, early renewal, and unresolved security work.
func TestUrlCompletedPriorityPreservesQuotaSecurityAndFreshness(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc().Truncate(time.Microsecond)
		testingUrlCompletionPriority(t, now.Add(-12*time.Hour))
		startedAt := now.Add(-9 * time.Hour)
		clients := testingUrlCompletionClients(t, startedAt, 4)
		for index, clientId := range clients[:3] {
			for success := range ProviderUrlProbeRunTarget {
				at := now.Add(-time.Hour + time.Duration(success)*time.Minute)
				if index == 2 {
					at = startedAt.Add(time.Duration(success) * time.Minute)
				}
				testingSetUrlProbeHealth(ctx, &ProviderEgressHealth{RunId: server.NewId(), ClientId: clientId,
					CycleStartedAt: startedAt, MeasuredAt: at, OKCount: 1, Total: 1})
			}
		}
		// Ten fresh successes do not excuse an unresolved per-URL TLS finding.
		SetProviderEgressHealth(ctx, &ProviderEgressHealth{RunId: server.NewId(), ClientId: clients[1],
			CycleStartedAt: startedAt, MeasuredAt: now.Add(-time.Minute), Total: 1, TLSAuthenticationFailure: true,
			UrlProbeEvidence: fp2TestUrlEvidence(now.Add(-time.Minute), "https://affected.example/", true)})
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE provider_egress_probe_cycle SET next_attempt_at=$1`, now))
			// A stale positive queue hint still needs the authoritative risk check.
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client_location_reliability SET arin_risk=true WHERE client_id=$1`, clients[3]))
		})
		result := ClaimProviderUrlProbeDueWithStatus(ctx, now, 4, 0, 1)
		if result.PriorityMaintenancePending || len(result.Providers) != 2 {
			t.Fatalf("priority changed quota/risk admission: %+v", result)
		}
		seen := map[server.Id]ProviderUrlProbeDue{}
		for _, due := range result.Providers {
			seen[due.ClientId] = due
		}
		security, securityOk := seen[clients[1]]
		expired, expiredOk := seen[clients[2]]
		if !securityOk || security.RunsNeeded != 0 || len(security.SecurityDestinations) != 1 ||
			!expiredOk || expired.RunsNeeded != ProviderUrlProbeRunTarget {
			t.Fatalf("security exception or expired success window lost: %+v", result.Providers)
		}
		full := testingReadUrlCompletionCycle(t, ctx, clients[0])
		if full.count != 0 || full.ordinal != 0 || full.successes != ProviderUrlProbeRunTarget || full.errors != 0 || full.history != ProviderUrlProbeRunTarget {
			t.Fatalf("quota-full provider acquired a completed turn or changed measured quota: %+v", full)
		}
		renewalAt := now.Add(-time.Hour + ProviderEgressProbeRefreshAge - ProviderUrlProbeRenewalHeadroom)
		if !full.next.Equal(renewalAt) {
			t.Fatalf("quota-full provider lost oldest-measurement renewal pacing: got %s, want %s", full.next, renewalAt)
		}
		for _, due := range result.Providers {
			testingCompleteUrlClaim(t, ctx, due, now.Add(time.Second), "tunnel_failed")
			cycle := testingReadUrlCompletionCycle(t, ctx, due.ClientId)
			if cycle.count != 1 || cycle.successes != ProviderUrlProbeRunTarget-due.RunsNeeded {
				t.Fatalf("completion count changed independent URL quota: %+v", cycle)
			}
		}
		if !GetAllProviderEgressTLSAuthenticationFailedClientIds(ctx)[clients[1]] {
			t.Fatal("completion receipt cleared a TLS security finding")
		}
	})
}

// Non-power-of-two reassignment moves slot ownership, not counters or active
// leases. Within each owned shard, count then oldest due then ID is exact.
func TestUrlCompletedPrioritySlotRedistribution(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc().Truncate(time.Microsecond)
		testingUrlCompletionPriority(t, now.Add(-8*time.Hour))
		clients := testingUrlCompletionClients(t, now, 64)
		server.Tx(ctx, func(tx server.PgTx) {
			// No history lookup is needed to sort this materialized fixture.
			server.RaisePgResult(tx.Exec(ctx, `UPDATE provider_egress_probe_cycle SET
				completed_run_count=slot_id%7,next_attempt_at=$1::timestamp-(slot_id%17)*interval '1 second'`, now))
		})
		seen := map[server.Id]bool{}
		for shard := range 3 {
			want := []server.Id{}
			server.Db(ctx, func(conn server.PgConn) {
				rows, err := conn.Query(ctx, `SELECT client_id FROM provider_egress_probe_cycle WHERE slot_id%3=$1
					ORDER BY completed_run_count,next_attempt_at,client_id`, shard)
				server.WithPgResult(rows, err, func() {
					for rows.Next() {
						var clientId server.Id
						server.Raise(rows.Scan(&clientId))
						want = append(want, clientId)
					}
				})
			})
			due := ClaimProviderUrlProbeDue(ctx, now, 64, shard, 3)
			if len(due) != len(want) {
				t.Fatalf("shard %d/3 lost owned work: got=%d want=%d", shard, len(due), len(want))
			}
			for index, provider := range due {
				if provider.ClientId != want[index] || seen[provider.ClientId] || provider.ClaimOrdinal != 1 {
					t.Fatalf("shard ordering or durable identity changed: shard=%d provider=%+v", shard, provider)
				}
				seen[provider.ClientId] = true
			}
		}
		if len(seen) != len(clients) {
			t.Fatalf("logical slots omitted %d providers", len(clients)-len(seen))
		}
		for shard := range 5 {
			if due := ClaimProviderUrlProbeDue(ctx, now, 64, shard, 5); len(due) != 0 {
				t.Fatalf("repartitioning reclaimed active leases: shard=%d due=%+v", shard, due)
			}
		}
		leased := now.Add(ProviderEgressProbeAttemptBackoff)
		for shard := range 5 {
			for _, due := range ClaimProviderUrlProbeDue(ctx, leased, 64, shard, 5) {
				if !seen[due.ClientId] || due.ClaimOrdinal != 2 || due.CompletedRunCount == nil {
					t.Fatalf("replacement worker reset claim identity/count: %+v", due)
				}
				delete(seen, due.ClientId)
			}
		}
		if len(seen) != 0 {
			t.Fatalf("new shard geometry stranded expired reservations: %v", seen)
		}
	})
}

// A pathological single-provider history cannot turn a priority request into
// unbounded expiry. Its unfinished maintenance remains explicit, never zero.
func TestUrlCompletedPriorityExpiryBacklogRemainsExplicit(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc().Truncate(time.Microsecond)
		testingUrlCompletionPriority(t, now.Add(-8*time.Hour))
		clientId := testingUrlCompletionClients(t, now, 1)[0]
		count := providerUrlProbeExpiryRunsPerClient + 1
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO provider_url_probe_run
				(client_id,claim_ordinal,claimed_at,completed_at,received_at,counted)
				SELECT $1,i,$2::timestamp-interval '5 hours',$2::timestamp-interval '5 hours',$2::timestamp-interval '5 hours',true
				FROM generate_series(1,$3::integer) AS i`, clientId, now, count))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE provider_egress_probe_cycle SET claim_ordinal=$2,
				completed_run_count=$2,completed_next_expiry_at=$3::timestamp-interval '1 hour' WHERE client_id=$1`, clientId, count, now))
		})
		first := ClaimProviderUrlProbeDueWithStatus(ctx, now, 1, 0, 1)
		remaining := testingReadUrlCompletionCycle(t, ctx, clientId)
		if !first.PriorityMaintenancePending || len(first.Providers) != 0 || remaining.count != 1 || remaining.ordinal != int64(count) {
			t.Fatalf("expiry page became unbounded or silently empty: result=%+v cycle=%+v", first, remaining)
		}
		second := ClaimProviderUrlProbeDueWithStatus(ctx, now, 1, 0, 1)
		if second.PriorityMaintenancePending || len(second.Providers) != 1 || *second.Providers[0].CompletedRunCount != 0 || second.Providers[0].ClaimOrdinal != int64(count+1) {
			t.Fatalf("final expiry page did not restore exact admission: %+v", second)
		}
	})
}

// Retention is bounded and cannot turn a retired claim into a new completion.
// An unacknowledged issued turn also expires without pretending it completed.
func TestUrlCompletedReceiptRetentionRejectsResurrection(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc().Truncate(time.Microsecond)
		clientId := testingUrlCompletionClients(t, now, 1)[0]
		issuedAt := now.Add(-providerUrlProbeRunRetention - time.Hour)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE provider_egress_probe_cycle SET claim_ordinal=2 WHERE client_id=$1`, clientId))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO provider_url_probe_run(client_id,claim_ordinal,claimed_at)
				VALUES($1,1,$2),($1,2,$2)`, clientId, issuedAt))
		})
		if removed := RemoveExpiredProviderUrlProbeRuns(ctx, now, 1); removed != 1 {
			t.Fatalf("bounded retention removed %d, want 1", removed)
		}
		if receipt, err := CompleteProviderUrlProbeRun(ctx, ProviderUrlProbeCompletion{
			ClientId: clientId, ClaimOrdinal: 1, CompletedAt: now, ProbeFailure: "tunnel_failed", AllowPacing: true,
		}, now); err == nil || receipt != nil {
			t.Fatal("retired claim was resurrected")
		}
		if receipt, err := CompleteProviderUrlProbeRun(ctx, ProviderUrlProbeCompletion{
			ClientId: clientId, ClaimOrdinal: 2, CompletedAt: now, ProbeFailure: "tunnel_failed", AllowPacing: true,
		}, now); err == nil || receipt != nil {
			t.Fatal("ancient never-acknowledged claim acquired a current completion")
		}
		cycle := testingReadUrlCompletionCycle(t, ctx, clientId)
		if cycle.count != 0 || cycle.ordinal != 2 || cycle.history != 0 {
			t.Fatalf("retention fabricated a completion or reset identity: %+v", cycle)
		}
	})
}
