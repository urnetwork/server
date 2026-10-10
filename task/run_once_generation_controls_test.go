// Claim boundaries, target-owned continuations and recovery retain wake custody.
package task

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

func runOnceGenerationWorker(ctx context.Context, target *runOnceGenerationTarget) *TaskWorker {
	settings := DefaultTaskWorkerSettings()
	settings.ClaimRegisteredTargetsOnly = true
	worker := NewTaskWorker(ctx, settings)
	worker.AddTargets(target)
	return worker
}

func TestRunOncePendingSchedulesCoalesceBeforeClaim(t *testing.T) {
	runOnceGenerationEnv(t, func(t testing.TB, ctx context.Context) {
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		scope := server.NewId()
		for range 4 {
			ScheduleTask(runOnceGenerationWork, &runOnceGenerationArgs{Scope: scope}, owner,
				runOnceGenerationKey(scope), RunAt(server.NowUtc().Add(-time.Hour)))
		}
		pending := runOnceGenerationPending(ctx, []server.Id{scope})
		if len(pending) != 1 {
			t.Fatal("pending schedules did not coalesce before claim", len(pending))
		}
		worker := runOnceGenerationWorker(ctx, &runOnceGenerationTarget{Target: NewTaskTarget(runOnceGenerationWork)})
		defer worker.Close()
		finished, retried, posts, err := worker.EvalTasks(1)
		if err != nil || len(finished) != 1 || len(retried)+len(posts) != 0 || pending[finished[0]] == nil ||
			len(runOnceGenerationPending(ctx, []server.Id{scope})) != 0 {
			t.Fatal("pre-claim duplicates created unnecessary follow-up work", finished, retried, posts, err)
		}
	})
}

func TestRunOnceClaimSnapshotPrecedesExactRowRead(t *testing.T) {
	runOnceGenerationEnv(t, func(t testing.TB, ctx context.Context) {
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		scope := server.NewId()
		oldId := ScheduleTask(runOnceGenerationWork, &runOnceGenerationArgs{Scope: scope}, owner,
			runOnceGenerationKey(scope), RunAt(server.NowUtc().Add(-time.Hour)))
		worker := runOnceGenerationWorker(ctx, &runOnceGenerationTarget{Target: NewTaskTarget(runOnceGenerationWork)})
		defer worker.Close()
		var wake sync.Once
		worker.claimAfterCommit = func(*taskClaimGuard) {
			wake.Do(func() {
				ScheduleTask(runOnceGenerationWork, &runOnceGenerationArgs{Scope: scope}, owner,
					runOnceGenerationKey(scope), RunAt(server.NowUtc().Add(-time.Hour)))
			})
		}
		finished, retried, posts, err := worker.EvalTasks(1)
		pending := runOnceGenerationPending(ctx, []server.Id{scope})
		if err != nil || len(finished) != 1 || finished[0] != oldId || len(retried)+len(posts) != 0 ||
			len(pending) != 1 || pending[oldId] != nil {
			t.Fatal("post-claim wake was swallowed by the later task metadata read", pending, err)
		}
	})
}

func TestRunOnceTransactionalPostKeepsCursorAndRequestedTime(t *testing.T) {
	runOnceGenerationEnv(t, func(t testing.TB, ctx context.Context) {
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		scope := server.NewId()
		wakeAt := server.NowUtc().Add(2 * time.Minute).Truncate(time.Microsecond)
		postAt := wakeAt.Add(10 * time.Minute)
		target := &runOnceGenerationTarget{Target: NewTaskTarget(runOnceGenerationWork)}
		target.before = func(context.Context, *Task) error {
			// A later deadline cannot overwrite the earliest post-claim request.
			for _, at := range []time.Time{postAt.Add(time.Hour), wakeAt, wakeAt.Add(time.Minute)} {
				ScheduleTask(runOnceGenerationWork, &runOnceGenerationArgs{Scope: scope, Cursor: 66}, owner,
					runOnceGenerationKey(scope), RunAt(at))
			}
			return nil
		}
		target.after = func(tx server.PgTx, _ *Task) error {
			ScheduleTaskInTx(tx, runOnceGenerationWork, &runOnceGenerationArgs{Scope: scope, Cursor: 99}, owner,
				runOnceGenerationKey(scope), RunAt(postAt))
			return nil
		}
		oldId := ScheduleTask(runOnceGenerationWork, &runOnceGenerationArgs{Scope: scope, Cursor: 7}, owner,
			runOnceGenerationKey(scope), RunAt(server.NowUtc().Add(-time.Hour)))
		before := taskLifecycleCounts()
		worker := runOnceGenerationWorker(ctx, target)
		defer worker.Close()
		finished, retried, posts, err := worker.EvalTasks(1)
		requireTaskLifecycleDelta(t, before, 1, 1, 4)
		pending := runOnceGenerationPending(ctx, []server.Id{scope})
		if err != nil || len(finished) != 1 || finished[0] != oldId || len(retried)+len(posts) != 0 || len(pending) != 1 {
			t.Fatal("transactional successor and dirty wake did not coalesce", err, pending)
		}
		for id, row := range pending {
			var args runOnceGenerationArgs
			server.Raise(json.Unmarshal([]byte(row.ArgsJson), &args))
			if id == oldId || args.Scope != scope || args.Cursor != 99 || !row.RunAt.Equal(wakeAt) {
				t.Fatal("wake replaced the Post cursor or lost its future deadline", args, row.RunAt, wakeAt)
			}
		}
		if finished, retried, posts, err := worker.EvalTasks(1); err != nil || len(finished)+len(retried)+len(posts) != 0 {
			t.Fatal("future wake became prematurely eligible", err)
		}
	})
}

