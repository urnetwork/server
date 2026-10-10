// Claim snapshots distinguish coalesced pending work from a wake during Run.
package task

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server"
)

var errTaskClaimOwnership = errors.New("task claim generation ownership missing")

// One statement moves the exact fenced owner and captures its wake. Deletion
// holds row/key ownership through the later transactional post and commit.
const taskCompletionOwnerSql = `WITH removed AS (
 DELETE FROM pending_task WHERE task_id=$1 AND claim_generation=$5 RETURNING *
), copied AS (
 INSERT INTO finished_task (
  task_id,function_name,args_json,client_address,client_address_hash,
  client_address_port,client_by_jwt_json,run_at,run_once_key,run_priority,
  run_max_time_seconds,run_start_time,run_end_time,reschedule_error,result_json)
 SELECT task_id,function_name,args_json,client_address,client_address_hash,
  client_address_port,client_by_jwt_json,run_at,run_once_key,run_priority,
  run_max_time_seconds,$2,$3,reschedule_error,$4 FROM removed
 RETURNING task_id
)
SELECT removed.run_once_generation,removed.run_once_wake_at
FROM removed JOIN copied USING(task_id)`

// A producer either precedes the exact owner deletion and leaves a generation,
// or follows its commit and inserts anew. The move rolls back on any refusal.
func finishTaskOwnerInTx(ctx context.Context, tx server.PgTx, r *taskExecutionResult) *time.Time {
	queued := r.task
	var generation int64
	var wakeAt *time.Time
	err := tx.QueryRow(ctx, taskCompletionOwnerSql, queued.TaskId, r.runStartTime,
		r.runEndTime, r.resultJson, queued.ClaimGeneration).Scan(&generation, &wakeAt)
	if errors.Is(err, pgx.ErrNoRows) {
		server.Raise(errTaskClaimOwnership)
	}
	server.Raise(err)
	server.AddTxCommitCount(tx, &taskFinishedCounter, 1)
	if queued.RunOnceKey == "" {
		return nil
	}
	if generation < queued.RunOnceGeneration {
		server.Raise(errors.New("run-once generation moved behind its claim"))
	}
	if generation == queued.RunOnceGeneration {
		return nil
	}
	if wakeAt == nil {
		server.Raise(errors.New("run-once wake generation has no requested time"))
	}
	return wakeAt
}

// A target's transactional post gets first choice of successor arguments. A
// concurrent wake can advance that successor's deadline, never replace its args.
// The old ID is finished; a fresh ID owns the next exact advisory/execution claim.
func taskRunOnceWakeAfterPost(ctx context.Context, tx server.PgTx, taskId server.Id, wakeAt *time.Time) {
	if wakeAt == nil {
		return
	}
	successorId := server.NewId()
	var inserted bool
	err := tx.QueryRow(ctx, `INSERT INTO pending_task (
        task_id,function_name,args_json,client_address,client_address_hash,
        client_address_port,client_by_jwt_json,run_at,run_once_key,run_priority,
        run_max_time_seconds,claim_time,release_time)
        SELECT $2,function_name,args_json,client_address,client_address_hash,
            client_address_port,client_by_jwt_json,$3,run_once_key,run_priority,
            run_max_time_seconds,$4,$4
        FROM finished_task WHERE task_id=$1
        ON CONFLICT (run_once_key) DO UPDATE SET
            run_at=LEAST(pending_task.run_at,EXCLUDED.run_at),
            run_priority=LEAST(pending_task.run_priority,EXCLUDED.run_priority),
            run_max_time_seconds=GREATEST(pending_task.run_max_time_seconds,EXCLUDED.run_max_time_seconds)
        RETURNING task_id=$2`,
		taskId, successorId, *wakeAt, time.Time{}).Scan(&inserted)
	if errors.Is(err, pgx.ErrNoRows) {
		server.Raise(errTaskClaimOwnership)
	}
	server.Raise(err)
	observeTaskSubmissionInTx(tx, inserted)
}
