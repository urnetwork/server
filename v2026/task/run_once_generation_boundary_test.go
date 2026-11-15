// Native lock edges prove both sides of the completion/scheduling boundary.
package task

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
)

func runOnceGenerationBlockingEdge(t testing.TB, ctx context.Context, waiter, holder int) {
	t.Helper()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		var blocked bool
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT $2=ANY(pg_blocking_pids($1))`, waiter, holder).Scan(&blocked))
		})
		if blocked {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("explicit completion/schedule blocking edge was not observed", ctx.Err())
		case <-tick.C:
		}
	}
}

// The existing causal controls commit scheduling first. Here completion owns
// the old unique key first; the producer must wait, then insert a distinct owner.
func TestRunOnceScheduleAfterCompletionLockKeepsSuccessor(t *testing.T) {
	runOnceGenerationEnv(t, func(t testing.TB, ctx context.Context) {
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		scope := server.NewId()
		id := ScheduleTask(runOnceGenerationWork, &runOnceGenerationArgs{Scope: scope}, owner,
			runOnceGenerationKey(scope), RunAt(server.NowUtc().Add(-time.Hour)))
		finishPid, producerPid := make(chan int, 1), make(chan int, 1)
		release := make(chan struct{})
		var resumed sync.Once
		target := &runOnceGenerationTarget{Target: NewTaskTarget(runOnceGenerationWork)}
		target.after = func(tx server.PgTx, _ *Task) error {
			var pid int
			server.Raise(tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid))
			finishPid <- pid
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		worker := runOnceGenerationWorker(ctx, target)
		done, scheduled := make(chan struct{}), make(chan struct{})
		var evalErr, scheduleErr error
		var finished, retried, posts []server.Id
		go func() {
			defer close(done)
			server.HandleError(func() { finished, retried, posts, evalErr = worker.EvalTasks(1) }, func(err error) { evalErr = err })
		}()
		startedProducer := false
		defer func() {
			resumed.Do(func() { close(release) })
			select {
			case <-done:
			case <-ctx.Done():
			}
			if startedProducer {
				select {
				case <-scheduled:
				case <-ctx.Done():
				}
			}
			worker.Close()
		}()
		var holder int
		select {
		case holder = <-finishPid:
		case <-ctx.Done():
			t.Fatal("completion did not hold the exact handoff boundary", ctx.Err())
		}
		startedProducer = true
		go func() {
			defer close(scheduled)
			server.HandleError(func() {
				server.Tx(ctx, func(tx server.PgTx) {
					var pid int
					server.Raise(tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid))
					select {
					case producerPid <- pid:
					default:
					}
					ScheduleTaskInTx(tx, runOnceGenerationWork, &runOnceGenerationArgs{Scope: scope}, owner,
						runOnceGenerationKey(scope), RunAt(server.NowUtc().Add(-time.Hour)))
				})
			}, func(err error) { scheduleErr = err })
		}()
		var waiter int
		select {
		case waiter = <-producerPid:
		case <-ctx.Done():
			t.Fatal("producer did not reach its actual insert", ctx.Err())
		}
		runOnceGenerationBlockingEdge(t, ctx, waiter, holder)
		resumed.Do(func() { close(release) })
		for _, joined := range []<-chan struct{}{done, scheduled} {
			select {
			case <-joined:
			case <-ctx.Done():
				t.Fatal("completion/schedule boundary did not join", ctx.Err())
			}
		}
		pending := runOnceGenerationPending(ctx, []server.Id{scope})
		if evalErr != nil || scheduleErr != nil || len(finished) != 1 || finished[0] != id || len(retried)+len(posts) != 0 ||
			len(pending) != 1 || pending[id] != nil || len(GetFinishedTasks(ctx, id)) != 1 {
			t.Fatal("schedule following the completion lock lost its distinct owner", evalErr, scheduleErr, pending)
		}
	})
}

func TestRunOnceBatchStaleMemberRollsBackCurrentNeighbor(t *testing.T) {
	runOnceGenerationEnv(t, func(t testing.TB, ctx context.Context) {
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		target := &runOnceGenerationTarget{Target: NewTaskTarget(runOnceGenerationWork), batch: true}
		worker := runOnceGenerationWorker(ctx, target)
		defer worker.Close()
		ids := make([]server.Id, 2)
		for index := range ids {
			scope := server.NewId()
			ids[index] = ScheduleTask(runOnceGenerationWork, &runOnceGenerationArgs{Scope: scope}, owner,
				runOnceGenerationKey(scope), RunAt(server.NowUtc().Add(-time.Hour)))
		}
		old, oldGuard, err := worker.takeTasks(2)
		if err != nil || oldGuard == nil || len(old) != 2 {
			t.Fatal("batch stale control failed its original claim", err)
		}
		defer oldGuard.release()
		stale := worker.executeTask(ctx, old[ids[0]], target)
		oldGuard.release()
		for _, id := range ids {
			ReleaseTask(ctx, id)
		}
		current, guard, err := worker.takeTasks(2)
		if err != nil || guard == nil || len(current) != 2 {
			t.Fatal("batch stale control failed its reclaim", err)
		}
		defer guard.release()
		healthy := worker.executeTask(ctx, current[ids[1]], target)
		retrySingles, err := worker.finalizeTaskBatch([]*taskExecutionResult{stale, healthy})
		if retrySingles || !errors.Is(err, errTaskCompletionBatchOwnership) || len(GetFinishedTasks(ctx, ids...)) != 0 || len(GetTasks(ctx, ids...)) != 2 {
			t.Fatal("stale batch member copied/deleted a current neighbor or authorized fallback", retrySingles, err)
		}
		for id, row := range GetTasks(ctx, ids...) {
			if row.ClaimGeneration != current[id].ClaimGeneration {
				t.Fatal("batch rollback changed reclaimed ownership")
			}
		}
	})
}

// The injected failure is after a real commit, not a socket simulation. The
// caller cannot acknowledge or replay it, but every successor remains durable.
func TestRunOnceBatchLostReplyRetainsCommittedSuccessors(t *testing.T) {
	runOnceGenerationEnv(t, func(t testing.TB, ctx context.Context) {
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		target := &runOnceGenerationTarget{Target: NewTaskTarget(runOnceGenerationWork), batch: true}
		worker := runOnceGenerationWorker(ctx, target)
		defer worker.Close()
		ids, scopes := make([]server.Id, 2), make([]server.Id, 2)
		for index := range ids {
			scopes[index] = server.NewId()
			ids[index] = ScheduleTask(runOnceGenerationWork, &runOnceGenerationArgs{Scope: scopes[index]}, owner,
				runOnceGenerationKey(scopes[index]), RunAt(server.NowUtc().Add(-time.Hour)))
		}
		claimed, guard, err := worker.takeTasks(2)
		if err != nil || guard == nil || len(claimed) != 2 {
			t.Fatal("lost reply control failed its actual claim", err)
		}
		defer guard.release()
		results := make([]*taskExecutionResult, 2)
		for index, id := range ids {
			results[index] = worker.executeTask(ctx, claimed[id], target)
			ScheduleTask(runOnceGenerationWork, &runOnceGenerationArgs{Scope: scopes[index]}, owner,
				runOnceGenerationKey(scopes[index]), RunAt(server.NowUtc().Add(-time.Hour)))
		}
		commits := 0
		worker.completionBatchCommitReturned = func() { commits++; panic(io.ErrUnexpectedEOF) }
		retrySingles, err := worker.finalizeTaskBatch(results)
		pending := runOnceGenerationPending(ctx, scopes)
		if retrySingles || !errors.Is(err, io.ErrUnexpectedEOF) || commits != 1 || len(pending) != 2 || len(GetFinishedTasks(ctx, ids...)) != 2 {
			t.Fatal("lost commit reply erased or replayed acknowledged durable wake work", retrySingles, err, commits)
		}
		for _, id := range ids {
			if pending[id] != nil {
				t.Fatal("lost reply retained the completed identity as its successor")
			}
		}
	})
}
