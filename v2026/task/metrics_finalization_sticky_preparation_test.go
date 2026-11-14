package task

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/urnetwork/server/v2026"
)

// Each failed invocation has one source phase. A previously quarantined guard
// fails preparation without repeating the original Post or its deadline event.
func TestTaskFinalizationStickySessionReportsPreparationWithoutAnotherAdmission(t *testing.T) {
	runOnceGenerationEnv(t, func(t testing.TB, ctx context.Context) {
		worker, guard, owner, ids, _, results := runOnceGenerationClaimResults(t, ctx, false)
		defer worker.Close()
		defer guard.release()
		defer owner.Cancel()
		worker.AddTargets(&finalizationPhaseOwnedTarget{Target: NewTaskTarget(runOnceGenerationWork)})
		result := results[0]
		name := worker.metricName(result.task.FunctionName)
		post := taskFinalizationPhaseErrorsTotal.WithLabelValues(name, "transactional_post", "deadline")
		preparation := taskFinalizationPhaseErrorsTotal.WithLabelValues(name, "preparation", "other")
		admission := taskFinalizationPhaseErrorsTotal.WithLabelValues(name, "admission", "other")
		acquire := taskFinalizationPhaseErrorsTotal.WithLabelValues(name, "ownership_acquire", "other")
		deadlineAttempts := taskFinalizationErrorsTotal.WithLabelValues(name, "deadline")
		otherAttempts := taskFinalizationErrorsTotal.WithLabelValues(name, "other")
		beforePost, beforePreparation := testutil.ToFloat64(post), testutil.ToFloat64(preparation)
		beforeAdmission, beforeAcquire := testutil.ToFloat64(admission), testutil.ToFloat64(acquire)
		beforeDeadline, beforeOther := testutil.ToFloat64(deadlineAttempts), testutil.ToFloat64(otherAttempts)
		admissionAttempts := func() float64 {
			total := float64(0)
			for _, stage := range []string{"unknown", "precheck", "probe", "cleanup", "acknowledged_busy_wait"} {
				for _, cause := range []string{"other", "deadline"} {
					total += testutil.ToFloat64(taskFinalizationAdmissionErrorsTotal.WithLabelValues(name, stage, cause))
				}
			}
			return total
		}
		beforeAdmissionAttempts := admissionAttempts()
		pending := GetTasks(ctx, ids...)[ids[0]]
		if pending == nil || pending.TaskId != ids[0] {
			t.Fatal("sticky preparation fixture lost its exact claimed task")
		}
		before := server.RaisePgResult(json.Marshal(pending))
		postCalls := 0
		result.runPost = func(server.PgTx) ([]server.PostFunction, error) {
			postCalls++
			panic(context.DeadlineExceeded)
		}
		failure := server.HandleError(func() { worker.finalizeTaskWithGuard(result, guard) })
		uncertain := guard.completionSessionError()
		if failure != context.DeadlineExceeded || uncertain == nil || taskExecutionErrorCause(uncertain) != "other" ||
			postCalls != 1 || ctx.Err() != nil || guard.conn.Conn().IsClosed() || guard.conn.Conn().PgConn().TxStatus() != 'I' ||
			testutil.ToFloat64(post) != beforePost+1 || testutil.ToFloat64(preparation) != beforePreparation ||
			testutil.ToFloat64(deadlineAttempts) != beforeDeadline+1 || testutil.ToFloat64(otherAttempts) != beforeOther {
			t.Fatal("initial Post refusal lost its exact deadline event, source classifier, rollback or quarantine", failure, uncertain)
		}
		checkExecution := func(wantFree bool) {
			server.Db(ctx, func(probe server.PgConn) {
				var free bool
				server.Raise(probe.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, taskAdvisoryLockKey(ids[0])).Scan(&free))
				if free {
					server.RaisePgResult(probe.Exec(ctx, `SELECT pg_advisory_unlock($1)`, taskAdvisoryLockKey(ids[0])))
				}
				if free != wantFree {
					t.Fatal("sticky preparation changed execution custody before the caller's join")
				}
			}, server.OptNoRetry())
		}
		checkExecution(false)
		refused := server.HandleError(func() { worker.finalizeTaskWithGuard(result, guard) })
		after := server.RaisePgResult(json.Marshal(GetTasks(ctx, ids...)[ids[0]]))
		if refused != uncertain || guard.completionSessionError() != uncertain || postCalls != 1 ||
			testutil.ToFloat64(preparation) != beforePreparation+1 || testutil.ToFloat64(otherAttempts) != beforeOther+1 ||
			testutil.ToFloat64(post) != beforePost+1 || testutil.ToFloat64(deadlineAttempts) != beforeDeadline+1 ||
			testutil.ToFloat64(admission) != beforeAdmission || testutil.ToFloat64(acquire) != beforeAcquire ||
			!bytes.Equal(before, after) || len(GetFinishedTasks(ctx, ids...)) != 0 || admissionAttempts() != beforeAdmissionAttempts {
			t.Fatal("later full finalizer lost its separate preparation event or repeated durable work", refused)
		}
		checkExecution(false)
		// The body has returned and neither finalizer committed; ordinary caller
		// cleanup now owns the whole execution session and its retained locks.
		guard.release()
		checkExecution(true)
	})
}
