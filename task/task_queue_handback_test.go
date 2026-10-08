// Producer and completed-task owners use real active functions and committed
// queue rows. An admission refusal is the positive ordering witness; no sleep
// or negative arrival timeout decides whether a financial transaction ran.
package task

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

func runTaskQueueFutureHandback(t *testing.T, batch bool) {
	runOnceGenerationEnv(t, func(t testing.TB, ctx context.Context) {
		count := 1
		if batch {
			count = 2
		}
		var reruns atomic.Int64
		ctx = server.Testing_WithTxRerunHook(ctx, func() { reruns.Add(1) })
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		active, published := make(chan struct{}, count), make(chan struct{}, count)
		functionsRelease, producerRelease := make(chan struct{}), make(chan struct{})
		waiting, admissionRelease := make(chan struct{}), make(chan struct{})
		var functionOnce, producerOnce, waitingOnce, admissionOnce sync.Once
		defer functionOnce.Do(func() { close(functionsRelease) })
		defer producerOnce.Do(func() { close(producerRelease) })
		defer admissionOnce.Do(func() { close(admissionRelease) })
		workerCtx := server.Testing_WithPgOwnershipObservation(ctx, func(event server.PgOwnershipEvent) {
			if event.Kind == server.PgOwnershipWaiting {
				waitingOnce.Do(func() { close(waiting) })
				taskQueueWait(ctx, admissionRelease)
			}
		})
		worker := NewTaskWorkerWithDefaults(workerCtx)
		defer worker.Close()
		baseTime := server.NowUtc().Add(2 * time.Minute).Truncate(time.Second)
		expectedAt := baseTime.Add(20 * time.Second)
		target := &taskQueueProtocolTarget{runOnceGenerationTarget: &runOnceGenerationTarget{
			Target: NewTaskTarget(runOnceGenerationWork), batch: batch,
		}}
		target.before = func(runCtx context.Context, _ *Task) error {
			active <- struct{}{}
			taskQueueWait(runCtx, functionsRelease)
			return nil
		}
		if !batch {
			target.after = func(tx server.PgTx, queued *Task) error {
				var args runOnceGenerationArgs
				if err := json.Unmarshal([]byte(queued.ArgsJson), &args); err != nil {
					return err
				}
				ScheduleTaskInTx(tx, runOnceGenerationWork, &runOnceGenerationArgs{Scope: args.Scope, Cursor: 99}, owner,
					runOnceGenerationKey(args.Scope), RunAt(baseTime.Add(60*time.Second)), RequireQueueOwnership(tx))
				return nil
			}
		}
		worker.AddTargets(target)
		worker.completionResultPublished = func() { published <- struct{}{} }
		never := make(chan time.Time)
		first := true
		worker.heartbeatAfter = func(time.Duration) <-chan time.Time {
			if first {
				first = false
				for range count {
					taskQueueWait(ctx, published)
				}
			}
			return never
		}
		batchCommits := 0
		worker.completionBatchCommitReturned = func() { batchCommits++ }
		scopes, originals := make([]server.Id, count), make([]server.Id, count)
		keys := make([]server.PgOwnershipKey, count)
		for index := range count {
			scopes[index] = server.NewId()
			keys[index] = RunOnceOwnershipKey(runOnceGenerationKey(scopes[index]))
			originals[index] = ScheduleTask(runOnceGenerationWork, &runOnceGenerationArgs{Scope: scopes[index], Cursor: 7}, owner,
				runOnceGenerationKey(scopes[index]), RunAt(server.NowUtc().Add(-time.Hour)))
		}
		evalDone := make(chan error, 1)
		go func() {
			var resultErr error
			server.HandleError(func() {
				finished, retried, posts, err := worker.EvalTasks(count)
				if err != nil || len(finished) != count || len(retried)+len(posts) != 0 {
					resultErr = taskQueueProtocolFailure("handback count=%d retry=%d/%d error=%v", len(finished), len(retried), len(posts), err)
				}
			}, func(err error) { resultErr = err })
			evalDone <- resultErr
		}()
		for range count {
			taskQueueWait(ctx, active)
		}
		producerReady, producerDone := make(chan struct{}), make(chan error, 1)
		go func() {
			var resultErr error
			server.HandleError(func() {
				server.OwnedTx(ctx, keys, func(tx server.PgTx) {
					for _, scope := range scopes {
						for _, seconds := range []int{30, 60, 20} {
							ScheduleTaskInTx(tx, runOnceGenerationWork, &runOnceGenerationArgs{Scope: scope, Cursor: 200}, owner,
								runOnceGenerationKey(scope), RunAt(baseTime.Add(time.Duration(seconds)*time.Second)), RequireQueueOwnership(tx))
						}
					}
					close(producerReady)
					taskQueueWait(ctx, producerRelease)
				}, server.TxReadCommitted, server.OptNoRetry())
			}, func(err error) { resultErr = err })
			producerDone <- resultErr
		}()
		taskQueueWait(ctx, producerReady)
		functionOnce.Do(func() { close(functionsRelease) })
		taskQueueWait(ctx, waiting)
		// OwnedTx reports waiting only after releasing every partial key and
		// its checkout, before any finishing business transaction begins.
		if len(GetFinishedTasks(ctx, originals...)) != 0 || len(GetTasks(ctx, originals...)) != count {
			t.Fatal("handback mutated custody before its producer owner committed")
		}
		producerOnce.Do(func() { close(producerRelease) })
		producerErr := taskQueueError(ctx, producerDone)
		admissionOnce.Do(func() { close(admissionRelease) })
		evalErr := taskQueueError(ctx, evalDone)
		if producerErr != nil || evalErr != nil || reruns.Load() != 0 {
			t.Fatalf("owned producer/handback failed or retried: producer=%v eval=%v reruns=%d", producerErr, evalErr, reruns.Load())
		}
		if batch && batchCommits != 1 {
			t.Fatalf("queued results bypassed the actual batch handback: commits=%d", batchCommits)
		}
		if len(GetTasks(ctx, originals...)) != 0 || len(GetFinishedTasks(ctx, originals...)) != count {
			t.Fatal("committed original owner custody is incomplete")
		}
		pending := runOnceGenerationPending(ctx, scopes)
		if len(pending) != count {
			t.Fatalf("active future requests lost successor custody: count=%d want=%d", len(pending), count)
		}
		for _, next := range pending {
			var args runOnceGenerationArgs
			server.Raise(json.Unmarshal([]byte(next.ArgsJson), &args))
			wantCursor := 7
			if !batch {
				wantCursor = 99
			}
			if !next.RunAt.Equal(expectedAt) || args.Cursor != wantCursor || !next.ClaimTime.IsZero() {
				t.Fatalf("owned handback changed MIN deadline/cursor: at=%s want=%s cursor=%d want=%d", next.RunAt, expectedAt, args.Cursor, wantCursor)
			}
		}
		future, futureGuard, err := worker.takeTasks(count)
		if futureGuard != nil {
			defer futureGuard.release()
		}
		if err != nil || len(future) != 0 {
			t.Fatalf("owned future successor became immediately eligible: count=%d error=%v", len(future), err)
		}
	})
}

