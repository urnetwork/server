// The production payout target at its real 6h max time. A held
// transfer_contract lock keeps the run inside its body, since it reaches that
// table only after resuming a staged sweep assignment. The test fires the
// max-time timer itself: after the assignment's completion commit is notified,
// or right after the body starts when nothing is staged.
package taskworker

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
	"github.com/urnetwork/server/taskworker/work"
)

// A run that completed a staged sweep assignment and then reached its max time
// is resumed by the next run, so it retries after RescheduleTimeout. The runner
// wraps a max-time failure in its own "Timeout" error, which hid the payout's
// committed-progress retry hint and left the run on the hour-long backoff.
func TestPayoutMaxTimeAfterCommittedProgressRetriesPromptly(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		delay := runPayoutToMaxTime(t, true)
		if delay != task.RescheduleTimeout {
			t.Fatalf("payout max time after committed progress retries in %s, want %s", delay, task.RescheduleTimeout)
		}
	})
}

// A run that committed nothing before its max time keeps the ordinary error
// backoff, so a stuck run is not retried hot.
func TestPayoutMaxTimeWithoutProgressKeepsBackoff(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		delay := runPayoutToMaxTime(t, false)
		if delay < 30*time.Minute || 90*time.Minute+task.RescheduleTimeout < delay {
			t.Fatalf("payout max time without progress retries in %s, want the [30m, 90m] backoff", delay)
		}
	})
}

