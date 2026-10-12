// Finished history is kept for a day, or for a week while its Post still has
// a retry pending, and is then removed by one recurring cleanup task.
//
// A run removes eligible rows in short groups, each one read-committed
// transaction that seeks the (run_end_time, task_id) index from a keyset
// position, admits the exact finished-task owners of its candidates, and
// deletes them under row locks that skip busy rows. A run stops starting
// groups once its time budget is spent, well inside its max time, and its Post
// re-arms the successor at once from the position it reached. Resuming there
// instead of at the oldest row matters because the rows a run deleted stay in
// the index until vacuum, and while a long snapshot holds them visible every
// scan from the oldest row has to step through them again.
//
// A sweep that reaches the retention boundary re-arms after the cleanup
// interval and the next sweep starts at the oldest row again, so rows an
// earlier group skipped, or whose Post retry window has since ended, are
// revisited. A run that fails or times out keeps its pending row and retries
// within the error retry cap from the position it started at, instead of the
// generic backoff, which reaches an hour after a dozen consecutive failures.
//
// An old failed Post can still run, so age never substitutes for its finished
// owner: a group whose owners are not all free is left whole for a later
// sweep, and nothing waits on a busy owner or row.
package task

import (
	"context"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// Successful history is kept this long after its run ends.
const finishedTaskRetention = 24 * time.Hour

// History whose Post has not completed is kept this long for its retries.
const finishedTaskPostErrorRetention = 7 * 24 * time.Hour

// Candidates read, admitted and removed by one short transaction. Their
// owners fit one admission statement.
const finishedTaskRetentionGroupSize = 64

// A run stops starting groups after this, well inside its max time.
const taskCleanupBudget = 30 * time.Second

// The budget leaves a run's last group three quarters of this to finish.
const taskCleanupMaxTime = 2 * time.Minute

// A sweep that reached the retention boundary starts again after this.
const taskCleanupInterval = 10 * time.Minute

// A failed or timed-out run retries within this.
const taskCleanupErrorRetryCap = time.Minute

// A position in the (run_end_time, task_id) index order. The zero position
// precedes every row.
type FinishedTaskPosition struct {
	RunEndTime time.Time `json:"run_end_time"`
	TaskId     server.Id `json:"task_id"`
}

// The position and the boundary are index conditions, so a group seeks to its
// position and reads no further than the retention boundary. Both retention
// predicates are then checked on the rows it reads.
const finishedTaskRetentionCandidatesSql = `SELECT task_id, run_end_time FROM finished_task
	WHERE (run_end_time, task_id) > ($1::timestamp, $2::uuid)
		AND run_end_time < $3::timestamp
		AND ((run_end_time < $4::timestamp AND (post_error IS NULL OR post_completed)) OR run_end_time < $5::timestamp)
	ORDER BY run_end_time, task_id
	LIMIT $6`

// The admitted candidates are found once by primary key, rechecked, locked
// skipping rows another transaction holds, and deleted by row address.
const finishedTaskRetentionDeleteSql = `DELETE FROM finished_task
	WHERE ctid = ANY(ARRAY(
		SELECT ctid FROM finished_task
		WHERE task_id = ANY($1::uuid[])
			AND ((run_end_time < $2::timestamp AND (post_error IS NULL OR post_completed)) OR run_end_time < $3::timestamp)
		ORDER BY task_id
		FOR UPDATE SKIP LOCKED))
	AND task_id = ANY($1::uuid[])`

// One group: the next candidates after the position, admitted by their exact
// finished-task owners before any row lock, then removed. A refused group is
// left whole and a locked row is skipped, both for a later sweep. Returns the
// last candidate visited, the candidates visited and the rows removed.
func removeFinishedTaskGroup(
	ctx context.Context,
	after FinishedTaskPosition,
	minTime time.Time,
	postErrorMinTime time.Time,
) (next FinishedTaskPosition, visited int, removed int64) {
	bound := minTime
	if bound.Before(postErrorMinTime) {
		bound = postErrorMinTime
	}
	server.Tx(ctx, func(tx server.PgTx) {
		next = after
		visited = 0
		removed = 0
		taskIds := make([]server.Id, 0, finishedTaskRetentionGroupSize)
		rows, err := tx.Query(
			ctx,
			finishedTaskRetentionCandidatesSql,
			after.RunEndTime,
			after.TaskId,
			bound,
			minTime,
			postErrorMinTime,
			finishedTaskRetentionGroupSize,
		)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				var taskId server.Id
				var runEndTime time.Time
				server.Raise(rows.Scan(&taskId, &runEndTime))
				taskIds = append(taskIds, taskId)
				next = FinishedTaskPosition{RunEndTime: runEndTime, TaskId: taskId}
			}
		})
		visited = len(taskIds)
		if visited == 0 {
			return
		}
		keys := make([]server.PgOwnershipKey, len(taskIds))
		for index, taskId := range taskIds {
			keys[index] = taskFinishedOwnershipKey(taskId)
		}
		admitted, err := server.TryTxOwnershipExec(ctx, tx, keys)
		server.Raise(err)
		if !admitted {
			return
		}
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			finishedTaskRetentionDeleteSql,
			taskIds,
			minTime,
			postErrorMinTime,
		))
		removed = tag.RowsAffected()
	}, server.TxReadCommitted, server.OptNoRetry())
	return
}

