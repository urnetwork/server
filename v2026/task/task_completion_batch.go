// Explicitly certified, already-returned owners can share their durable handback.
package task

import (
	"context"
	"errors"
	"time"

	"github.com/urnetwork/server/v2026"
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
var errTaskCompletionBatchMode = errors.New("task completion batch ownership modes differ")

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
 DELETE FROM pending_task WHERE task_id=ANY($1::uuid[])
  AND (task_id,claim_generation) IN (SELECT * FROM unnest($1::uuid[],$8::bigint[]))
 RETURNING *
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
), successors AS (
 INSERT INTO pending_task (
  task_id,function_name,args_json,client_address,client_address_hash,
  client_address_port,client_by_jwt_json,run_at,run_once_key,run_priority,
  run_max_time_seconds,claim_time,release_time)
 SELECT observed.successor_id,removed.function_name,removed.args_json,
  removed.client_address,removed.client_address_hash,removed.client_address_port,
  removed.client_by_jwt_json,removed.run_once_wake_at,removed.run_once_key,
  removed.run_priority,removed.run_max_time_seconds,$7,$7
 FROM unnest($1::uuid[],$5::bigint[],$6::uuid[])
  AS observed(task_id,claimed_generation,successor_id)
 JOIN removed ON removed.task_id=observed.task_id
 WHERE removed.run_once_key IS NOT NULL
  AND removed.run_once_generation > observed.claimed_generation
 ON CONFLICT (run_once_key) DO UPDATE SET
  run_at=LEAST(pending_task.run_at,EXCLUDED.run_at),
  run_priority=LEAST(pending_task.run_priority,EXCLUDED.run_priority),
  run_max_time_seconds=GREATEST(pending_task.run_max_time_seconds,EXCLUDED.run_max_time_seconds)
 RETURNING task_id
)
SELECT (SELECT count(*) FROM copied),(SELECT count(*) FROM removed),
 (SELECT count(*) FROM successors),
 (SELECT count(*) FROM removed
  JOIN unnest($1::uuid[],$5::bigint[]) AS observed(task_id,claimed_generation)
   ON removed.task_id=observed.task_id
  WHERE removed.run_once_key IS NOT NULL
   AND removed.run_once_generation > observed.claimed_generation),
 (SELECT count(*) FROM removed
  JOIN unnest($1::uuid[],$5::bigint[]) AS observed(task_id,claimed_generation)
   ON removed.task_id=observed.task_id
  WHERE removed.run_once_key IS NOT NULL
   AND removed.run_once_generation < observed.claimed_generation)`

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
	generations := make([]int64, 0, len(results))
	claims := make([]int64, 0, len(results))
	successorIds := make([]server.Id, 0, len(results))
	seen := map[server.Id]bool{}
	keys := make([]server.PgOwnershipKey, 0, len(results))
	owned := false
	for index, result := range results {
		if !self.canBatchTaskCompletion(result) || seen[result.task.TaskId] {
			return false, errors.New("invalid task completion batch member")
		}
		seen[result.task.TaskId] = true
		memberKeys, memberOwned, err := taskCompletionOwnershipKeys(self.targets[result.task.FunctionName], result.task, result.resultJson, true)
		if err != nil {
			return false, err
		}
		if index == 0 {
			owned = memberOwned
		} else if memberOwned != owned {
			// A generic target keeps its original backend/isolation policy.
			// No transaction has started, so both collectors can finish each
			// member independently through their existing safe fallback.
			return true, errTaskCompletionBatchMode
		}
		if memberOwned {
			keys = append(keys, memberKeys...)
		}
		ids = append(ids, result.task.TaskId)
		starts, ends = append(starts, result.runStartTime), append(ends, result.runEndTime)
		values = append(values, result.resultJson)
		generations = append(generations, result.task.RunOnceGeneration)
		claims = append(claims, result.task.ClaimGeneration)
		successorIds = append(successorIds, server.NewId())
	}
	timeout := self.settings.FinalizeTimeout
	if timeout <= 0 {
		timeout = DefaultTaskFinalizeTimeout
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(self.ctx), timeout)
	defer cancel()
	bodyComplete := false
	server.HandleError(func() {
		finish := func(tx server.PgTx) {
			var copied, removed, successors, expectedSuccessors, invalidGenerations int
			server.Raise(tx.QueryRow(ctx, taskCompletionBatchSql, ids, starts, ends, values,
				generations, successorIds, time.Time{}, claims).Scan(&copied, &removed, &successors, &expectedSuccessors, &invalidGenerations))
			if copied != len(results) || removed != len(results) || successors != expectedSuccessors || invalidGenerations != 0 {
				server.Raise(errTaskCompletionBatchOwnership)
			}
			bodyComplete = true
		}
		if owned {
			server.OwnedTx(ctx, keys, finish, server.TxReadCommitted, server.OptNoRetry())
		} else {
			server.Tx(ctx, finish, server.OptNoRetry())
		}
		if self.completionBatchCommitReturned != nil {
			self.completionBatchCommitReturned()
		}
	}, func(err error) { returnErr = err })
	return returnErr != nil && !bodyComplete && !errors.Is(returnErr, errTaskCompletionBatchOwnership), returnErr
}
