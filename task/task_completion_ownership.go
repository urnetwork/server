// Supported queue-writing targets declare their complete post-publication key
// set before the finishing transaction begins. The executing task's durable
// queue key is always included, independently of its post declaration.
package task

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server"
)

// Implementing this contract opts all of this target's handbacks into the
// same-backend queue protocol. Extra keys must be derived from immutable scope
// and the completed result, without business SQL or side effects. Errors retain
// their original pending owner and require no post keys.
type TaskCompletionOwnershipTarget interface {
	TaskCompletionOwnershipKeys(queued *Task, resultJson string) ([]server.PgOwnershipKey, error)
}

func taskCompletionOwnershipKeys(target Target, queued *Task, resultJson string, successful bool) ([]server.PgOwnershipKey, bool, error) {
	declaration, owned := target.(TaskCompletionOwnershipTarget)
	if !owned {
		return nil, false, nil
	}
	keys := []server.PgOwnershipKey{
		taskQueueOwnershipKey(queued.TaskId, queued.RunOnceKey),
		taskFinishedOwnershipKey(queued.TaskId),
	}
	if successful {
		extra, err := declaration.TaskCompletionOwnershipKeys(queued, resultJson)
		if err != nil {
			return nil, true, err
		}
		keys = append(keys, extra...)
	}
	return keys, true, nil
}

// The retry task has its own pending owner, while its post acknowledges the
// original finished row. Both identities precede the handback transaction.
type taskPostRetryTarget struct {
	Target
}

func taskFinishedOwnershipKey(taskId server.Id) server.PgOwnershipKey {
	return server.NewPgOwnershipKey("finished_task/task_id", taskId)
}

func (self *taskPostRetryTarget) TaskCompletionOwnershipKeys(queued *Task, _ string) ([]server.PgOwnershipKey, error) {
	var args RunPostArgs
	if err := json.Unmarshal([]byte(queued.ArgsJson), &args); err != nil {
		return nil, err
	}
	if args.TaskId == (server.Id{}) {
		return nil, errors.New("post retry has no finished owner")
	}
	return []server.PgOwnershipKey{taskFinishedOwnershipKey(args.TaskId)}, nil
}

// A retry discovers its original payload before admission, then checks exactly
// that immutable publication authority under the finished-row owner. A changed
// payload cannot expand the predeclared key set through a stale callback.
func validateFinishedTaskPostOwner(ctx context.Context, tx server.PgTx, finished *FinishedTask, storedFunctionName string) (bool, error) {
	var functionName, argsJson, resultJson string
	var runOnceKey *string
	err := tx.QueryRow(ctx, `SELECT function_name,args_json,result_json,run_once_key
        FROM finished_task WHERE task_id=$1 FOR UPDATE`, finished.TaskId).Scan(
		&functionName, &argsJson, &resultJson, &runOnceKey)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	storedKey := ""
	if runOnceKey != nil {
		storedKey = *runOnceKey
	}
	if functionName != storedFunctionName || argsJson != finished.ArgsJson || resultJson != finished.ResultJson || storedKey != finished.RunOnceKey {
		return false, errors.New("post retry durable publication authority changed")
	}
	return true, nil
}
