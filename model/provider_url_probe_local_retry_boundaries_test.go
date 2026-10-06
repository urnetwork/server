package model

import (
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// Quota renewal can already need work before a measurement expires. Local
// readiness cannot turn that finished attempt into a fresh fifteen-minute hold.
func TestUrlCompletedLocalRetryPreservesRenewalSecurityAndEligibility(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc().Truncate(time.Microsecond)
		testingUrlCompletionClients(t, now.Add(-8*time.Hour), 3)
		due := ClaimProviderUrlProbeDue(ctx, now, 3, 0, 1)
		if len(due) != 3 {
			t.Fatal("missing real claims")
		}
		server.Tx(ctx, func(tx server.PgTx) {
			for index, claim := range due[:2] {
				for i := range 10 {
					expiry := now.Add(time.Duration(10+i*20) * time.Minute)
					if index == 0 && i == 0 {
						expiry = now.Add(5 * time.Minute)
					}
					server.RaisePgResult(tx.Exec(ctx, `INSERT INTO provider_egress_health_history
						(run_id,client_id,measured_at,ok_count,total_count,class_results,tls_authentication_failure,url_probe,url_probe_policy_version)
						VALUES($1,$2,$3,1,1,'{}',false,true,1)`, server.NewId(), claim.ClientId, expiry.Add(-ProviderEgressProbeRefreshAge)))
				}
			}
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO provider_egress_health
				(client_id,measured_at,ok_count,total_count,class_results,reputation_ok,reputation_total,tls_authentication_failure,legacy_tls_authentication_failure)
				VALUES($1,$2,0,0,'{}',0,0,true,true)`, due[1].ClientId, now.Add(-time.Minute)))
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM provide_key WHERE client_id=$1`, due[2].ClientId))
		})
		finished, received := now.Add(5*time.Second), now.Add(6*time.Second)
		for index, claim := range due {
			testingLocalCompletion(t, ctx, claim, finished, received)
			cycle := testingReadUrlCompletionCycle(t, ctx, claim.ClientId)
			if delay := cycle.next.Sub(received); delay < 54*time.Second || delay > 66*time.Second {
				t.Fatalf("finished renewal/security/local turn kept live lease: index=%d delay=%s", index, delay)
			}
			wantHistory := 10
			if index == 2 {
				wantHistory = 0
			}
			if cycle.history != wantHistory || cycle.successes != 0 || cycle.errors != 0 || cycle.count != 1 {
				t.Fatalf("local receipt changed evidence: %+v", cycle)
			}
		}
		server.Db(ctx, func(conn server.PgConn) {
			var tls, legacy bool
			server.Raise(conn.QueryRow(ctx, `SELECT tls_authentication_failure,legacy_tls_authentication_failure FROM provider_egress_health WHERE client_id=$1`, due[1].ClientId).Scan(&tls, &legacy))
			if !tls || !legacy {
				t.Fatal("local completion cleared security quarantine")
			}
		})
		got := ClaimProviderUrlProbeDue(ctx, received.Add(67*time.Second), 3, 0, 1)
		if len(got) != 2 {
			t.Fatalf("renewal/security retry missing or ineligible provider admitted: %+v", got)
		}
		for _, claim := range got {
			if claim.ClientId == due[2].ClientId || claim.RunsNeeded != 0 {
				t.Fatal("retry bypassed current admission or manufactured deficit")
			}
		}
	})
}