func TestRunOnceFailedExecutionKeepsWakeForRetry(t *testing.T) {
	runOnceGenerationEnv(t, func(t testing.TB, ctx context.Context) {
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		scope := server.NewId()
		oldId := ScheduleTask(runOnceGenerationWork, &runOnceGenerationArgs{Scope: scope}, owner,
			runOnceGenerationKey(scope), RunAt(server.NowUtc().Add(-time.Hour)))
		target := &runOnceGenerationTarget{Target: NewTaskTarget(runOnceGenerationWork)}
		calls := 0
		wakeAt := server.NowUtc().Add(-time.Hour).Truncate(time.Microsecond)
		target.before = func(context.Context, *Task) error {
			calls++
			if calls == 1 {
				ScheduleTask(runOnceGenerationWork, &runOnceGenerationArgs{Scope: scope}, owner,
					runOnceGenerationKey(scope), RunAt(wakeAt))
				return io.ErrUnexpectedEOF
			}
			return nil
		}
		worker := runOnceGenerationWorker(ctx, target)
		defer worker.Close()
		finished, retried, posts, err := worker.EvalTasks(1)
		pending := GetTasks(ctx, oldId)[oldId]
		if err != nil || len(finished)+len(posts) != 0 || len(retried) != 1 || retried[0] != oldId || pending == nil ||
			pending.RescheduleErrorCount != 1 || pending.RunOnceGeneration != 1 || len(GetFinishedTasks(ctx, oldId)) != 0 {
			t.Fatal("failed invocation lost its exact pending wake/retry owner", err, pending)
		}
		if !pending.RunAt.Equal(wakeAt) {
			t.Fatal("failed invocation replaced the producer's earlier RunOnce wake with error backoff", pending.RunAt, wakeAt)
		}
		// The error handback releases at now. Its generated availability block
		// is therefore still later than an immediate poll, even for a past wake.
		// Advance only the existing claim-clock seam to that exact stored block;
		// no manual run_at/lease update or second producer is needed.
		var availableBlock int64
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT available_block FROM pending_task WHERE task_id=$1`, oldId).Scan(&availableBlock))
		})
		if availableBlock <= pending.ReleaseTime.Unix()/BlockSizeSeconds || pending.ReleaseTime.Unix()/BlockSizeSeconds+2 < availableBlock {
			t.Fatal("retry eligibility did not retain the handback's next availability block")
		}
		worker.claimNow = func() time.Time { return time.Unix(availableBlock*BlockSizeSeconds, 0) }
		finished, retried, posts, err = worker.EvalTasks(1)
		if err != nil || len(finished) != 1 || finished[0] != oldId || len(retried)+len(posts) != 0 ||
			len(runOnceGenerationPending(ctx, []server.Id{scope})) != 0 || calls != 2 {
			t.Fatal("retry did not absorb the already pending generation", finished, err)
		}
	})
}

func TestRunOnceAbandonedClaimKeepsWakeForRecovery(t *testing.T) {
	runOnceGenerationEnv(t, func(t testing.TB, ctx context.Context) {
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		scope := server.NewId()
		id := ScheduleTask(runOnceGenerationWork, &runOnceGenerationArgs{Scope: scope}, owner,
			runOnceGenerationKey(scope), RunAt(server.NowUtc().Add(-time.Hour)))
		worker := runOnceGenerationWorker(ctx, &runOnceGenerationTarget{Target: NewTaskTarget(runOnceGenerationWork)})
		defer worker.Close()
		claimed, guard, err := worker.takeTasks(1)
		if err != nil || guard == nil || claimed[id] == nil {
			t.Fatal("abandonment control did not acquire a real owner", err)
		}
		defer guard.release()
		ScheduleTask(runOnceGenerationWork, &runOnceGenerationArgs{Scope: scope}, owner,
			runOnceGenerationKey(scope), RunAt(server.NowUtc().Add(-time.Hour)))
		// Drop the actual advisory session and explicitly expire its recovery
		// lease. No process crash or elapsed lease timeout is simulated here.
		guard.release()
		if !ReleaseTask(ctx, id) {
			t.Fatal("abandoned owner disappeared before recovery")
		}
		finished, retried, posts, err := worker.EvalTasks(1)
		if err != nil || len(finished) != 1 || finished[0] != id || len(retried)+len(posts) != 0 ||
			len(runOnceGenerationPending(ctx, []server.Id{scope})) != 0 {
			t.Fatal("recovery failed to absorb the wake retained by an abandoned claim", err)
		}
	})
}

// Lease renewal leaves claim generation stable; a new claim changes it. Both
// successful and failing stale handbacks must refuse the new exact owner.
func TestRunOnceStaleFinalizerCannotRetireReclaimedOwner(t *testing.T) {
	runOnceGenerationEnv(t, func(t testing.TB, ctx context.Context) {
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		scope := server.NewId()
		id := ScheduleTask(runOnceGenerationWork, &runOnceGenerationArgs{Scope: scope}, owner,
			runOnceGenerationKey(scope), RunAt(server.NowUtc().Add(-time.Hour)))
		worker := runOnceGenerationWorker(ctx, &runOnceGenerationTarget{Target: NewTaskTarget(runOnceGenerationWork)})
		defer worker.Close()
		old, guard, err := worker.takeTasks(1)
		if err != nil || guard == nil || old[id] == nil {
			t.Fatal("stale-owner control failed its first claim", err)
		}
		defer guard.release()
		refreshTaskTimestampLeases(ctx, old)
		if renewed := GetTasks(ctx, id)[id]; renewed == nil || renewed.ClaimGeneration != old[id].ClaimGeneration {
			t.Fatal("heartbeat changed the execution fence")
		}
		guard.release()
		ReleaseTask(ctx, id)
		current, currentGuard, err := worker.takeTasks(1)
		if err != nil || currentGuard == nil || current[id] == nil || current[id].ClaimGeneration <= old[id].ClaimGeneration {
			t.Fatal("reclaim did not advance the exact execution fence", err)
		}
		defer currentGuard.release()
		postCalls := 0
		stale := &taskExecutionResult{task: old[id], resultJson: "{}", runStartTime: server.NowUtc(), runEndTime: server.NowUtc(),
			runPost: func(server.PgTx) ([]server.PostFunction, error) { postCalls++; return nil, nil }}
		var caught error
		server.HandleError(func() { worker.finalizeTask(stale) }, func(err error) { caught = err })
		if !errors.Is(caught, errTaskClaimOwnership) || postCalls != 0 {
			t.Fatal("stale success retired or posted over the reclaimed owner", caught)
		}
		stale.err = io.ErrUnexpectedEOF
		caught = nil
		server.HandleError(func() { worker.finalizeTask(stale) }, func(err error) { caught = err })
		if !errors.Is(caught, errTaskClaimOwnership) {
			t.Fatal("stale failure rescheduled the reclaimed owner", caught)
		}
		before := GetTasks(ctx, id)[id]
		refreshTaskTimestampLeases(ctx, old)
		after := GetTasks(ctx, id)[id]
		if before == nil || after == nil || !before.ClaimTime.Equal(after.ClaimTime) || !before.ReleaseTime.Equal(after.ReleaseTime) ||
			after.ClaimGeneration != current[id].ClaimGeneration || after.RescheduleErrorCount != 0 || len(GetFinishedTasks(ctx, id)) != 0 {
			t.Fatal("stale heartbeat or finalizer changed current ownership")
		}
	})
}
