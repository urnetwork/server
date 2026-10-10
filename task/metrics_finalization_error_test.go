// Handback failures remain separate from execution and committed queue events.
package task

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

type finalizationMetricDeclarationTarget struct {
	Target
	failure any
}

func (self *finalizationMetricDeclarationTarget) AlternateFunctionNames() []string {
	return []string{"synthetic.finalization.alias"}
}

func (self *finalizationMetricDeclarationTarget) TaskCompletionOwnershipKeys(*Task, string) ([]server.PgOwnershipKey, error) {
	panic(self.failure)
}

// The actual single finalizer observes its original panic before any database
// work, preserves it, and resolves aliases through registration only.
func TestTaskFinalizationMetricPreservesTypedPanicAndFiniteLabels(t *testing.T) {
	target := &finalizationMetricDeclarationTarget{Target: NewTaskTarget(runOnceGenerationWork)}
	worker := NewTaskWorkerWithDefaults(t.Context())
	defer worker.Close()
	worker.AddTargets(target)
	queued := &Task{TaskId: server.NewId(), FunctionName: "synthetic.finalization.alias"}
	result := &taskExecutionResult{task: queued}
	for _, test := range []struct {
		failure any
		cause   string
	}{
		{context.DeadlineExceeded, "deadline"},
		{errors.Join(server.DbContextDoneError, context.Canceled), "canceled"},
		{executionMetricCustomMatcher{}, "other"},
		{"synthetic_private_panic", "non_error_panic"},
	} {
		target.failure = test.failure
		metric := taskFinalizationErrorsTotal.WithLabelValues(worker.metricName(queued.FunctionName), test.cause)
		before := testutil.ToFloat64(metric)
		phaseMetric := taskFinalizationPhaseErrorsTotal.WithLabelValues(worker.metricName(queued.FunctionName), "preparation", test.cause)
		beforePhase := testutil.ToFloat64(phaseMetric)
		var caught any
		func() {
			defer func() { caught = recover() }()
			worker.finalizeTask(result)
		}()
		if caught != test.failure || testutil.ToFloat64(metric) != before+1 || testutil.ToFloat64(phaseMetric) != beforePhase+1 {
			t.Fatal("single finalization changed its panic or failure count", test.cause)
		}
	}
	unregistered := &taskExecutionResult{task: &Task{TaskId: server.NewId(), FunctionName: "synthetic_private_function"}}
	metric := taskFinalizationErrorsTotal.WithLabelValues("unregistered", "deadline")
	before := testutil.ToFloat64(metric)
	worker.observeTaskFinalizationFailure([]*taskExecutionResult{nil, {}, unregistered, unregistered}, context.DeadlineExceeded)
	worker.observeTaskFinalizationFailure([]*taskExecutionResult{unregistered}, nil)
	if testutil.ToFloat64(metric) != before+1 {
		t.Fatal("invalid or duplicate members changed a single failed attempt count")
	}
	registry := prometheus.NewPedanticRegistry()
	registry.MustRegister(taskFinalizationErrorsTotal)
	families, err := registry.Gather()
	if err != nil || len(families) != 1 || families[0].GetType().String() != "COUNTER" {
		t.Fatal("finalization metric descriptor invalid", err)
	}
	for _, metric := range families[0].Metric {
		if len(metric.Label) != 2 {
			t.Fatal("finalization labels expanded")
		}
		for _, label := range metric.Label {
			if (label.GetName() != "task" && label.GetName() != "cause") || strings.Contains(label.GetValue(), "synthetic_private") {
				t.Fatal("private identity or diagnostic became a metric label")
			}
		}
	}
}

