// Each returned task commits its own completion and continuation. One evaluator
// finalizes sequentially, retaining its batch's advisory ownership until join.
package task

import (
	"context"
	mathrand "math/rand"
	"time"

	"github.com/urnetwork/server"
)

// A fresh detached deadline bounds each handback. Outcomes and external posts
// are returned only after commit; a transaction retry resets its attempt state.
func (self *TaskWorker) finalizeTask(r *taskExecutionResult) (
	commitPosts []server.PostFunction,
	postRescheduled bool,
) {
	finalizeTimeout := self.settings.FinalizeTimeout
	if finalizeTimeout <= 0 {
		finalizeTimeout = DefaultTaskFinalizeTimeout
	}
	finalizeCtx, finalizeCancel := context.WithTimeout(
		context.WithoutCancel(self.ctx),
		finalizeTimeout,
	)
	defer finalizeCancel()

	task := r.task
	server.Tx(finalizeCtx, func(tx server.PgTx) {
		commitPosts = nil
		postRescheduled = false

		if r.err != nil {
			now := server.NowUtc()
			// Preserve the same task and its classified retry cadence/count.
			// Draining must not persist retry arguments from canceled work.
			delay, errorCountDelta := taskTargetErrorRetryDelay(
				self.targets[task.FunctionName],
				r.err,
				task.RescheduleErrorCount,
				mathrand.Float64(),
			)
			var retryArgsJson *string
			if self.ctx.Err() == nil && self.drainCtx.Err() == nil {
				retryArgsJson = taskRetryArgsJson(r.err)
			}
			server.RaisePgResult(tx.Exec(
				finalizeCtx,
				`
					UPDATE pending_task
					SET
						reschedule_error = $2,
						reschedule_error_count = pending_task.reschedule_error_count + $5,
						args_json = COALESCE($6, pending_task.args_json),
						run_at = $3,
						release_time = $4
					WHERE task_id = $1
				`,
				task.TaskId,
				r.err.Error(),
				now.Add(delay),
				now,
				errorCountDelta,
				retryArgsJson,
			))
			return
		}

		server.BatchInTx(finalizeCtx, tx, func(batch server.PgBatch) {
			batch.Queue(
				`
					INSERT INTO finished_task (
						task_id,
						function_name,
						args_json,
						client_address,
						client_address_hash,
						client_address_port,
						client_by_jwt_json,
						run_at,
						run_once_key,
						run_priority,
						run_max_time_seconds,
						run_start_time,
						run_end_time,
						reschedule_error,
						result_json
					)
					SELECT
						task_id,
						function_name,
						args_json,
						client_address,
						client_address_hash,
						client_address_port,
						client_by_jwt_json,
						run_at,
						run_once_key,
						run_priority,
						run_max_time_seconds,
						$2 AS run_start_time,
						$3 AS run_end_time,
						reschedule_error,
						$4 AS result_json
					FROM pending_task
					WHERE task_id = $1
				`,
				task.TaskId,
				r.runStartTime,
				r.runEndTime,
				r.resultJson,
			)
			batch.Queue(`DELETE FROM pending_task WHERE task_id = $1`, task.TaskId)
		})

		posts, err := r.runPost(tx)
		if err == nil {
			commitPosts = posts
			return
		}
		postRescheduled = true
		// Raise at the failed statement; an aborted transaction cannot persist
		// either the completion or its durable post retry.
		server.RaisePgResult(tx.Exec(
			finalizeCtx,
			`
				UPDATE finished_task
				SET
					post_error = $2,
					post_completed = false
				WHERE task_id = $1
			`,
			task.TaskId,
			err.Error(),
		))
		rescheduleTime := server.NowUtc().Add(time.Second * time.Duration(mathrand.Intn(int(RescheduleTimeout/time.Second))))
		clientSession, err := task.ClientSession(finalizeCtx)
		server.Raise(err)
		defer clientSession.Cancel()
		ScheduleTaskInTx(
			tx,
			self.RunPost,
			&RunPostArgs{TaskId: task.TaskId},
			clientSession,
			RunAt(rescheduleTime),
		)
	})
	return
}
