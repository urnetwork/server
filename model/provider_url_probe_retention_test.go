// Receipt storage age must not grant fresh completion, quota, or security.
package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// A population of retained claims must not turn a bounded cleanup page into
// a history scan. Both normal and generic prepared plans execute the owner SQL.
func TestUrlReceiptRetention500kUsesBoundedIndexedPages(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc().Truncate(time.Microsecond)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO provider_url_probe_run
				(client_id,claim_ordinal,claimed_at,completed_at,received_at,counted)
				SELECT md5('synthetic-retention-'||i::text)::uuid,j,
					CASE WHEN j<=4 THEN $1::timestamp-interval '8 days' ELSE $1::timestamp END,
					CASE WHEN j=5 THEN $1::timestamp ELSE NULL END,
					CASE WHEN j=5 THEN $1::timestamp ELSE NULL END,j=5
				FROM generate_series(1,100000) AS i CROSS JOIN generate_series(1,5) AS j`, now))
		})
		server.Db(ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(ctx, `VACUUM (ANALYZE) provider_url_probe_run`))
		})
		custom := testingExplainUrlCompleted(t, providerUrlProbeRunRetentionSql, now.Add(-providerUrlProbeRunRetention), 5000)
		testingCheckUrlCompletedMaintenanceWork(t, "retention_500k_custom", custom, 0, 10000, 320000)
		generic := testingExplainUrlCompletedGeneric(t, "retention_500k_generic", providerUrlProbeRunRetentionSql,
			testingUrlCompletedTimestampLiteral(now.Add(-providerUrlProbeRunRetention))+",5000")
		testingCheckUrlCompletedMaintenanceWork(t, "retention_500k_generic", generic, 0, 10000, 320000)
		for _, plan := range []testingUrlCompletedPlan{custom, generic} {
			indexed := false
			testingWalkUrlCompletedPlan(plan.Plan, func(node testingUrlCompletedPlanNode) {
				if node.IndexName == "provider_url_probe_run_retention" && node.IndexCond != "" {
					indexed = true
				}
			})
			if !indexed {
				t.Fatal("retention lost the bounded old-receipt index range")
			}
		}
		server.Db(ctx, func(conn server.PgConn) {
			var old, current int
			server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*) FILTER(WHERE NOT counted),COUNT(*) FILTER(WHERE counted) FROM provider_url_probe_run`).Scan(&old, &current))
			if old != 390000 || current != 100000 {
				t.Fatal("cleanup lost its page bound or deleted current authority", old, current)
			}
		})
	})
}

// Busy receipts remain for a later turn, and cancellation cannot commit a
// partial page. The actual row lock is the barrier; no timing race is required.
func TestUrlReceiptRetentionSkipsLockedAndCanceledPages(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		now, clientId := server.NowUtc(), server.NewId()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO provider_url_probe_run(client_id,claim_ordinal,claimed_at)
				SELECT $1,i,$2 FROM generate_series(1,3) AS i`, clientId, now.Add(-8*24*time.Hour)))
		})
		server.Tx(ctx, func(tx server.PgTx) {
			rows, err := tx.Query(ctx, `SELECT claim_ordinal FROM provider_url_probe_run WHERE client_id=$1 FOR UPDATE`, clientId)
			server.WithPgResult(rows, err, func() {
				for rows.Next() {
				}
			})
			if removed := RemoveExpiredProviderUrlProbeRuns(ctx, now, 2); removed != 0 {
				t.Fatal("cleanup crossed another owner's row locks")
			}
		})
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		var failure any
		func() {
			defer func() { failure = recover() }()
			RemoveExpiredProviderUrlProbeRuns(canceled, now, 2)
		}()
		if failure == nil {
			t.Fatal("canceled model cleanup reported success")
		}
		for _, want := range []int64{2, 1, 0} {
			if removed := RemoveExpiredProviderUrlProbeRuns(ctx, now, 2); removed != want {
				t.Fatal("cleanup lost bounded resumable progress", removed, want)
			}
		}
	})
}
