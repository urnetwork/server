// Scheduling a finished run-once task again from its stored row.
package task

import (
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// A finished run-once task is scheduled again with its stored row copied, so an
// argument field its build does not decode survives, and counted as one
// submission. A finished task without the key schedules nothing. Outside the
// finishing transaction, where a pending row holds the key, it schedules
// nothing either and counts nothing, leaving the pending row as it was with
// no wake recorded, for the caller's fallback to merge and count once.
func TestRescheduleFinishedTaskCopiesTheStoredArguments(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		clientSession := session.Testing_CreateClientSession(ctx, nil)
		key := RunOnce("reschedule_finished_task")
		const storedArgs = `{"newer_field": [1, 2]}`
		var taskId server.Id
		server.Tx(ctx, func(tx server.PgTx) {
			taskId = ScheduleTaskInTx(tx, claimProfileAllowed, &claimProfileArgs{}, clientSession, key,
				RunAt(server.NowUtc().Add(-time.Minute)))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET args_json = $2 WHERE task_id = $1`, taskId, storedArgs))
		})
		worker := NewTaskWorkerWithDefaults(ctx)
		defer worker.Close()
		worker.AddTargets(NewTaskTarget(claimProfileAllowed))
		if finished, _, _, err := worker.EvalTasks(1); err != nil || len(finished) != 1 || finished[0] != taskId {
			t.Fatalf("the fixture task did not finish: finished=%v err=%v", finished, err)
		}
		pending := func() (count int, argsJson string, runAt time.Time, pendingTaskId server.Id) {
			server.Db(ctx, func(conn server.PgConn) {
				result, err := conn.Query(ctx, `SELECT task_id, args_json, run_at FROM pending_task WHERE run_once_key = $1`, key.String())
				server.WithPgResult(result, err, func() {
					for result.Next() {
						count++
						server.Raise(result.Scan(&pendingTaskId, &argsJson, &runAt))
					}
				})
			})
			return
		}

		runAt := server.NowUtc().Add(time.Hour).Truncate(time.Microsecond)
		submitted, balked := taskSubmittedCounter.ConfirmedCount(), taskBalkedCounter.ConfirmedCount()
		var otherKey, unknownTask, rescheduled bool
		server.Tx(ctx, func(tx server.PgTx) {
			otherKey = RescheduleFinishedTaskInTx(ctx, tx, taskId, RunOnce("reschedule_finished_task", "other"), runAt)
			unknownTask = RescheduleFinishedTaskInTx(ctx, tx, server.NewId(), key, runAt)
			rescheduled = RescheduleFinishedTaskInTx(ctx, tx, taskId, key, runAt)
		})
		if otherKey || unknownTask || !rescheduled {
			t.Fatalf("rescheduled other key=%t unknown task=%t own key=%t", otherKey, unknownTask, rescheduled)
		}
		if counted := taskSubmittedCounter.ConfirmedCount() - submitted; counted != 1 || taskBalkedCounter.ConfirmedCount() != balked {
			t.Fatalf("the reschedule counted %d submissions and %d balks, want 1 and 0", counted, taskBalkedCounter.ConfirmedCount()-balked)
		}
		count, argsJson, pendingRunAt, pendingTaskId := pending()
		if count != 1 || argsJson != storedArgs || !pendingRunAt.Equal(runAt) || pendingTaskId == taskId {
			t.Fatalf("the successor is not the stored row: count=%d args=%s run_at=%s new_id=%t", count, argsJson, pendingRunAt, pendingTaskId != taskId)
		}

		const pendingArgs = `{"pending": true}`
		var merged bool
		balked = taskBalkedCounter.ConfirmedCount()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET args_json = $2 WHERE task_id = $1`, pendingTaskId, pendingArgs))
			merged = RescheduleFinishedTaskInTx(ctx, tx, taskId, key, runAt.Add(-30*time.Minute))
		})
		var generation int64
		var wakeAt *time.Time
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT run_once_generation, run_once_wake_at FROM pending_task WHERE task_id = $1`, pendingTaskId).Scan(&generation, &wakeAt))
		})
		count, argsJson, pendingRunAt, _ = pending()
		if merged || count != 1 || argsJson != pendingArgs || !pendingRunAt.Equal(runAt) || generation != 0 || wakeAt != nil {
			t.Fatalf("a call outside the finishing transaction changed the pending row: merged=%t count=%d args=%s run_at=%s generation=%d wake=%v",
				merged, count, argsJson, pendingRunAt, generation, wakeAt)
		}
		if counted := taskBalkedCounter.ConfirmedCount() - balked; counted != 0 {
			t.Fatalf("a call outside the finishing transaction counted %d balks, leaving its fallback to count a second", counted)
		}
	})
}
