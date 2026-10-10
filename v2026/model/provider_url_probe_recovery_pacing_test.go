// Accepted measurements replenish a mature deficit before ordinary warmup pacing.
package model

import (
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// A measured success counts as one run, not as completion of the rolling ten.
// After expiry removes multiple old runs, a mature provider must not wait the
// ordinary warmup success interval before recovering the remaining deficit.
func TestUrlProbeMatureMeasuredDeficitUsesRecoveryPace(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mature bool
		prior  int
		ok     int
	}{
		{name: "mature_success_deficit", mature: true, prior: 8, ok: 1},
		{name: "warmup_success_unchanged", mature: false, prior: 8, ok: 1},
		{name: "mature_failure_counts_and_keeps_retry", mature: true, prior: 8, ok: 0},
		{name: "complete_waits_for_oldest_renewal", mature: true, prior: 9, ok: 1},
	} {
		server.DefaultTestEnv().Run(t, func(t testing.TB) {
			ctx := t.Context()
			now := server.NowUtc().Truncate(time.Microsecond)
			cycleAge := time.Hour
			if tc.mature {
				cycleAge = 5 * time.Hour
			}
			testingSeedUrlProbeFleet(t, now.Add(-cycleAge), 1)
			var clientId server.Id
			var cycleAt time.Time
			server.Db(ctx, func(c server.PgConn) {
				server.Raise(c.QueryRow(ctx, `SELECT client_id,cycle_started_at FROM provider_egress_probe_cycle`).Scan(&clientId, &cycleAt))
			})
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO provider_egress_health_history(run_id,client_id,measured_at,ok_count,total_count,class_results,tls_authentication_failure,url_probe,url_probe_policy_version)
   SELECT md5($1::text||'-retained-'||g)::uuid,$1::uuid,$2::timestamp-g*interval '1 minute',1,1,'{}',false,true,1 FROM generate_series(1,$3::integer)g`, clientId, now, tc.prior))
			})
			before := GetProviderUrlProbeFleet(ctx, now)
			if before.Eligible != 1 || before.RunsNeeded != 10-tc.prior {
				t.Fatalf("%s: invalid finite fixture quota", tc.name)
			}
			due := ClaimProviderUrlProbeDue(ctx, now, 1, 0, 1)
			if len(due) != 1 || due[0].RunsNeeded != 10-tc.prior {
				t.Fatalf("%s: missing exact deficit admission", tc.name)
			}
			health := &ProviderEgressHealth{RunId: server.NewId(), ClientId: clientId, CycleStartedAt: cycleAt, MeasuredAt: now, OKCount: tc.ok, Total: 1}
			testingSetUrlProbeHealth(ctx, health)
			testingSetUrlProbeHealth(ctx, health)
			after := GetProviderUrlProbeFleet(ctx, now)
			if after.RunsNeeded != 9-tc.prior {
				t.Fatalf("%s: success/error/replay did not preserve one accepted measured run", tc.name)
			}
			var next time.Time
			server.Db(ctx, func(c server.PgConn) {
				server.Raise(c.QueryRow(ctx, `SELECT next_attempt_at FROM provider_egress_probe_cycle WHERE client_id=$1`, clientId).Scan(&next))
			})
			if tc.prior == 9 {
				renewalAt := now.Add(ProviderEgressProbeRefreshAge - ProviderUrlProbeRenewalHeadroom - time.Duration(tc.prior)*time.Minute)
				if !next.Equal(renewalAt) {
					t.Fatalf("%s: quota-full oldest-measurement renewal pacing: got %s, want %s", tc.name, next, renewalAt)
				}
			} else {
				pace := 20 * time.Minute
				if tc.mature || tc.ok == 0 {
					pace = time.Minute
				}
				if next.Sub(now) < pace*9/10 || next.Sub(now) > pace*11/10 {
					t.Fatalf("%s: next measured deficit turn paced for %s, expected %s +/-10%%", tc.name, next.Sub(now), pace)
				}
			}
			if early := ClaimProviderUrlProbeDue(ctx, next.Add(-time.Microsecond), 1, 0, 1); len(early) != 0 {
				t.Fatalf("%s: durable pacing reservation bypassed", tc.name)
			}
		})
	}
}
