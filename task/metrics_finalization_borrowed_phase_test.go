package task

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/urnetwork/server"
)

// A donated guard still probes real business keys, but never reports another
// pool acquisition. Its same observation must reach the actual Post and COMMIT.
func TestTaskFinalizationBorrowedPhaseAdmissionPostAndRecovery(t *testing.T) {
	runOnceGenerationEnv(t, func(t testing.TB, ctx context.Context) {
		waiting := false
		observed := server.Testing_WithPgOwnershipObservation(ctx, func(event server.PgOwnershipEvent) {
			if event.Kind == server.PgOwnershipWaiting {
				waiting = true
				select {
				case <-time.After(250 * time.Millisecond):
				case <-ctx.Done():
				}
			}
		})
		worker, guard, owner, ids, _, results := runOnceGenerationClaimResults(t, observed, false)
		defer worker.Close()
		defer guard.release()
		defer owner.Cancel()
		worker.AddTargets(&finalizationPhaseOwnedTarget{Target: NewTaskTarget(runOnceGenerationWork)})
		worker.settings.FinalizeTimeout = 150 * time.Millisecond
		result := results[0]
		keys, owned, err := taskCompletionOwnershipKeys(worker.targets[result.task.FunctionName], result.task, result.resultJson, true)
		if err != nil || !owned {
			t.Fatal("borrowed phase fixture has no business owner", err)
		}
		name := worker.metricName(result.task.FunctionName)
		admission := taskFinalizationPhaseErrorsTotal.WithLabelValues(name, "admission", "deadline")
		acquire := taskFinalizationPhaseErrorsTotal.WithLabelValues(name, "ownership_acquire", "deadline")
		postFailure := errors.New("synthetic non-connection transactional Post refusal")
		post := taskFinalizationPhaseErrorsTotal.WithLabelValues(name, "transactional_post", "other")
		beforeAdmission, beforeAcquire, beforePost := testutil.ToFloat64(admission), testutil.ToFloat64(acquire), testutil.ToFloat64(post)
		originalPost := result.runPost
		postCalls := 0
		result.runPost = func(tx server.PgTx) ([]server.PostFunction, error) {
			postCalls++
			return originalPost(tx)
		}
		server.OwnedTx(ctx, keys, func(server.PgTx) {
			failure := server.HandleError(func() { worker.finalizeTaskWithGuard(result, guard) })
			cause, _ := failure.(error)
			if !errors.Is(cause, context.DeadlineExceeded) || !waiting || postCalls != 0 ||
				testutil.ToFloat64(admission) != beforeAdmission+1 || testutil.ToFloat64(acquire) != beforeAcquire {
				t.Fatal("borrowed refusal lost admission phase or invented a pool acquisition", failure)
			}
		}, server.TxReadCommitted, server.OptNoRetry())
		if guard.completionSessionError() != nil || GetTasks(ctx, ids...)[ids[0]] == nil || len(GetFinishedTasks(ctx, ids...)) != 0 {
			t.Fatal("known busy observation changed custody or durable state")
		}
		worker.settings.FinalizeTimeout = DefaultTaskFinalizeTimeout
		// Healthy same-session recovery requires a non-connection failure.
		// The separate deadline control requires quarantine instead.
		result.runPost = func(server.PgTx) ([]server.PostFunction, error) { panic(postFailure) }
		failure := server.HandleError(func() { worker.finalizeTaskWithGuard(result, guard) })
		if failure != postFailure || guard.completionSessionError() != nil ||
			testutil.ToFloat64(post) != beforePost+1 || testutil.ToFloat64(acquire) != beforeAcquire ||
			GetTasks(ctx, ids...)[ids[0]] == nil || len(GetFinishedTasks(ctx, ids...)) != 0 {
			t.Fatal("donated Post failure lost original panic, phase or rollback", failure)
		}
		result.runPost = func(tx server.PgTx) ([]server.PostFunction, error) { postCalls++; return originalPost(tx) }
		worker.finalizeTaskWithGuard(result, guard)
		if postCalls != 1 || guard.completionSessionError() != nil || GetFinishedTasks(ctx, ids...)[ids[0]] == nil ||
			len(GetTasks(ctx, ids...)) != 0 || testutil.ToFloat64(admission) != beforeAdmission+1 ||
			testutil.ToFloat64(post) != beforePost+1 || testutil.ToFloat64(acquire) != beforeAcquire {
			t.Fatal("borrowed phase composition changed successful completion or failed-attempt counts")
		}
	})
}
