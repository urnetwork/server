// Admission remains authoritative while storage maintenance is withheld.
package model

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/qualityprobe/egresshealth"
)

// Old, future, wrong-policy and unmeasured history cannot gain credit when
// cleanup is delayed. Expiry and an unresolved TLS finding stay authoritative.
func TestUrlDueWithheldRetentionPreservesQuotaSecurityAndClaims(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc().Truncate(time.Microsecond)
		testingUrlCompletionPriority(t, now.Add(-8*time.Hour))
		clientId := testingUrlCompletionClients(t, now.Add(-5*time.Hour), 1)[0]
		destination := egresshealth.Destination{Name: "retained-security", Class: egresshealth.ClassSite, Url: "https://retained-security.example/page"}
		encoded, err := json.Marshal(destination)
		server.Raise(err)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO provider_url_probe_run
				(client_id,claim_ordinal,claimed_at,completed_at,received_at,counted)
				VALUES($1,1,$2,NULL,NULL,false),($1,2,$2,$2,$2,true)`, clientId, now.Add(-8*24*time.Hour)))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE provider_egress_probe_cycle SET claim_ordinal=2,completed_run_count=1,
				completed_next_expiry_at=$2,next_attempt_at=$3 WHERE client_id=$1`, clientId, now.Add(-8*24*time.Hour+4*time.Hour), now))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO provider_egress_url_security
				(client_id,url_key,destination,measured_at,tls_failure) VALUES($1,$2,$3::jsonb,$4,true)`, clientId,
				egresshealth.UrlProbeDestinationKey(destination), string(encoded), now))
			for _, row := range []struct {
				at            time.Time
				total, policy int
				url           bool
			}{
				{at: now.Add(-4 * time.Hour), total: 1, policy: 1, url: true},
				{at: now.Add(time.Microsecond), total: 1, policy: 1, url: true},
				{at: now, total: 1, policy: 0, url: true},
				{at: now, total: 1, policy: 2, url: true},
				{at: now, total: 0, policy: 1, url: true},
				{at: now, total: 2, policy: 1, url: true},
				{at: now, total: 1, policy: 1, url: false},
			} {
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO provider_egress_health_history
					(run_id,client_id,measured_at,ok_count,total_count,class_results,tls_authentication_failure,url_probe,url_probe_policy_version)
					VALUES($1,$2,$3,0,$4,'{}',false,$5,$6)`, server.NewId(), clientId, row.at, row.total, row.url, row.policy))
			}
		})
		var observation ProviderUrlProbeDueObservation
		due := ClaimProviderUrlProbeDueWithObservation(ctx, now, 1, 0, 1, &observation)
		if due.PriorityMaintenancePending || len(due.Providers) != 1 || due.Providers[0].ClaimOrdinal != 3 || due.Providers[0].RunsNeeded != ProviderUrlProbeRunTarget ||
			len(due.Providers[0].SecurityDestinations) != 1 || due.Providers[0].CompletedRunCount == nil || *due.Providers[0].CompletedRunCount != 0 {
			t.Fatal("withheld storage cleanup changed current admission authority")
		}
		if observation.RetentionDatabase != (server.DbTiming{}) || observation.Phases[ProviderUrlProbeDueRetentionQuery].Count != 0 ||
			observation.ClaimDatabase.Phases[server.DbTimingAcquire].Count != 1 || observation.ClaimDatabase.Phases[server.DbTimingBegin].Count != 1 {
			t.Fatal("Due retained a second storage-maintenance checkout")
		}
		server.Db(ctx, func(conn server.PgConn) {
			var retained int
			server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*) FROM provider_url_probe_run WHERE client_id=$1 AND claim_ordinal<3`, clientId).Scan(&retained))
			if retained != 2 {
				t.Fatal("Due performed storage cleanup")
			}
		})
		before := GetProviderUrlProbeFleet(ctx, now)
		if before.QuotaComplete != 0 || before.Complete != 0 || before.SecurityExceptions != 1 || before.RunsNeeded != ProviderUrlProbeRunTarget {
			t.Fatal("retained invalid history gained quota or hid security")
		}
		if receipt, err := CompleteProviderUrlProbeRun(ctx, ProviderUrlProbeCompletion{
			ClientId: clientId, ClaimOrdinal: 1, CompletedAt: now, ProbeFailure: "tunnel_failed", AllowPacing: true,
		}, now); err == nil || receipt != nil {
			t.Fatal("delayed cleanup let an ancient claim acquire a new completion")
		}
		if removed := RemoveExpiredProviderUrlProbeRuns(ctx, now, 5000); removed != 2 {
			t.Fatal("independent maintenance lost old receipt progress", removed)
		}
		if after := GetProviderUrlProbeFleet(ctx, now); !reflect.DeepEqual(before, after) {
			t.Fatal("storage cleanup changed the current fleet projection")
		}
		fresh := due.Providers[0]
		completion := ProviderUrlProbeCompletion{ClientId: clientId, ClaimOrdinal: fresh.ClaimOrdinal, CompletedAt: now, ProbeFailure: "tunnel_failed", AllowPacing: true}
		first, err := CompleteProviderUrlProbeRun(ctx, completion, now)
		if err != nil || first == nil || first.Replay {
			t.Fatal("current issued claim was lost", err)
		}
		repeat, err := CompleteProviderUrlProbeRun(ctx, completion, now)
		if err != nil || repeat == nil || !repeat.Replay || !repeat.CompletedAt.Equal(first.CompletedAt) {
			t.Fatal("current receipt replay lost identity", err)
		}
		if after := GetProviderUrlProbeFleet(ctx, now); after.QuotaComplete != 0 || after.SecurityExceptions != 1 || after.RunsNeeded != ProviderUrlProbeRunTarget {
			t.Fatal("attempt completion manufactured measured quota")
		}
	})
}
