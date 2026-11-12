package taskworker

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
	"github.com/urnetwork/server/taskworker/work"
)

// USDC payouts ended (owner decision 2026-11-12). A "payout" or "payout_retry"
// row queued before the change, under the target's function name or its
// pre-2024.11 name, runs once in the production taskworker as a no-op: it
// finishes, plans no payment and queues nothing, so no payout row is left. The
// startup schedule queues no payout run. Before the change the run planned and
// sent, its post queued the next Sunday's run, and every start queued one.
func TestQueuedPayoutRunsDrainAsNoOps(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession := session.NewLocalClientSession(ctx, "192.0.2.1:1", nil)
		defer clientSession.Cancel()

		payoutNames := []string{
			task.NewTaskTarget(work.Payout).TargetFunctionName(),
			"bringyour.com/service/taskworker/work.Payout",
		}
		pendingPayoutRuns := func() (count int) {
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*) FROM pending_task WHERE function_name = ANY($1)`, payoutNames).Scan(&count))
			})
			return
		}

		due := server.NowUtc().Add(-time.Minute)
		weeklyId := task.ScheduleTask(work.Payout, &work.SchedulePayoutArgs{}, clientSession, task.RunOnce("payout"), task.RunAt(due), task.MaxTime(6*time.Hour))
		retryId := task.ScheduleTask(work.Payout, &work.SchedulePayoutArgs{}, clientSession, task.RunOnce("payout_retry"), task.RunAt(due))
		// the retry row as the old post wrote it, under the pre-2024.11 name
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET function_name = $2, args_json = '{"retry":true}' WHERE task_id = $1`, retryId, payoutNames[1]))
		})
		if count := pendingPayoutRuns(); count != 2 {
			t.Fatalf("queued %d payout runs, want 2", count)
		}

		worker := InitTaskWorker(ctx)
		defer worker.Close()
		finishedIds := []server.Id{}
		for range 20 {
			finished, _, _, err := worker.EvalTasks(10)
			if err != nil {
				t.Fatal(err)
			}
			finishedIds = append(finishedIds, finished...)
			if pendingPayoutRuns() == 0 {
				break
			}
		}
		if count := pendingPayoutRuns(); count != 0 {
			t.Fatalf("%d payout runs are still queued after the queued runs ran", count)
		}
		for _, taskId := range []server.Id{weeklyId, retryId} {
			if !slices.Contains(finishedIds, taskId) {
				t.Fatalf("payout run %s did not finish: finished %v", taskId, finishedIds)
			}
		}
		finished := task.GetFinishedTasks(ctx, weeklyId, retryId)
		for _, taskId := range []server.Id{weeklyId, retryId} {
			finishedTask, ok := finished[taskId]
			if !ok {
				t.Fatalf("payout run %s has no finished row", taskId)
			}
			if finishedTask.RescheduleError != "" {
				t.Fatalf("payout run %s finished with error %q", taskId, finishedTask.RescheduleError)
			}
		}

		var plans, payments, queued int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*) FROM payment_report`).Scan(&plans))
			server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*) FROM account_payment`).Scan(&payments))
			server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*) FROM pending_task`).Scan(&queued))
		})
		if plans != 0 || payments != 0 || queued != 0 {
			t.Fatalf("the payout runs planned %d plans and %d payments and queued %d tasks", plans, payments, queued)
		}

		initTaskSchedule(ctx)
		if count := pendingPayoutRuns(); count != 0 {
			t.Fatalf("the startup schedule queued %d payout runs", count)
		}
	})
}
