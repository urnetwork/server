package task

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/urnetwork/server"
)

// A deadline-class Post panic is retained as the primary failure. Its existing
// conservative connection policy quarantines, rather than reusing, this guard.
func TestTaskFinalizationBorrowedDeadlineQuarantinesWithoutReplay(t *testing.T) {
	runOnceGenerationEnv(t, func(t testing.TB, ctx context.Context) {
		worker, guard, owner, ids, _, results := runOnceGenerationClaimResults(t, ctx, false)
		defer worker.Close()
		defer guard.release()
		defer owner.Cancel()
		worker.AddTargets(&finalizationPhaseOwnedTarget{Target: NewTaskTarget(runOnceGenerationWork)})
		result := results[0]
		name := worker.metricName(result.task.FunctionName)
		post := taskFinalizationPhaseErrorsTotal.WithLabelValues(name, "transactional_post", "deadline")
		acquire := taskFinalizationPhaseErrorsTotal.WithLabelValues(name, "ownership_acquire", "deadline")
		beforePost, beforeAcquire := testutil.ToFloat64(post), testutil.ToFloat64(acquire)
		postCalls := 0
		result.runPost = func(server.PgTx) ([]server.PostFunction, error) {
			postCalls++
			panic(context.DeadlineExceeded)
		}
		failure := server.HandleError(func() { worker.finalizeTaskWithGuard(result, guard) })
		if failure != context.DeadlineExceeded || postCalls != 1 || ctx.Err() != nil ||
			guard.completionSessionError() == nil || guard.conn.Conn().IsClosed() ||
			guard.conn.Conn().PgConn().TxStatus() != 'I' ||
			testutil.ToFloat64(post) != beforePost+1 || testutil.ToFloat64(acquire) != beforeAcquire ||
			GetTasks(ctx, ids...)[ids[0]] == nil || len(GetFinishedTasks(ctx, ids...)) != 0 {
			t.Fatal("deadline Post lost its original panic, phase, rollback or required quarantine", failure, guard.completionSessionError())
		}
		checkExecution := func(wantFree bool) {
			server.Db(ctx, func(probe server.PgConn) {
				var free bool
				server.Raise(probe.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, taskAdvisoryLockKey(ids[0])).Scan(&free))
				if free {
					server.RaisePgResult(probe.Exec(ctx, `SELECT pg_advisory_unlock($1)`, taskAdvisoryLockKey(ids[0])))
				}
				if free != wantFree {
					t.Fatal("deadline scope changed execution custody before the caller's join")
				}
			}, server.OptNoRetry())
		}
		checkExecution(false)
		keys, owned, err := taskCompletionOwnershipKeys(worker.targets[result.task.FunctionName], result.task, result.resultJson, true)
		if err != nil || !owned {
			t.Fatal("deadline control lost its declared business ownership", err)
		}
		refused := server.HandleError(func() {
			guard.finalizeOwnedTx(ctx, keys, func(server.PgTx) { postCalls++ })
		})
		if refused != guard.completionSessionError() || postCalls != 1 ||
			testutil.ToFloat64(post) != beforePost+1 || testutil.ToFloat64(acquire) != beforeAcquire {
			t.Fatal("quarantined guard replayed a callback or changed the failed finalization's metrics", refused)
		}
		// The fixture's body already returned and no external Post committed.
		// Its owner can now complete the ordinary whole-guard cleanup.
		guard.release()
		checkExecution(true)
	})
}
