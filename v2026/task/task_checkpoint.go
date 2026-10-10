// Resumable targets retain acknowledged progress under the same pending owner.
package task

import (
	"context"
	"errors"
	"time"

	"github.com/urnetwork/server/v2026"
)

// A pure, bounded argument transformation for a target with durable progress.
// It runs while the exact claim row/key is owned, before claim COMMIT. It must
// perform no I/O, retain no state, and preserve all declared business owners.
// Ordinary targets do not implement this interface and keep their exact args.
type TaskClaimCheckpointTarget interface {
	TaskClaimCheckpointArgs(taskId server.Id, argsJson string, generation int64, wakeAt *time.Time) (string, error)
}

type taskClaimCheckpoint struct {
	target     TaskClaimCheckpointTarget
	taskId     server.Id
	argsJson   string
	generation int64
	claim      int64
	wakeAt     *time.Time
}

// The caller owns both its business publication and this exact pending key.
// One commit publishes work and advances its cursor; rollback advances neither.
func CheckpointTaskArgsInTx(ctx context.Context, tx server.PgTx, queued *Task, argsJson string) {
	if queued == nil || queued.TaskId == (server.Id{}) || queued.ClaimGeneration <= 0 ||
		!server.TxOwnsKeys(tx, []server.PgOwnershipKey{taskQueueOwnershipKey(queued.TaskId, queued.RunOnceKey)}) {
		panic(errors.New("task checkpoint requires the exact queue owner and claim"))
	}
	name := queued.storedFunctionName
	if name == "" {
		name = queued.FunctionName
	}
	var key *string
	if queued.RunOnceKey != "" {
		key = &queued.RunOnceKey
	}
	tag := server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET args_json=$5
        WHERE task_id=$1 AND claim_generation=$2 AND function_name=$3
          AND run_once_key IS NOT DISTINCT FROM $4`,
		queued.TaskId, queued.ClaimGeneration, name, key, argsJson))
	if tag.RowsAffected() != 1 {
		panic(errTaskClaimOwnership)
	}
}

// The normal finalizer owns the exact finished row and its pending key. A
// completed logical pass restores its original arguments before the framework
// publishes a concurrent wake. This same operation is safe on a RunPost retry.
func FinishTaskCheckpointInTx(ctx context.Context, tx server.PgTx, taskId server.Id, runOnceKey string,
	prepare func(string) (string, *time.Time, error),
) error {
	if runOnceKey == "" || !server.TxOwnsKeys(tx, []server.PgOwnershipKey{
		taskQueueOwnershipKey(taskId, runOnceKey), taskFinishedOwnershipKey(taskId),
	}) {
		return errors.New("task checkpoint finish requires its exact completed owner")
	}
	var argsJson string
	if err := tx.QueryRow(ctx, `SELECT args_json FROM finished_task WHERE task_id=$1 AND run_once_key=$2`, taskId, runOnceKey).Scan(&argsJson); err != nil {
		return err
	}
	original, wakeAt, err := prepare(argsJson)
	if err != nil {
		return err
	}
	if original != argsJson {
		if _, err := tx.Exec(ctx, `UPDATE finished_task SET args_json=$2 WHERE task_id=$1`, taskId, original); err != nil {
			return err
		}
	}
	taskRunOnceWakeAfterPost(ctx, tx, taskId, wakeAt)
	return nil
}

// A context-local timer seam lets native tests fire the real ordinary adapter
// after a proven commit, without changing global budgets or replacing work.
type taskRunTimerKey struct{}

func Testing_WithTaskRunAfter(ctx context.Context, after func(context.Context, time.Duration) <-chan time.Time) context.Context {
	return context.WithValue(ctx, taskRunTimerKey{}, after)
}
