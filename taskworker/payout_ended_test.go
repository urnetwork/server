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

// USDC payouts ended (owner decisions 2026-11-12 and 2026-11-13). A "payout" or
// "payout_retry" row queued before the change, under the target's function
// name or its pre-2024.11 name, and a queued pending-payment sweep or hung
// payment check run once in the production taskworker as no-ops: each
// finishes, plans, sends and cancels nothing and queues nothing, so no payout
// row is left and a pending payment stays as it was. The startup schedule
// queues none of them. Before the change the payout run planned and sent and
// its post queued the next Sunday's run, the sweep queued a send of the
// pending payment, the check canceled it as hung, each post queued its next
// run, and every start queued the three.
func TestQueuedPayoutRunsDrainAsNoOps(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession := session.NewLocalClientSession(ctx, "192.0.2.1:1", nil)
		defer clientSession.Cancel()

		payoutNames := []string{
			task.NewTaskTarget(work.Payout).TargetFunctionName(),
			"bringyour.com/service/taskworker/work.Payout",
			task.NewTaskTarget(work.ProcessPendingPayouts).TargetFunctionName(),
			task.NewTaskTarget(work.CancelHungAccountPayments).TargetFunctionName(),
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
		sweepId := task.ScheduleTask(work.ProcessPendingPayouts, &work.ProcessPendingPayoutsArgs{}, clientSession, task.RunOnce("process_pending_payouts"), task.RunAt(due))
		hungId := task.ScheduleTask(work.CancelHungAccountPayments, &work.CancelHungAccountPaymentsArgs{}, clientSession, task.RunOnce("cancel_hung_account_payments"), task.RunAt(due))
		if count := pendingPayoutRuns(); count != 4 {
			t.Fatalf("queued %d payout runs, want 4", count)
		}
		// a pending payment created long ago, past any hung payment timeout
		paymentId := server.NewId()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO account_payment (payment_id, payment_plan_id, network_id, payout_byte_count, payout_nano_cents, min_sweep_time, create_time)
				VALUES ($1, $2, $3, 1, 1, '2000-01-01', '2000-01-01')`, paymentId, server.NewId(), server.NewId()))
		})

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
		queuedIds := []server.Id{weeklyId, retryId, sweepId, hungId}
		for _, taskId := range queuedIds {
			if !slices.Contains(finishedIds, taskId) {
				t.Fatalf("payout run %s did not finish: finished %v", taskId, finishedIds)
			}
		}
		finished := task.GetFinishedTasks(ctx, queuedIds...)
		for _, taskId := range queuedIds {
			finishedTask, ok := finished[taskId]
			if !ok {
				t.Fatalf("payout run %s has no finished row", taskId)
			}
			if finishedTask.RescheduleError != "" {
				t.Fatalf("payout run %s finished with error %q", taskId, finishedTask.RescheduleError)
			}
		}

		var plans, payments, pending, queued int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*) FROM payment_report`).Scan(&plans))
			server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*), COUNT(*) FILTER (WHERE payment_id = $1 AND NOT completed AND NOT canceled) FROM account_payment`, paymentId).Scan(&payments, &pending))
			server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*) FROM pending_task`).Scan(&queued))
		})
		if plans != 0 || payments != 1 || pending != 1 || queued != 0 {
			t.Fatalf("the payout runs planned %d plans and left %d payments, %d of them the pending one, and queued %d tasks", plans, payments, pending, queued)
		}

		initTaskSchedule(ctx)
		if count := pendingPayoutRuns(); count != 0 {
			t.Fatalf("the startup schedule queued %d payout runs", count)
		}
	})
}
