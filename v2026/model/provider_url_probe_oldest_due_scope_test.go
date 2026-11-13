package model

import (
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// The oldest due timestamp belongs to the eligible scheduling cohort, not to
// the deficient subset. Stale scheduling hints cannot redefine either cohort.
func TestUrlProbeOldestDueScopeIsIndependentOfQuotaDeficit(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc().Truncate(time.Microsecond)
		testingSeedUrlProbeFleet(t, now.Add(-48*time.Hour), 3)
		var ids []server.Id
		server.Db(ctx, func(conn server.PgConn) {
			rows, err := conn.Query(ctx, `SELECT client_id FROM provider_egress_probe_cycle ORDER BY client_id`)
			server.WithPgResult(rows, err, func() {
				for rows.Next() {
					var id server.Id
					server.Raise(rows.Scan(&id))
					ids = append(ids, id)
				}
			})
		})
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE provider_egress_probe_cycle SET eligible=false WHERE client_id=$1`, ids[0]))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE provider_egress_probe_cycle SET next_attempt_at=$2 WHERE client_id=$1`, ids[1], now.Add(-time.Minute)))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET active=false WHERE client_id=$1`, ids[2]))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE provider_egress_probe_cycle SET next_attempt_at=$2 WHERE client_id=$1`, ids[2], now.Add(-7*24*time.Hour)))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO provider_egress_health_history
				(run_id,client_id,measured_at,ok_count,total_count,class_results,tls_authentication_failure,url_probe,url_probe_policy_version)
				SELECT md5($1::text||'-scope-'||g)::uuid,$1::uuid,$2::timestamp-g*interval '1 minute',g%2,1,'{}',false,true,1
				FROM generate_series(1,10)g`, ids[0], now))
		})
		fleet := testingGetUrlProbeFleet(t, ctx, now)
		if fleet.Eligible != 2 || fleet.QuotaComplete != 1 || fleet.Overdue != 1 || fleet.Due != 2 || fleet.RunsNeeded != 10 || fleet.OldestDueSeconds != (48*time.Hour).Seconds() {
			t.Fatalf("quota-complete old timestamp or current eligibility scope changed: %+v", fleet)
		}
		// Removing the full provider from current eligibility exposes the actual
		// deficient provider's younger timestamp, even though old cycles remain.
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET active=false WHERE client_id=$1`, ids[0]))
		})
		fleet = testingGetUrlProbeFleet(t, ctx, now)
		if fleet.Eligible != 1 || fleet.Overdue != 1 || fleet.QuotaComplete != 0 || fleet.OldestDueSeconds != time.Minute.Seconds() {
			t.Fatalf("ineligible old cycles changed eligible oldest timestamp: %+v", fleet)
		}
		// A missing eligible cycle remains a full deficit and due, but supplies
		// no timestamp. A zero maximum is therefore not a no-deficit signal.
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM provider_egress_probe_cycle WHERE client_id=$1`, ids[1]))
		})
		fleet = testingGetUrlProbeFleet(t, ctx, now)
		if fleet.Eligible != 1 || fleet.Overdue != 1 || fleet.MissingCycles != 1 || fleet.Due != 1 || fleet.RunsNeeded != 10 || fleet.OldestDueSeconds != 0 {
			t.Fatalf("missing-cycle deficit was cleared or given an invented age: %+v", fleet)
		}
	})
}
