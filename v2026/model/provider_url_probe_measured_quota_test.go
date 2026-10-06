package model

import (
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// Ten real failed measurements finish the quota without a success. The
// completion/legacy-attempt writers must not turn a full quota into a retry
// loop, while one run expires at the exact four-hour boundary.
func TestUrlProbeMeasuredFailuresCompleteQuotaAndExpire(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc().Truncate(time.Microsecond)
		clientId := testingUrlCompletionClients(t, now.Add(-3*time.Hour), 1)[0]
		var firstAt time.Time
		for turn := range ProviderUrlProbeRunTarget {
			at := now.Add(-30*time.Minute + time.Duration(turn)*2*time.Minute)
			due := testingClaimUrlCompletion(t, ctx, clientId, at)
			if due.RunsNeeded != ProviderUrlProbeRunTarget-turn || due.SuccessesNeeded != due.RunsNeeded {
				t.Fatalf("measured deficit or wire alias is wrong: %+v", due)
			}
			at = at.Add(time.Second)
			if turn == 0 {
				firstAt = at
			}
			result := &ProviderEgressHealth{RunId: server.NewId(), ClientId: clientId,
				CycleStartedAt: due.CycleStartedAt, MeasuredAt: at, Total: 1}
			testingSetUrlProbeHealth(ctx, result)
			// Retrying an accepted run with a newer clock cannot add a run or
			// move its four-hour expiry. First accepted history is immutable.
			replay := *result
			replay.MeasuredAt = at.Add(time.Second)
			testingSetUrlProbeHealth(ctx, &replay)
			testingCompleteUrlClaim(t, ctx, due, at.Add(2*time.Second), "url_failed")
		}
		fleet := testingGetUrlProbeFleet(t, ctx, now)
		if fleet.QuotaComplete != 1 || fleet.Complete != 1 || fleet.RunsNeeded != 0 {
			t.Fatalf("ten accepted errors failed measured quota: %+v", fleet)
		}
		cycle := testingReadUrlCompletionCycle(t, ctx, clientId)
		if cycle.history != 10 || cycle.successes != 0 || cycle.errors != 10 || cycle.count != 10 ||
			!cycle.next.Equal(firstAt.Add(ProviderEgressProbeRefreshAge-ProviderUrlProbeRenewalHeadroom)) {
			t.Fatalf("receipt dedup, diagnostics or failure completion pacing drifted: %+v", cycle)
		}
		SetProviderEgressProbeAttempt(ctx, &ProviderEgressProbeAttempt{ClientId: clientId, AttemptAt: now, ProbeFailure: "setup_failed"})
		if after := testingReadUrlCompletionCycle(t, ctx, clientId); !after.next.Equal(cycle.next) {
			t.Fatalf("attempt-only writer reopened full measured quota: %+v", after)
		}
		if early := ClaimProviderUrlProbeDue(ctx, cycle.next.Add(-time.Microsecond), 1, 0, 1); len(early) != 0 {
			t.Fatalf("quota-full failure was retried before its replacement deadline: %+v", early)
		}
		due := ClaimProviderUrlProbeDue(ctx, cycle.next, 1, 0, 1)
		if len(due) != 1 || due[0].RunsNeeded != 0 || due[0].OutcomeCount != 10 {
			t.Fatalf("replacement horizon did not issue one turn while coverage remained full: %+v", due)
		}
	})
}

// The completed-turn ledger intentionally counts local failures for fairness.
// The user quota requires an accepted URL measurement, so ten such receipts
// must still leave the entire measured deficit visible.
func TestUrlProbeSetupCompletionsNeverFillMeasuredQuota(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc().Truncate(time.Microsecond)
		clientId := testingUrlCompletionClients(t, now.Add(-3*time.Hour), 1)[0]
		for turn := range ProviderUrlProbeRunTarget {
			at := now.Add(-30*time.Minute + time.Duration(turn)*2*time.Minute)
			due := testingClaimUrlCompletion(t, ctx, clientId, at)
			testingCompleteUrlClaim(t, ctx, due, at.Add(time.Second), "tunnel_failed")
		}
		fleet := testingGetUrlProbeFleet(t, ctx, now)
		cycle := testingReadUrlCompletionCycle(t, ctx, clientId)
		if cycle.count != 10 || cycle.history != 0 || fleet.QuotaComplete != 0 || fleet.RunsNeeded != 10 {
			t.Fatalf("all-turn fairness receipts fabricated measured quota: cycle=%+v fleet=%+v", cycle, fleet)
		}
	})
}

// Accepted shape and policy, rather than current ratio or receipt arrival,
// decide quota credit. Security-only and old/future policy rows never qualify.
func TestUrlProbeMeasuredQuotaExactPolicyShapeAndClock(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc().Truncate(time.Microsecond)
		clientId := testingUrlCompletionClients(t, now.Add(-5*time.Hour), 1)[0]
		server.Tx(ctx, func(tx server.PgTx) {
			for _, row := range []struct {
				at                time.Time
				ok, total, policy int
				url               bool
			}{
				{now.Add(-4*time.Hour + time.Microsecond), 0, 1, 1, true},
				{now.Add(-time.Minute), 1, 1, 1, true},
				{now.Add(-4 * time.Hour), 0, 1, 1, true},
				{now.Add(time.Microsecond), 0, 1, 1, true},
				{now, 0, 1, 0, true}, {now, 0, 1, 2, true},
				{now, 0, 0, 1, true}, {now, 0, 2, 1, true},
				{now, 0, 1, 1, false},
			} {
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO provider_egress_health_history
					(run_id,client_id,measured_at,ok_count,total_count,class_results,tls_authentication_failure,url_probe,url_probe_policy_version)
					VALUES($1,$2,$3,$4,$5,'{}',false,$6,$7)`, server.NewId(), clientId, row.at, row.ok, row.total, row.url, row.policy))
			}
		})
		server.Db(ctx, func(conn server.PgConn) {
			var count int
			var oldest time.Time
			server.Raise(conn.QueryRow(ctx, providerUrlProbeRunWindowSql("$1", "$2"), clientId, now).Scan(&count, &oldest))
			if count != 2 || !oldest.Equal(now.Add(-4*time.Hour+time.Microsecond)) {
				t.Fatalf("quota credited wrong shape, policy or boundary: count=%d oldest=%s", count, oldest)
			}
		})
	})
}