func TestTaskQueueOrdinaryHandbackPreservesActiveFutureMinimum(t *testing.T) {
	runTaskQueueFutureHandback(t, false)
}

func TestTaskQueueBatchHandbackPreservesActiveFutureMinimum(t *testing.T) {
	runTaskQueueFutureHandback(t, true)
}

// A dispatcher-like post needs another deduplicated queue key. Holding that
// extra key must exclude the entire completion transaction before it locks or
// removes its own row; its publication then commits with the exact handback.
func TestTaskQueuePostDeclaresCompletePublicationSetBeforeBegin(t *testing.T) {
	runOnceGenerationEnv(t, func(t testing.TB, ctx context.Context) {
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		scope, publicationScope := server.NewId(), server.NewId()
		id := ScheduleTask(runOnceGenerationWork, &runOnceGenerationArgs{Scope: scope}, owner,
			runOnceGenerationKey(scope), RunAt(server.NowUtc().Add(-time.Hour)))
		key := RunOnceOwnershipKey(runOnceGenerationKey(publicationScope))
		entered, waiting, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
		probe := make(chan struct{})
		var waitingOnce, releaseOnce, probeOnce sync.Once
		defer releaseOnce.Do(func() { close(release) })
		defer probeOnce.Do(func() { close(probe) })
		probeResult, ownerDone := make(chan error, 1), make(chan error, 1)
		go func() {
			var ownerErr error
			server.HandleError(func() {
				server.OwnedTx(ctx, []server.PgOwnershipKey{key}, func(tx server.PgTx) {
					close(entered)
					taskQueueWait(ctx, probe)
					var observed server.Id
					err := tx.QueryRow(ctx, `SELECT task_id FROM pending_task WHERE task_id=$1 FOR UPDATE NOWAIT`, id).Scan(&observed)
					probeResult <- err
					taskQueueWait(ctx, release)
					server.Raise(err)
				}, server.TxReadCommitted, server.OptNoRetry())
			}, func(err error) { ownerErr = err })
			ownerDone <- ownerErr
		}()
		taskQueueWait(ctx, entered)
		workerCtx := server.Testing_WithPgOwnershipObservation(ctx, func(event server.PgOwnershipEvent) {
			if event.Kind == server.PgOwnershipWaiting {
				waitingOnce.Do(func() { close(waiting) })
				taskQueueWait(ctx, release)
			}
		})
		worker := NewTaskWorkerWithDefaults(workerCtx)
		defer worker.Close()
		target := &taskQueueProtocolTarget{runOnceGenerationTarget: &runOnceGenerationTarget{Target: NewTaskTarget(runOnceGenerationWork)}, extra: []server.PgOwnershipKey{key}}
		target.after = func(tx server.PgTx, _ *Task) error {
			ScheduleTaskInTx(tx, runOnceGenerationWork, &runOnceGenerationArgs{Scope: publicationScope, Cursor: 99}, owner,
				runOnceGenerationKey(publicationScope), RunAt(server.NowUtc().Add(time.Hour)), RequireQueueOwnership(tx))
			return nil
		}
		worker.AddTargets(target)
		done := make(chan error, 1)
		go func() {
			var resultErr error
			server.HandleError(func() {
				finished, retried, posts, err := worker.EvalTasks(1)
				if err != nil || len(finished) != 1 || finished[0] != id || len(retried)+len(posts) != 0 {
					resultErr = errors.New("declared publication owner did not finish exactly")
				}
			}, func(err error) { resultErr = err })
			done <- resultErr
		}()
		taskQueueWait(ctx, waiting)
		probeOnce.Do(func() { close(probe) })
		rowErr := taskQueueError(ctx, probeResult)
		releaseOnce.Do(func() { close(release) })
		ownerErr, evalErr := taskQueueError(ctx, ownerDone), taskQueueError(ctx, done)
		if rowErr != nil || ownerErr != nil || evalErr != nil {
			t.Fatalf("post key admission followed business row entry: row=%v owner=%v eval=%v", rowErr, ownerErr, evalErr)
		}
		if len(GetTasks(ctx, id)) != 0 || len(GetFinishedTasks(ctx, id)) != 1 || len(runOnceGenerationPending(ctx, []server.Id{publicationScope})) != 1 {
			t.Fatal("complete publication set did not retain atomic task custody")
		}
	})
}
