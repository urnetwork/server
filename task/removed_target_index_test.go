// The retired-function reap runs at every taskworker start against the whole
// queue, tens of millions of rows in production. These tests pin its access
// path and its exact-name scope with the transaction's own scan counters.
package task

import (
	"context"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// With sequential scans disabled the reap must still find an index path. A
// bare function_name filter has none, so on the production queue the startup
// reap read every row, for minutes, inside the scheduling transaction that
// held every shared run-once row (the 2026-10-12 taskworker deploy).
func TestRemovePendingTasksForFunctionUsesFunctionPollIndex(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession := session.NewLocalClientSession(ctx, "192.0.2.1:1", nil)
		defer clientSession.Cancel()

		var removedName string
		server.Tx(ctx, func(tx server.PgTx) {
			removedTaskId := ScheduleTaskInTx(tx, removedTargetTask, &removedTargetArgs{}, clientSession)
			ScheduleTaskInTx(tx, survivorTargetTask, &removedTargetArgs{}, clientSession)
			server.Raise(tx.QueryRow(ctx, `SELECT function_name FROM pending_task WHERE task_id = $1`, removedTaskId).Scan(&removedName))
		})

		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `SET LOCAL enable_seqscan = off`))
			// counts accumulate in this backend until the transaction ends
			scanCounts := func() (queueScanCount int64, functionIndexScanCount int64) {
				server.Raise(tx.QueryRow(ctx, `
					SELECT
						pg_stat_get_xact_numscans('pending_task'::regclass),
						pg_stat_get_xact_numscans('pending_task_function_poll_order'::regclass)
				`).Scan(&queueScanCount, &functionIndexScanCount))
				return
			}
			queueScanCountBefore, functionIndexScanCountBefore := scanCounts()
			removedCount := RemovePendingTasksForFunctionInTx(ctx, tx, removedName)
			queueScanCountAfter, functionIndexScanCountAfter := scanCounts()
			if removedCount != 1 {
				t.Fatalf("reap removed %d rows of its function; want 1", removedCount)
			}
			if queueScanCountAfter != queueScanCountBefore || functionIndexScanCountAfter <= functionIndexScanCountBefore {
				t.Fatalf(
					"retired-function reap scanned the whole queue instead of the function poll index: sequential scans %d->%d, function index scans %d->%d",
					queueScanCountBefore,
					queueScanCountAfter,
					functionIndexScanCountBefore,
					functionIndexScanCountAfter,
				)
			}
		})
	})
}

// The index matches canonical names, which drop version path segments. The
// reap still removes only rows whose stored name is exactly the retired one.
func TestRemovePendingTasksForFunctionKeepsOtherVersionsOfItsCanonicalName(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession := session.NewLocalClientSession(ctx, "192.0.2.1:1", nil)
		defer clientSession.Cancel()

		const retiredName = "example.com/synthetic/retired.Target"
		const versionedName = "example.com/synthetic/v2/retired.Target"
		var retiredTaskId server.Id
		var versionedTaskId server.Id
		server.Tx(ctx, func(tx server.PgTx) {
			retiredTaskId = ScheduleTaskInTx(tx, removedTargetTask, &removedTargetArgs{}, clientSession)
			versionedTaskId = ScheduleTaskInTx(tx, survivorTargetTask, &removedTargetArgs{}, clientSession)
			server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET function_name = $2 WHERE task_id = $1`, retiredTaskId, retiredName))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET function_name = $2 WHERE task_id = $1`, versionedTaskId, versionedName))
			var canonicalNameCount int
			server.Raise(tx.QueryRow(ctx, `
				SELECT count(DISTINCT regexp_replace(function_name, '/v[0-9]+', '', 'g'))
				FROM pending_task WHERE task_id = ANY($1::uuid[])
			`, []server.Id{retiredTaskId, versionedTaskId}).Scan(&canonicalNameCount))
			if canonicalNameCount != 1 {
				t.Fatal("synthetic names do not share one canonical name")
			}
		})

		var removedCount int64
		server.Tx(ctx, func(tx server.PgTx) {
			removedCount = RemovePendingTasksForFunctionInTx(ctx, tx, retiredName)
		})
		var retiredRemaining int
		var versionedRemaining int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM pending_task WHERE task_id = $1`, retiredTaskId).Scan(&retiredRemaining))
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM pending_task WHERE task_id = $1`, versionedTaskId).Scan(&versionedRemaining))
		})
		if removedCount != 1 || retiredRemaining != 0 || versionedRemaining != 1 {
			t.Fatalf("reap of %s removed %d rows; retired remaining %d, versioned remaining %d", retiredName, removedCount, retiredRemaining, versionedRemaining)
		}
	})
}
