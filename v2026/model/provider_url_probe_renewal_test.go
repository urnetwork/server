package model

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

func testingRenewalHistory(t testing.TB, now time.Time, expiries []time.Time) server.Id {
	t.Helper()
	client := testingUrlCompletionClients(t, now.Add(-8*time.Hour), 1)[0]
	server.Tx(t.Context(), func(tx server.PgTx) {
		for _, expiry := range expiries {
			server.RaisePgResult(tx.Exec(t.Context(), `INSERT INTO provider_egress_health_history
 (run_id,client_id,measured_at,ok_count,total_count,class_results,tls_authentication_failure,url_probe,url_probe_policy_version)
 VALUES($1,$2,$3,0,1,'{}',false,true,1)`, server.NewId(), client, expiry.Add(-ProviderEgressProbeRefreshAge)))
		}
		server.RaisePgResult(tx.Exec(t.Context(), `UPDATE provider_egress_probe_cycle SET next_attempt_at=$2 WHERE client_id=$1`, client, now))
	})
	return client
}

func TestUrlProbeRenewalProjectsLatestTenWithoutSurplusLoop(t *testing.T) {
	for _, count := range []int{11, 12} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			server.DefaultTestEnv().Run(t, func(t testing.TB) {
				now := server.NowUtc().Truncate(time.Microsecond)
				expiries := make([]time.Time, count)
				for i := range expiries {
					if i < count-10 {
						expiries[i] = now.Add(time.Duration(i+1) * time.Minute)
					} else {
						expiries[i] = now.Add(time.Duration(i-(count-10))*20*time.Minute + 10*time.Minute)
					}
				}
				// All eleven/twelve rows are current, but the earliest extras
				// can expire while the latest ten still cover the horizon.
				client := testingRenewalHistory(t, now, expiries)
				if due := ClaimProviderUrlProbeDue(t.Context(), now, 8, 0, 1); len(due) != 0 {
					t.Fatal("surplus history caused an unnecessary replacement")
				}
				cycle := testingReadUrlCompletionCycle(t, t.Context(), client)
				if !cycle.next.Equal(now.Add(4*time.Minute)) || cycle.ordinal != 0 {
					t.Fatal("latest ten did not set the next exact replacement horizon")
				}
			})
		})
	}
}

func TestUrlProbeRenewalClusterIsBoundedByExpiringMeasurements(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc().Truncate(time.Microsecond)
		expires := now.Add(ProviderUrlProbeRenewalHeadroom)
		expiries := make([]time.Time, 10)
		for i := range expiries {
			expiries[i] = expires
		}
		client := testingRenewalHistory(t, now, expiries)
		at := now
		for i := range 10 {
			due := ClaimProviderUrlProbeDue(ctx, at, 8, 0, 1)
			if len(due) != 1 || due[0].RunsNeeded != 0 {
				t.Fatal("one expiring measurement replacement was not admitted while coverage remained full")
			}
			at = at.Add(25 * time.Second)
			testingSetUrlProbeHealth(ctx, &ProviderEgressHealth{RunId: server.NewId(), ClientId: client, CycleStartedAt: due[0].CycleStartedAt, MeasuredAt: at, OKCount: i % 2, Total: 1})
			testingCompleteUrlClaim(t, ctx, due[0], at.Add(time.Microsecond), "")
		}
		if due := ClaimProviderUrlProbeDue(ctx, at.Add(time.Second), 8, 0, 1); len(due) != 0 {
			t.Fatal("replacement continued after all ten expiring measurements were renewed")
		}
		if cycle := testingReadUrlCompletionCycle(t, ctx, client); cycle.history != 20 || cycle.ordinal != 10 {
			t.Fatal("cluster replacement emitted surplus work")
		}
		if fleet := testingGetUrlProbeFleet(t, ctx, expires); fleet.QuotaComplete != 1 || fleet.RunsNeeded != 0 {
			t.Fatal("bounded accepted cluster renewal lost coverage")
		}
	})
}

func TestUrlProbeRenewalSetupFailureAndConcurrentClaim(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc().Truncate(time.Microsecond)
		expiries := make([]time.Time, 10)
		for i := range expiries {
			expiries[i] = now.Add(ProviderUrlProbeRenewalHeadroom + time.Duration(i)*20*time.Minute)
		}
		client := testingRenewalHistory(t, now, expiries)
		var wg sync.WaitGroup
		results := make(chan []ProviderUrlProbeDue, 8)
		for range 8 {
			wg.Add(1)
			go func() { defer wg.Done(); results <- ClaimProviderUrlProbeDue(ctx, now, 1, 0, 1) }()
		}
		wg.Wait()
		close(results)
		all := []ProviderUrlProbeDue{}
		for due := range results {
			all = append(all, due...)
		}
		if len(all) != 1 {
			t.Fatal("concurrent renewal issued more than one in-flight claim")
		}
		completion := ProviderUrlProbeCompletion{ClientId: client, ClaimOrdinal: all[0].ClaimOrdinal, CompletedAt: now.Add(25 * time.Second), ProbeFailure: "tunnel_failed", AllowPacing: true}
		receipt, err := CompleteProviderUrlProbeRun(ctx, completion, completion.CompletedAt)
		if err != nil || receipt == nil {
			t.Fatal("owned setup completion failed")
		}
		cycle := testingReadUrlCompletionCycle(t, ctx, client)
		if cycle.history != 10 || cycle.next.Before(completion.CompletedAt.Add(54*time.Second)) || cycle.next.After(completion.CompletedAt.Add(66*time.Second)) {
			t.Fatal("setup failure credited quota or failed to pace required renewal")
		}
		replay, err := CompleteProviderUrlProbeRun(ctx, completion, completion.CompletedAt.Add(time.Second))
		if err != nil || replay == nil || !replay.Replay {
			t.Fatal("setup completion replay lost its identity")
		}
		if after := testingReadUrlCompletionCycle(t, ctx, client); after.next != cycle.next || after.count != cycle.count {
			t.Fatal("replay changed pacing or completion count")
		}
		if due := ClaimProviderUrlProbeDue(ctx, cycle.next, 1, 0, 1); len(due) != 1 || due[0].RunsNeeded != 0 {
			t.Fatal("measured-full provider could not retry required early replacement")
		}
	})
}

