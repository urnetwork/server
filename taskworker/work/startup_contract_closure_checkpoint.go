// Publication progress belongs to the exact pending startup claim, not a new
// page task. Each committed child chunk carries its cursor in the same commit.
package work

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/task"
)

type startupContractClosureProgress struct {
	TaskId       server.Id  `json:"task_id"`
	After        server.Id  `json:"after"`
	RetryFrom    *server.Id `json:"retry_from,omitempty"`
	OriginalArgs string     `json:"original_args"`
	Generation   int64      `json:"generation"`
	RerunAt      *time.Time `json:"rerun_at,omitempty"`
}

type startupContractClosureCheckpointKey struct{}
type startupContractClosureCheckpoint struct {
	queued    *task.Task
	key       server.PgOwnershipKey
	progress  *startupContractClosureProgress
	published bool
}

func parseStartupContractClosureCheckpoint(raw string) (*ScheduleOpenContractClosuresArgs, error) {
	if len(raw) > 32*1024 {
		return nil, fmt.Errorf("startup contract scan checkpoint exceeds bounded arguments")
	}
	var args ScheduleOpenContractClosuresArgs
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		return nil, err
	}
	if args.Progress == nil {
		return &args, nil
	}
	progress := args.Progress
	var original ScheduleOpenContractClosuresArgs
	if progress.TaskId == (server.Id{}) || len(progress.OriginalArgs) == 0 || len(progress.OriginalArgs) > 4096 || progress.Generation < 0 ||
		json.Unmarshal([]byte(progress.OriginalArgs), &original) != nil || original.Progress != nil ||
		original.StartedAt.IsZero() || !original.StartedAt.Equal(args.StartedAt) || original.PageSize != args.PageSize ||
		(progress.RetryFrom != nil && progress.After.Cmp(*progress.RetryFrom) < 0) ||
		(progress.RerunAt != nil && progress.RerunAt.IsZero()) {
		return nil, fmt.Errorf("invalid startup contract scan checkpoint")
	}
	return &args, nil
}

// This pure claim-time fold commits before the old wake is cleared. Even a
// process exit immediately after claim cannot erase a requested fresh pass.
func (self *startupContractClosureTarget) TaskClaimCheckpointArgs(taskId server.Id, raw string, generation int64, wakeAt *time.Time) (string, error) {
	args, err := parseStartupContractClosureCheckpoint(raw)
	if err != nil {
		return raw, err
	}
	if args.Progress == nil {
		return raw, nil
	}
	progress := args.Progress
	// A legacy worker may finish without understanding this progress. A new
	// pending identity always owns a fresh head pass, never the old cursor.
	if progress.TaskId != taskId {
		return progress.OriginalArgs, nil
	}
	if generation < progress.Generation {
		return raw, fmt.Errorf("startup checkpoint generation moved backwards")
	}
	if generation == progress.Generation {
		return raw, nil
	}
	// An old worker can clear a wake without folding it into this state.
	// A full head pass conservatively absorbs that already-claimed request.
	if wakeAt == nil {
		return progress.OriginalArgs, nil
	}
	if progress.RerunAt == nil || wakeAt.Before(*progress.RerunAt) {
		copyWake := *wakeAt
		progress.RerunAt = &copyWake
	}
	progress.Generation = generation
	encoded, err := json.Marshal(args)
	return string(encoded), err
}

func (self *startupContractClosureCheckpoint) write(ctx context.Context, tx server.PgTx, args *ScheduleOpenContractClosuresArgs, progress *startupContractClosureProgress) {
	next := *args
	next.Private = true
	next.Progress = progress
	encoded, err := json.Marshal(&next)
	server.Raise(err)
	task.CheckpointTaskArgsInTx(ctx, tx, self.queued, string(encoded))
}

func (self *startupContractClosureTarget) Run(ctx context.Context, queued *task.Task) (any, func(server.PgTx) ([]server.PostFunction, error), error) {
	args, err := parseStartupContractClosureCheckpoint(queued.ArgsJson)
	if err != nil {
		return nil, nil, err
	}
	if queued.RunOnceKey == "" || queued.ClaimGeneration <= 0 {
		return self.Target.Run(ctx, queued)
	}
	progress := args.Progress
	if progress != nil && progress.TaskId != queued.TaskId {
		copyTask := *queued
		copyTask.ArgsJson = progress.OriginalArgs
		return self.Run(ctx, &copyTask)
	}
	if progress == nil {
		if len(queued.ArgsJson) > 4096 {
			return nil, nil, fmt.Errorf("startup contract scan arguments exceed checkpoint bound")
		}
		progress = &startupContractClosureProgress{TaskId: queued.TaskId, OriginalArgs: queued.ArgsJson, Generation: queued.RunOnceGeneration}
	} else if progress.Generation != queued.RunOnceGeneration {
		return nil, nil, fmt.Errorf("startup checkpoint wake was not retained by its claim")
	}
	checkpoint := &startupContractClosureCheckpoint{
		queued: queued, key: task.PendingTaskOwnershipKey(queued.TaskId, &queued.RunOnceKey), progress: progress,
	}
	result, post, err := self.Target.Run(context.WithValue(ctx, startupContractClosureCheckpointKey{}, checkpoint), queued)
	if err != nil {
		if checkpoint.published {
			err = task.WithCommittedProgressRetry(err)
		}
		return result, post, err
	}
	return result, func(tx server.PgTx) ([]server.PostFunction, error) {
		var posts []server.PostFunction
		if post != nil {
			var err error
			posts, err = post(tx)
			if err != nil {
				return nil, err
			}
		}
		postCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), task.DefaultTaskFinalizeTimeout)
		defer cancel()
		return posts, finishStartupContractClosureCheckpoint(postCtx, tx, queued.TaskId, queued.RunOnceKey)
	}, nil
}

// Restore only inside the successful finalizer. A timeout/rollback after EOF
// keeps its tail cursor, while every real successor starts from the head with
// byte-identical original arguments and the earliest retained producer wake.
func finishStartupContractClosureCheckpoint(ctx context.Context, tx server.PgTx, id server.Id, key string) error {
	return task.FinishTaskCheckpointInTx(ctx, tx, id, key, func(raw string) (string, *time.Time, error) {
		args, err := parseStartupContractClosureCheckpoint(raw)
		if err != nil {
			return raw, nil, err
		}
		if args.Progress == nil {
			return raw, nil, nil
		}
		return args.Progress.OriginalArgs, args.Progress.RerunAt, nil
	})
}

func (self *startupContractClosureTarget) RunPost(ctx context.Context, finished *task.FinishedTask, tx server.PgTx) ([]server.PostFunction, error) {
	posts, err := self.Target.RunPost(ctx, finished, tx)
	if err != nil {
		return nil, err
	}
	return posts, finishStartupContractClosureCheckpoint(ctx, tx, finished.TaskId, finished.RunOnceKey)
}
