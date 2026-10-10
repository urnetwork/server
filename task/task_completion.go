// Ordinary returned tasks commit their own completion and continuation. Explicit
// no-post opt-ins may share handback; advisory ownership always remains until join.
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
	return self.finalizeTaskWithGuard(r, nil)
}

func (self *TaskWorker) finalizeTaskWithGuard(r *taskExecutionResult, guard *taskClaimGuard) (
	commitPosts []server.PostFunction,
	postRescheduled bool,
) {
	observation := newTaskFinalizationObservation()
	defer func() {
		if recovered := recover(); recovered != nil {
			self.observeTaskFinalizationFailure([]*taskExecutionResult{r}, recovered, observation)
			panic(recovered)
		}
	}()
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
	keys, owned, err := taskCompletionOwnershipKeys(self.targets[task.FunctionName], task, r.resultJson, r.err == nil)
	server.Raise(err)
	finish := func(tx server.PgTx) {
		commitPosts, postRescheduled = self.finalizeTaskInTx(finalizeCtx, tx, r, observation)
	}
	if owned {
		guard.finalizeOwnedTx(finalizeCtx, keys, finish, &observation.db)
	} else {
		server.Tx(finalizeCtx, finish, &observation.db)
	}
	return
}

// The caller owns one finite handback deadline and transaction. A certified
// cohort can share those owners across member errors without extending either
// budget, replaying a transaction, or borrowing another member's queue key.
func (self *TaskWorker) finalizeTaskInTx(finalizeCtx context.Context, tx server.PgTx, r *taskExecutionResult, observation *taskFinalizationObservation) (
	commitPosts []server.PostFunction,
	postRescheduled bool,
) {
	observation.body = taskFinalizationQueueUpdate
	task := r.task
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
		if r.collectorInterrupted {
			// The body was stopped to release a failed collector's custody.
			// Preserve its error and pending owner without growing task backoff.
			delay = errorRescheduleDelay(RescheduleTimeout, RescheduleBackoffMaxTimeout, 0, 0, mathrand.Float64())
			errorCountDelta = 0
		}
		var retryArgsJson *string
		if self.ctx.Err() == nil && self.drainCtx.Err() == nil {
			retryArgsJson = taskRetryArgsJson(r.err)
		}
		tag := server.RaisePgResult(tx.Exec(
			finalizeCtx,
			`
					UPDATE pending_task
					SET
						reschedule_error = $2,
						reschedule_error_count = pending_task.reschedule_error_count + $5,
						args_json = COALESCE($6, pending_task.args_json),
						run_at = CASE WHEN run_once_generation > $8 AND run_once_wake_at IS NOT NULL
							THEN LEAST($3, run_once_wake_at) ELSE $3 END,
						release_time = $4
					WHERE task_id = $1 AND claim_generation = $7
				`,
			task.TaskId,
			r.err.Error(),
			now.Add(delay),
			now,
			errorCountDelta,
			retryArgsJson,
			task.ClaimGeneration,
			task.RunOnceGeneration,
		))
		if tag.RowsAffected() != 1 {
			server.Raise(errTaskClaimOwnership)
		}
		return
	}

	wakeAt := finishTaskOwnerInTx(finalizeCtx, tx, r)

	observation.body = taskFinalizationTransactionalPost
	posts, err := r.runPost(tx)
	observation.body = taskFinalizationQueueUpdate
	if err == nil {
		commitPosts = posts
		taskRunOnceWakeAfterPost(finalizeCtx, tx, task.TaskId, wakeAt)
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
	taskRunOnceWakeAfterPost(finalizeCtx, tx, task.TaskId, wakeAt)
	return
}