// A failed finish is visible even though its function succeeded. A recovered
// transaction retry publishes no terminal failure, and the financial marker
// still follows its original commit/rollback result.
func TestTaskFinalizationMetricEvalTasksRollbackAndRecoveredRetry(t *testing.T) {
	for _, rollback := range []bool{true, false} {
		runOnceGenerationEnv(t, func(t testing.TB, ctx context.Context) {
			transient := 1
			if rollback {
				transient = 0
			}
			createCommitPostFault(ctx, transient, rollback)
			owner := session.NewLocalClientSession(ctx, "", nil)
			defer owner.Cancel()
			probe := &commitPostProbe{ctx: ctx}
			worker := newCommitPostWorker(ctx, probe.post)
			defer worker.Close()
			name := worker.metricName(NewTaskTarget(commitPostWork).TargetFunctionName())
			failure := taskFinalizationErrorsTotal.WithLabelValues(name, "postgres_other")
			retryFailure := taskFinalizationErrorsTotal.WithLabelValues(name, "postgres_serialization")
			executionFailure := taskExecutionErrorsTotal.WithLabelValues(name, "system", "postgres_other")
			before, beforeRetry, beforeExecution := testutil.ToFloat64(failure), testutil.ToFloat64(retryFailure), testutil.ToFloat64(executionFailure)
			phaseMetric := taskFinalizationPhaseErrorsTotal.WithLabelValues(name, "commit", "postgres_other")
			beforePhase := testutil.ToFloat64(phaseMetric)
			id := scheduleCommitPostWork(owner)
			var caught error
			server.HandleError(func() { _, _, _, caught = worker.EvalTasks(1) }, func(err error) { caught = err })
			expected := before
			if rollback {
				expected++
			}
			if testutil.ToFloat64(failure) != expected || testutil.ToFloat64(retryFailure) != beforeRetry || testutil.ToFloat64(executionFailure) != beforeExecution {
				t.Fatal("function success, internal retry, or collector recovery distorted failure count")
			}
			if rollback {
				if testutil.ToFloat64(phaseMetric) != beforePhase+1 {
					t.Fatal("deferred commit refusal lost its source phase")
				}
				if caught == nil || GetTasks(ctx, id)[id] == nil || GetFinishedTasks(ctx, id)[id] != nil || probe.workCount.Load() != 0 {
					t.Fatal("failed finalization changed durable rollback or post custody")
				}
			} else if caught != nil || GetFinishedTasks(ctx, id)[id] == nil || probe.workCount.Load() != 1 || !probe.workCommitted.Load() {
				t.Fatal("metric changed the recovered transaction retry or committed post")
			}
		})
	}
}

// A real failed shared body counts each member once. Successful individual
// fallback is another invocation with no failure, not a second observation of
// the first failure or an invented financial rollback.
func TestTaskFinalizationMetricBatchRollbackThenSingles(t *testing.T) {
	runOnceGenerationEnv(t, func(t testing.TB, ctx context.Context) {
		worker, guard, owner, ids, _, results := runOnceGenerationClaimResults(t, ctx, true)
		defer worker.Close()
		defer guard.release()
		defer owner.Cancel()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `CREATE SEQUENCE synthetic_metric_finish_attempt;
                CREATE FUNCTION synthetic_metric_finish_fault() RETURNS trigger LANGUAGE plpgsql AS $$
                BEGIN IF nextval('synthetic_metric_finish_attempt')=1 THEN
                    RAISE EXCEPTION 'synthetic handback body refusal' USING ERRCODE='55P03';
                END IF; RETURN NEW; END $$;
                CREATE TRIGGER synthetic_metric_finish_fault BEFORE INSERT ON finished_task
                FOR EACH ROW EXECUTE FUNCTION synthetic_metric_finish_fault()`))
		})
		metric := taskFinalizationErrorsTotal.WithLabelValues(worker.metricName(results[0].task.FunctionName), "postgres_lock")
		before := testutil.ToFloat64(metric)
		phaseMetric := taskFinalizationPhaseErrorsTotal.WithLabelValues(worker.metricName(results[0].task.FunctionName), "queue_update", "postgres_lock")
		beforePhase := testutil.ToFloat64(phaseMetric)
		lifecycle := taskLifecycleCounts()
		retry, err := worker.finalizeTaskBatch(results)
		if !retry || err == nil || testutil.ToFloat64(metric) != before+float64(len(results)) || testutil.ToFloat64(phaseMetric) != beforePhase+float64(len(results)) || len(GetFinishedTasks(ctx, ids...)) != 0 {
			t.Fatal("failed shared body lost exact-member attempt observations or safe fallback")
		}
		requireTaskLifecycleDelta(t, lifecycle, 0, 0, 0)
		for _, result := range results {
			worker.finalizeTask(result)
		}
		if testutil.ToFloat64(metric) != before+float64(len(results)) || len(GetFinishedTasks(ctx, ids...)) != len(results) {
			t.Fatal("successful fallback recounted failure or lost completion")
		}
		requireTaskLifecycleDelta(t, lifecycle, 0, uint64(len(results)), 0)
	})
}

