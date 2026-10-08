// Durable post retries reacquire the original complete publication set while
// their own task retains independent pending/finished custody.
package task

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

type taskQueuePostRetryKey struct{}

func taskQueuePostRetryPost(_ *runOnceGenerationArgs, _ *struct{}, owner *session.ClientSession, tx server.PgTx) error {
	return owner.Ctx.Value(taskQueuePostRetryKey{}).(func(server.PgTx) error)(tx)
}

func TestTaskQueuePostRetryOwnsOriginalPublicationAndFinishedRows(t *testing.T) {
	runOnceGenerationEnv(t, func(t testing.TB, ctx context.Context) {
		var reruns atomic.Int64
		ctx = server.Testing_WithTxRerunHook(ctx, func() { reruns.Add(1) })
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		scope, publicationScope := server.NewId(), server.NewId()
		key := RunOnceOwnershipKey(runOnceGenerationKey(publicationScope))
		var postCalls atomic.Int64
		workerCtx := context.WithValue(ctx, taskQueuePostRetryKey{}, func(tx server.PgTx) error {
			if postCalls.Add(1) == 1 {
				return errors.New("synthetic queue post retry")
			}
			ScheduleTaskInTx(tx, runOnceGenerationWork, &runOnceGenerationArgs{Scope: publicationScope, Cursor: 99}, owner,
				runOnceGenerationKey(publicationScope), RunAt(server.NowUtc().Add(time.Hour)), RequireQueueOwnership(tx))
			return nil
		})
		waiting, release := make(chan struct{}), make(chan struct{})
		var waitOnce, releaseOnce sync.Once
		defer releaseOnce.Do(func() { close(release) })
		workerCtx = server.Testing_WithPgOwnershipObservation(workerCtx, func(event server.PgOwnershipEvent) {
			if event.Kind == server.PgOwnershipWaiting {
				waitOnce.Do(func() { close(waiting) })
				taskQueueWait(ctx, release)
			}
		})
		worker := NewTaskWorkerWithDefaults(workerCtx)
		defer worker.Close()
		target := &taskQueueProtocolTarget{runOnceGenerationTarget: &runOnceGenerationTarget{
			Target: NewTaskTargetWithPost(runOnceGenerationWork, taskQueuePostRetryPost),
		}, extra: []server.PgOwnershipKey{key}}
		worker.AddTargets(target)
		id := ScheduleTask(runOnceGenerationWork, &runOnceGenerationArgs{Scope: scope}, owner,
			runOnceGenerationKey(scope), RunAt(server.NowUtc().Add(-time.Hour)))
		finished, retried, posts, err := worker.EvalTasks(1)
		if err != nil || len(finished)+len(retried) != 0 || len(posts) != 1 || posts[0] != id {
			t.Fatalf("post failure did not retain its original durable owner: error=%v", err)
		}
		var retryId server.Id
		server.Tx(ctx, func(tx server.PgTx) {
			server.Raise(tx.QueryRow(ctx, `SELECT task_id FROM pending_task WHERE function_name=$1 AND args_json::jsonb->>'task_id'=$2`,
				functionName(worker.RunPost), id.String()).Scan(&retryId))
		}, server.TxReadCommitted, server.OptNoRetry())
		server.OwnedTx(ctx, []server.PgOwnershipKey{PendingTaskOwnershipKey(retryId, nil)}, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET run_at=$2,release_time=$2 WHERE task_id=$1`, retryId, time.Time{}))
		}, server.TxReadCommitted, server.OptNoRetry())
		entered, probe := make(chan struct{}), make(chan struct{})
		var probeOnce sync.Once
		defer probeOnce.Do(func() { close(probe) })
		probeResult, ownerDone := make(chan error, 1), make(chan error, 1)
		go func() {
			var resultErr error
			server.HandleError(func() {
				server.OwnedTx(ctx, []server.PgOwnershipKey{key}, func(tx server.PgTx) {
					close(entered)
					taskQueueWait(ctx, probe)
					var observed server.Id
					err := tx.QueryRow(ctx, `SELECT task_id FROM finished_task WHERE task_id=$1 FOR UPDATE NOWAIT`, id).Scan(&observed)
					probeResult <- err
					taskQueueWait(ctx, release)
					server.Raise(err)
				}, server.TxReadCommitted, server.OptNoRetry())
			}, func(err error) { resultErr = err })
			ownerDone <- resultErr
		}()
		taskQueueWait(ctx, entered)
		evalDone := make(chan error, 1)
		go func() {
			var resultErr error
			server.HandleError(func() {
				finished, retried, posts, err := worker.EvalTasks(1)
				if err != nil || len(finished) != 1 || finished[0] != retryId || len(retried)+len(posts) != 0 {
					resultErr = errors.New("post retry did not finish its own durable identity")
				}
			}, func(err error) { resultErr = err })
			evalDone <- resultErr
		}()
		taskQueueWait(ctx, waiting)
		probeOnce.Do(func() { close(probe) })
		rowErr := taskQueueError(ctx, probeResult)
		releaseOnce.Do(func() { close(release) })
		ownerErr, evalErr := taskQueueError(ctx, ownerDone), taskQueueError(ctx, evalDone)
		if rowErr != nil || ownerErr != nil || evalErr != nil || postCalls.Load() != 2 || reruns.Load() != 0 {
			t.Fatalf("post retry crossed ownership or reran a transaction: row=%v owner=%v eval=%v calls=%d reruns=%d", rowErr, ownerErr, evalErr, postCalls.Load(), reruns.Load())
		}
		original := GetFinishedTasks(ctx, id)[id]
		if original == nil || !original.PostCompleted || len(GetTasks(ctx, id, retryId)) != 0 ||
			len(GetFinishedTasks(ctx, retryId)) != 1 || len(runOnceGenerationPending(ctx, []server.Id{publicationScope})) != 1 {
			t.Fatal("post retry lost original, retry, or publication custody")
		}
	})
}