// Removes eligible rows after the position one group at a time, until a group
// comes back short, which means the sweep reached the retention boundary, or
// until `stop` reports after a group that the caller's budget is spent. Every
// call completes at least one group. Returns where the sweep stopped.
func removeFinishedTasksAfter(
	ctx context.Context,
	after FinishedTaskPosition,
	minTime time.Time,
	postErrorMinTime time.Time,
	stop func() bool,
) (next FinishedTaskPosition, removeCount int64, complete bool) {
	next = after
	for {
		groupNext, visited, removed := removeFinishedTaskGroup(ctx, next, minTime, postErrorMinTime)
		next = groupNext
		removeCount += removed
		if visited < finishedTaskRetentionGroupSize {
			return next, removeCount, true
		}
		if stop() {
			return next, removeCount, false
		}
	}
}

// Removes every row past `minTime` whose Post completed, and every row past
// `postErrorMinTime`, in one complete sweep from the oldest row, using the same
// groups as the cleanup task. The recurring task bounds its sweeps by time.
func RemoveFinishedTasks(ctx context.Context, minTime time.Time, postErrorMinTime time.Time) (removeCount int64) {
	_, removeCount, _ = removeFinishedTasksAfter(
		ctx,
		FinishedTaskPosition{},
		minTime,
		postErrorMinTime,
		func() bool { return false },
	)
	return
}

// PERIODIC CLEANUP

type TaskCleanupArgs struct {
	// Where this run resumes; nil starts a sweep at the oldest row.
	After *FinishedTaskPosition `json:"after,omitempty"`
}

type TaskCleanupResult struct {
	// Where the successor resumes; nil when the sweep reached the retention
	// boundary.
	Next    *FinishedTaskPosition `json:"next,omitempty"`
	Removed int64                 `json:"removed"`
}

// Startup arms a run now. A pending run keeps its position and only moves
// earlier, so a deploy resumes a stalled cleanup at once.
func ScheduleTaskCleanup(clientSession *session.ClientSession, tx server.PgTx) {
	scheduleTaskCleanup(clientSession, tx, &TaskCleanupArgs{}, server.NowUtc())
}

func scheduleTaskCleanup(clientSession *session.ClientSession, tx server.PgTx, args *TaskCleanupArgs, runAt time.Time) {
	ScheduleTaskInTx(
		tx,
		TaskCleanup,
		args,
		clientSession,
		RunOnce("task_cleanup"),
		RunAt(runAt),
		MaxTime(taskCleanupMaxTime),
	)
}

// A context-local clock lets tests run the budget on synthetic time.
type taskCleanupClockKey struct{}

func taskCleanupNow(ctx context.Context) func() time.Time {
	if now, ok := ctx.Value(taskCleanupClockKey{}).(func() time.Time); ok {
		return now
	}
	return server.NowUtc
}

// One budgeted run from the position in its arguments. Retention is measured
// from the run's start.
func TaskCleanup(
	taskCleanup *TaskCleanupArgs,
	clientSession *session.ClientSession,
) (*TaskCleanupResult, error) {
	now := taskCleanupNow(clientSession.Ctx)
	startTime := now()
	after := FinishedTaskPosition{}
	if taskCleanup.After != nil {
		after = *taskCleanup.After
	}
	next, removeCount, complete := removeFinishedTasksAfter(
		clientSession.Ctx,
		after,
		startTime.Add(-finishedTaskRetention),
		startTime.Add(-finishedTaskPostErrorRetention),
		func() bool {
			return taskCleanupBudget <= now().Sub(startTime)
		},
	)
	result := &TaskCleanupResult{Removed: removeCount}
	if !complete {
		result.Next = &next
	}
	glog.Infof("[task]cleanup removed=%d complete=%t\n", removeCount, complete)
	return result, nil
}

// A run the budget stopped continues at once from its position; a complete
// sweep starts the next one at the oldest row after the interval.
func TaskCleanupPost(
	taskCleanup *TaskCleanupArgs,
	taskCleanupResult *TaskCleanupResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	if taskCleanupResult.Next != nil {
		scheduleTaskCleanup(clientSession, tx, &TaskCleanupArgs{After: taskCleanupResult.Next}, server.NowUtc())
	} else {
		scheduleTaskCleanup(clientSession, tx, &TaskCleanupArgs{}, server.NowUtc().Add(taskCleanupInterval))
	}
	return nil
}

// The registered cleanup target. A failed or timed-out run retries within the
// error retry cap, so one slow run cannot stretch the cadence to the hour-long
// generic backoff.
func NewTaskCleanupTaskTarget() Target {
	return WithErrorRetryCap(
		NewTaskTargetWithPost(TaskCleanup, TaskCleanupPost, "main.TaskCleanup"),
		taskCleanupErrorRetryCap,
	)
}