// Clocks are passed through the native admission/history paths. No real
// provider call or wall-clock wait is needed to expose the expiry/latency gap.
func TestUrlProbeRenewalKeepsCoverageAcrossExpiry(t *testing.T) {
	for _, spacing := range []time.Duration{20 * time.Minute, 24 * time.Minute} {
		t.Run(fmt.Sprint(spacing), func(t *testing.T) {
			server.DefaultTestEnv().Run(t, func(t testing.TB) {
				ctx := t.Context()
				now := server.NowUtc().Truncate(time.Microsecond)
				client := testingUrlCompletionClients(t, now.Add(-8*time.Hour), 1)[0]
				first := now.Add(-4*time.Hour + 10*time.Minute)
				var cycle time.Time
				for i := range ProviderUrlProbeRunTarget {
					at := first.Add(time.Duration(i) * spacing)
					due := testingClaimUrlCompletion(t, ctx, client, at.Add(-time.Second))
					cycle = due.CycleStartedAt
					testingSetUrlProbeHealth(ctx, &ProviderEgressHealth{RunId: server.NewId(), ClientId: client, CycleStartedAt: cycle, MeasuredAt: at, OKCount: i % 2, Total: 1})
					testingCompleteUrlClaim(t, ctx, due, at.Add(time.Microsecond), "")
				}
				expires := first.Add(ProviderEgressProbeRefreshAge)
				stored := testingReadUrlCompletionCycle(t, ctx, client)
				due := ClaimProviderUrlProbeDue(ctx, stored.next, 1, 0, 1)
				if len(due) != 1 {
					t.Fatal("paced renewal did not issue exactly one owned turn")
				}
				if duplicate := ClaimProviderUrlProbeDue(ctx, stored.next.Add(time.Second), 1, 0, 1); len(duplicate) != 0 {
					t.Fatal("renewal duplicated an in-flight claim")
				}
				completed := stored.next.Add(25 * time.Second)
				testingSetUrlProbeHealth(ctx, &ProviderEgressHealth{RunId: server.NewId(), ClientId: client, CycleStartedAt: cycle, MeasuredAt: completed, Total: 1})
				testingCompleteUrlClaim(t, ctx, due[0], completed.Add(time.Microsecond), "")
				fleet := testingGetUrlProbeFleet(t, ctx, expires)
				gap := max(time.Duration(0), completed.Sub(expires))
				t.Logf("spacing=%s accepted_latency=25s renewal_lead=%s modeled_coverage_gap=%s quota_at_expiry=%d", spacing, expires.Sub(stored.next), gap, fleet.QuotaComplete)
				if fleet.QuotaComplete != 1 || fleet.RunsNeeded != 0 || gap != 0 {
					t.Fatal("expiry-only admission leaves a latency-sized gap below ten accepted measurements")
				}
				if c := testingReadUrlCompletionCycle(t, ctx, client); c.history != 11 {
					t.Fatal("renewal must add exactly one real accepted outcome")
				}
			})
		})
	}
}

func TestUrlProbeRenewalHistoryIsProviderScoped(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc().Truncate(time.Microsecond)
		expiries := make([]time.Time, 9)
		for i := range expiries {
			expiries[i] = now.Add(time.Duration(i+1) * 20 * time.Minute)
		}
		client := testingRenewalHistory(t, now, expiries)
		other := server.NewId()
		server.Tx(ctx, func(tx server.PgTx) {
			for range 12 {
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO provider_egress_health_history
 (run_id,client_id,measured_at,ok_count,total_count,class_results,tls_authentication_failure,url_probe,url_probe_policy_version)
 VALUES($1,$2,$3,1,1,'{}',false,true,1)`, server.NewId(), other, now))
			}
		})
		due := ClaimProviderUrlProbeDue(ctx, now, 1, 0, 1)
		if len(due) != 1 || due[0].ClientId != client || due[0].RunsNeeded != 1 {
			t.Fatal("another provider's accepted history hid the measured deficit")
		}
	})
}
