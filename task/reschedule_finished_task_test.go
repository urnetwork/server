// Scheduling a finished run-once task again from its stored row.
package task

import (
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// A finished run-once task is scheduled again with its stored row copied, so an
// argument field its build does not decode survives. A finished task without
// the key schedules nothing, and a pending row of the key keeps its arguments
// and only moves earlier.
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
		var otherKey, unknownTask, rescheduled bool
		server.Tx(ctx, func(tx server.PgTx) {
			otherKey = RescheduleFinishedTaskInTx(ctx, tx, taskId, RunOnce("reschedule_finished_task", "other"), runAt)
			unknownTask = RescheduleFinishedTaskInTx(ctx, tx, server.NewId(), key, runAt)
			rescheduled = RescheduleFinishedTaskInTx(ctx, tx, taskId, key, runAt)
		})
		if otherKey || unknownTask || !rescheduled {
			t.Fatalf("rescheduled other key=%t unknown task=%t own key=%t", otherKey, unknownTask, rescheduled)
		}
		count, argsJson, pendingRunAt, pendingTaskId := pending()
		if count != 1 || argsJson != storedArgs || !pendingRunAt.Equal(runAt) || pendingTaskId == taskId {
			t.Fatalf("the successor is not the stored row: count=%d args=%s run_at=%s new_id=%t", count, argsJson, pendingRunAt, pendingTaskId != taskId)
		}

		const pendingArgs = `{"pending": true}`
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET args_json = $2 WHERE task_id = $1`, pendingTaskId, pendingArgs))
			RescheduleFinishedTaskInTx(ctx, tx, taskId, key, runAt.Add(time.Hour))
			RescheduleFinishedTaskInTx(ctx, tx, taskId, key, runAt.Add(-30*time.Minute))
		})
		count, argsJson, pendingRunAt, _ = pending()
		if count != 1 || argsJson != pendingArgs || !pendingRunAt.Equal(runAt.Add(-30*time.Minute)) {
			t.Fatalf("the pending row did not keep its arguments and move only earlier: count=%d args=%s run_at=%s", count, argsJson, pendingRunAt)
		}
	})
}
