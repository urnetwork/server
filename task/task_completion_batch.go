// Explicitly certified, already-returned owners can share their durable handback.
package task

import (
	"context"
	"errors"
	"time"

	"github.com/urnetwork/server"
)

const taskCompletionBatchLimit = 64

// An opt-in certifies that successful execution has no transactional post,
// external post or continuation. Its required effects and replay authority
// already committed before Run returned. Errors keep ordinary finalization.
// The collector checks the registered target, not its invocation adapter.
type TaskCompletionBatchTarget interface {
	TaskCompletionBatchEnabled() bool
}

var errTaskCompletionBatchOwnership = errors.New("task completion batch ownership missing")

// Only observed successful results from explicitly registered owners qualify.
func (self *TaskWorker) canBatchTaskCompletion(r *taskExecutionResult) bool {
	if r == nil || r.task == nil || r.err != nil || r.runPost == nil {
		return false
	}
	owner, ok := self.targets[r.task.FunctionName].(TaskCompletionBatchTarget)
	return ok && owner.TaskCompletionBatchEnabled()
}

// Each row keeps its current durable payload and its own execution result/times.
// DELETE RETURNING supplies the exact payload removed by this transaction; a
// failed copy rolls the move back. The identity predicate bounds queue access.
const taskCompletionBatchSql = `WITH removed AS (
 DELETE FROM pending_task WHERE task_id=ANY($1::uuid[]) RETURNING *
), copied AS (
 INSERT INTO finished_task (
  task_id,function_name,args_json,client_address,client_address_hash,
  client_address_port,client_by_jwt_json,run_at,run_once_key,run_priority,
  run_max_time_seconds,run_start_time,run_end_time,reschedule_error,result_json)
 SELECT removed.task_id,removed.function_name,removed.args_json,
  removed.client_address,removed.client_address_hash,removed.client_address_port,
  removed.client_by_jwt_json,removed.run_at,removed.run_once_key,removed.run_priority,
  removed.run_max_time_seconds,observed.run_start_time,observed.run_end_time,
  removed.reschedule_error,observed.result_json
 FROM unnest($1::uuid[],$2::timestamp[],$3::timestamp[],$4::text[])
  AS observed(task_id,run_start_time,run_end_time,result_json)
 JOIN removed ON removed.task_id=observed.task_id
 RETURNING task_id
)
SELECT (SELECT count(*) FROM copied),(SELECT count(*) FROM removed)`

// The existing advisory guard remains held until the collector joins all work.
// A known pre-commit rollback may fall back to independent owners, so one refused
// row cannot poison its neighbors. Once the body succeeds, any commit error is
// left unresolved: no replay or completion count may assume acknowledgement.
func (self *TaskWorker) finalizeTaskBatch(results []*taskExecutionResult) (retrySingles bool, returnErr error) {
	if len(results) < 2 || len(results) > taskCompletionBatchLimit {
		return false, errors.New("invalid task completion batch size")
	}
	ids := make([]server.Id, 0, len(results))
	starts, ends := make([]time.Time, 0, len(results)), make([]time.Time, 0, len(results))
	values := make([]string, 0, len(results))
	seen := map[server.Id]bool{}
	for _, result := range results {
		if !self.canBatchTaskCompletion(result) || seen[result.task.TaskId] {
			return false, errors.New("invalid task completion batch member")
		}
		seen[result.task.TaskId] = true
		ids = append(ids, result.task.TaskId)
		starts, ends = append(starts, result.runStartTime), append(ends, result.runEndTime)
		values = append(values, result.resultJson)
	}
	timeout := self.settings.FinalizeTimeout
	if timeout <= 0 {
		timeout = DefaultTaskFinalizeTimeout
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(self.ctx), timeout)
	defer cancel()
	bodyComplete := false
	server.HandleError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			var copied, removed int
			server.Raise(tx.QueryRow(ctx, taskCompletionBatchSql, ids, starts, ends, values).Scan(&copied, &removed))
			if copied != len(results) || removed != len(results) {
				server.Raise(errTaskCompletionBatchOwnership)
			}
			bodyComplete = true
		}, server.OptNoRetry())
		if self.completionBatchCommitReturned != nil {
			self.completionBatchCommitReturned()
		}
	}, func(err error) { returnErr = err })
	return returnErr != nil && !bodyComplete && !errors.Is(returnErr, errTaskCompletionBatchOwnership), returnErr
}