// Runs the scheduled payout through the production worker until its max time,
// with one staged sweep when staged, and returns the reschedule delay. The task
// enters with 16 earlier errors, so its ordinary backoff is at the 1h cap.
func runPayoutToMaxTime(t testing.TB, staged bool) time.Duration {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	owner := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
	defer owner.Cancel()
	oldBase, oldCap := task.RescheduleTimeout, task.RescheduleBackoffMaxTimeout
	task.RescheduleTimeout, task.RescheduleBackoffMaxTimeout = 2*time.Second, time.Hour
	defer func() {
		task.RescheduleTimeout, task.RescheduleBackoffMaxTimeout = oldBase, oldCap
	}()

	paymentPlanId := server.NewId()
	paymentId := server.NewId()
	contractId := server.NewId()
	balanceId := server.NewId()
	networkId := server.NewId()
	var taskId server.Id
	server.Tx(ctx, func(tx server.PgTx) {
		if staged {
			// a committed plan whose one sweep is still staged
			now := server.NowUtc()
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO account_payment (payment_id, payment_plan_id, network_id, payout_byte_count, payout_nano_cents, min_sweep_time)
				VALUES ($1, $2, $3, 1024, 100, $4)`, paymentId, paymentPlanId, networkId, now))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_escrow_sweep (contract_id, balance_id, network_id, payout_byte_count, payout_net_revenue_nano_cents, sweep_time)
				VALUES ($1, $2, $3, 1024, 100, $4)`, contractId, balanceId, networkId, now))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO payment_plan_assignment (payment_plan_id, create_time, sweep_count) VALUES ($1, $2, 1)`, paymentPlanId, now))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO payment_plan_sweep (contract_id, balance_id, network_id, payment_plan_id, previous_payment_id, payment_id)
				VALUES ($1, $2, $3, $4, NULL, $5)`, contractId, balanceId, networkId, paymentPlanId, paymentId))
			// the barrier: the assignment's completion commit notifies the test
			server.RaisePgResult(tx.Exec(ctx, `
				CREATE FUNCTION test_payment_plan_assignment_complete() RETURNS trigger LANGUAGE plpgsql AS $notify$
				BEGIN
					IF OLD.complete_time IS NULL AND NEW.complete_time IS NOT NULL THEN
						PERFORM pg_notify('test_payment_plan_assignment_complete', NEW.payment_plan_id::text);
					END IF;
					RETURN NEW;
				END $notify$;
				CREATE TRIGGER test_payment_plan_assignment_complete AFTER UPDATE ON payment_plan_assignment
					FOR EACH ROW EXECUTE FUNCTION test_payment_plan_assignment_complete();
			`))
		}
		// the production schedule: RunOnce "payout" with a 6h max time, due now
		work.SchedulePayout(owner, tx)
		server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET
			run_at = now() - interval '5 seconds', release_time = now() - interval '5 seconds', reschedule_error_count = 16
			WHERE run_once_key = '["payout"]'`))
		server.Raise(tx.QueryRow(ctx, `SELECT task_id FROM pending_task WHERE run_once_key = '["payout"]'`).Scan(&taskId))
	}, server.OptNoRetry())

	timer := make(chan time.Time)
	budgets := make(chan time.Duration, 1)
	worker := InitTaskWorker(task.Testing_WithTaskRunAfter(ctx, func(_ context.Context, budget time.Duration) <-chan time.Time {
		budgets <- budget
		return timer
	}))

	server.Db(ctx, func(listen server.PgConn) {
		if staged {
			server.RaisePgResult(listen.Exec(ctx, `LISTEN test_payment_plan_assignment_complete`))
			defer listen.Exec(context.Background(), `UNLISTEN *`)
		}
		server.Db(ctx, func(conn server.PgConn) {
			held, err := conn.Begin(ctx)
			server.Raise(err)
			defer held.Rollback(context.Background())
			server.RaisePgResult(held.Exec(ctx, `LOCK TABLE transfer_contract IN ACCESS EXCLUSIVE MODE`))

			type evaluation struct {
				finished    []server.Id
				retried     []server.Id
				postRetried []server.Id
				err         error
			}
			evaluated := make(chan evaluation, 1)
			go func() {
				finished, retried, postRetried, err := worker.EvalTasks(1)
				evaluated <- evaluation{finished: finished, retried: retried, postRetried: postRetried, err: err}
			}()

			// the body started with the production max time
			select {
			case budget := <-budgets:
				if budget != 6*time.Hour {
					t.Fatalf("payout max time = %s, want 6h", budget)
				}
			case <-ctx.Done():
				t.Fatal("payout body did not start", ctx.Err())
			}
			if staged {
				notification, err := listen.Conn().WaitForNotification(ctx)
				if err != nil || notification.Payload != paymentPlanId.String() {
					t.Fatal("staged assignment did not commit its completion", err)
				}
			}
			close(timer)

			var result evaluation
			select {
			case result = <-evaluated:
			case <-ctx.Done():
				t.Fatal("payout run did not stop at its max time", ctx.Err())
			}
			server.Raise(held.Rollback(ctx))
			if result.err != nil || len(result.finished) != 0 || len(result.retried) != 1 || result.retried[0] != taskId || len(result.postRetried) != 0 {
				t.Fatalf("max time did not retry the payout task: finished=%v retried=%v post=%v err=%v", result.finished, result.retried, result.postRetried, result.err)
			}
		})
	})

	var delay time.Duration
	server.Db(ctx, func(conn server.PgConn) {
		var id server.Id
		var runAt, released time.Time
		var count int
		var failure string
		server.Raise(conn.QueryRow(ctx, `SELECT task_id, run_at, release_time, reschedule_error_count, reschedule_error
			FROM pending_task WHERE run_once_key = '["payout"]'`).Scan(&id, &runAt, &released, &count, &failure))
		if id != taskId || count != 17 || !strings.Contains(failure, "Timeout") {
			t.Fatalf("max time lost the payout task's failure: same_task=%t count=%d error=%q", id == taskId, count, failure)
		}
		delay = runAt.Sub(released)

		var assignedPaymentId *server.Id
		var stagedCount int
		server.Raise(conn.QueryRow(ctx, `SELECT
			(SELECT payment_id FROM transfer_escrow_sweep WHERE contract_id = $1 AND balance_id = $2 AND network_id = $3),
			(SELECT COUNT(*) FROM payment_plan_sweep)`, contractId, balanceId, networkId).Scan(&assignedPaymentId, &stagedCount))
		if staged && (assignedPaymentId == nil || *assignedPaymentId != paymentId || stagedCount != 0) {
			t.Fatal("the staged sweep was not assigned before the max time")
		}
	})
	t.Logf("staged=%t retry_delay=%s", staged, delay)
	return delay
}