// Both cohort owners and all-success delegation to the batch owner count each
// result once. Lost replies remain observable failures even when rows committed;
// they must not be relabeled as rollbacks or rerun by metric instrumentation.
func TestTaskFinalizationMetricCohortDelegationAndUnknownReply(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		for _, lostReply := range []bool{false, true} {
			runOnceGenerationEnv(t, func(t testing.TB, ctx context.Context) {
				owner := session.NewLocalClientSession(ctx, "", nil)
				defer owner.Cancel()
				ids := taskRunCohortTestSchedule(owner, server.NewId(), 4)
				worker := NewTaskWorkerWithDefaults(ctx)
				defer worker.Close()
				target := &taskRunCohortTestTarget{Target: NewTaskTarget(taskClaimGroupTestCall)}
				worker.AddTargets(target)
				claimed, guard, _, err := worker.takeTasksWithGuard(ctx, 1, nil, taskClaimOptions{runCohorts: true})
				if guard != nil {
					defer guard.release()
				}
				if err != nil || guard == nil || len(claimed) != len(ids) {
					t.Fatal("cohort fixture failed", err)
				}
				slot := taskRunSlots(claimed)[0]
				if mixed {
					target.before = func(_ context.Context, queued *Task) error {
						if queued.TaskId == slot.tasks[0].TaskId {
							return context.Canceled
						}
						return nil
					}
				}
				results := worker.executeTaskRunCohort(ctx, slot, worker.prepareTaskBatchTargets(claimed), taskExecutionAdmissions{})
				cause := "other"
				commits := 0
				if lostReply {
					cause = "deadline"
					worker.completionBatchCommitReturned = func() { commits++; panic(context.DeadlineExceeded) }
				} else {
					results[len(results)-1].task.ClaimGeneration++
				}
				metric := taskFinalizationErrorsTotal.WithLabelValues(worker.metricName(results[0].task.FunctionName), cause)
				before := testutil.ToFloat64(metric)
				phase := "queue_update"
				if lostReply {
					phase = "acknowledged"
				}
				phaseMetric := taskFinalizationPhaseErrorsTotal.WithLabelValues(worker.metricName(results[0].task.FunctionName), phase, cause)
				beforePhase := testutil.ToFloat64(phaseMetric)
				lifecycle := taskLifecycleCounts()
				err = worker.finalizeTaskRunCohort(results)
				if err == nil || testutil.ToFloat64(metric) != before+float64(len(results)) || testutil.ToFloat64(phaseMetric) != beforePhase+float64(len(results)) || len(guard.taskIds) != len(ids) {
					t.Fatal("cohort delegation double-counted, concealed failure, or changed owner custody")
				}
				finished := 0
				if lostReply {
					finished = len(ids)
					if mixed {
						finished--
					}
				}
				if len(GetFinishedTasks(ctx, ids...)) != finished || len(GetTasks(ctx, ids...)) != len(ids)-finished {
					t.Fatal("failure observation changed the actual transaction outcome")
				}
				requireTaskLifecycleDelta(t, lifecycle, 0, uint64(finished), 0)
				if (lostReply && (commits != 1 || !errors.Is(err, context.DeadlineExceeded))) || (!lostReply && commits != 0) {
					t.Fatal("uncertain handback was replayed or its original cause changed")
				}
			})
		}
	}
}

// Function retry and durable Post retry are acknowledged handbacks. Their
// original errors belong to their own boundary, not a finalization failure.
func TestTaskFinalizationMetricAcknowledgedRetriesAreNotFailures(t *testing.T) {
	for _, functionError := range []bool{true, false} {
		runOnceGenerationEnv(t, func(t testing.TB, ctx context.Context) {
			owner := session.NewLocalClientSession(ctx, "", nil)
			defer owner.Cancel()
			work := func(struct{}, *session.ClientSession) (struct{}, error) {
				if functionError {
					return struct{}{}, errors.New("synthetic function failure")
				}
				return struct{}{}, nil
			}
			post := func(struct{}, struct{}, *session.ClientSession, server.PgTx) error {
				return errors.New("synthetic durable post retry")
			}
			settings := DefaultTaskWorkerSettings()
			settings.ClaimRegisteredTargetsOnly = true
			worker := NewTaskWorker(ctx, settings)
			defer worker.Close()
			target := NewTaskTargetWithPost(work, post)
			worker.AddTargets(target)
			metric := taskFinalizationErrorsTotal.WithLabelValues(worker.metricName(target.TargetFunctionName()), "other")
			before := testutil.ToFloat64(metric)
			id := ScheduleTask(work, struct{}{}, owner, RunAt(server.NowUtc().Add(-time.Hour)))
			finished, retried, posts, err := worker.EvalTasks(1)
			if err != nil || len(finished) != 0 || testutil.ToFloat64(metric) != before {
				t.Fatal("acknowledged retry was counted as a failed handback", err)
			}
			if functionError {
				if len(retried) != 1 || len(posts) != 0 || GetTasks(ctx, id)[id] == nil {
					t.Fatal("ordinary retry custody changed")
				}
			} else if len(retried) != 0 || len(posts) != 1 || GetFinishedTasks(ctx, id)[id] == nil {
				t.Fatal("durable post retry custody changed")
			}
		})
	}
}
