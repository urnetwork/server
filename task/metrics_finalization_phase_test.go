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
)

type finalizationPhaseOwnedTarget struct{ Target }

func (*finalizationPhaseOwnedTarget) TaskCompletionOwnershipKeys(*Task, string) ([]server.PgOwnershipKey, error) {
	return nil, nil
}

// The same typed deadline at two real boundaries must produce different phase
// observations. Neither failed attempt enters its Post or changes durable rows;
// after the exact resource is released the original result completes once.
func TestTaskFinalizationPhaseDistinguishesAcquireAndAdmission(t *testing.T) {
	for _, owned := range []bool{false, true} {
		runOnceGenerationEnv(t, func(t testing.TB, ctx context.Context) {
			pop := server.Config.PushSimpleResource(server.DefaultPgConfigResourceName, []byte("min_connections: 0\nmax_connections: 1\n"))
			server.PgReset()
			defer func() { pop(); server.PgReset() }()
			admissionObserved := false
			if owned {
				ctx = server.Testing_WithPgOwnershipObservation(ctx, func(event server.PgOwnershipEvent) {
					if event.Kind == server.PgOwnershipWaiting {
						admissionObserved = true
						// Hold this actual known-busy boundary until the shorter
						// finalizer deadline has expired; no next Acquire races it.
						select {
						case <-time.After(250 * time.Millisecond):
						case <-ctx.Done():
						}
					}
				})
			}
			worker, guard, owner, ids, _, results := runOnceGenerationClaimResults(t, ctx, false)
			defer worker.Close()
			defer guard.release()
			defer owner.Cancel()
			worker.settings.FinalizeTimeout = 150 * time.Millisecond
			phase := "acquire"
			if owned {
				worker.AddTargets(&finalizationPhaseOwnedTarget{Target: NewTaskTarget(runOnceGenerationWork)})
				phase = "admission"
			}
			name := worker.metricName(results[0].task.FunctionName)
			metric := taskFinalizationPhaseErrorsTotal.WithLabelValues(name, phase, "deadline")
			before := testutil.ToFloat64(metric)
			posts := 0
			originalPost := results[0].runPost
			results[0].runPost = func(tx server.PgTx) ([]server.PostFunction, error) {
				posts++
				return originalPost(tx)
			}
			attempt := func() {
				failure := server.HandleError(func() { worker.finalizeTask(results[0]) })
				cause, _ := failure.(error)
				if !errors.Is(cause, context.DeadlineExceeded) || ctx.Err() != nil || posts != 0 || owned && !admissionObserved || testutil.ToFloat64(metric) != before+1 {
					t.Fatal("resource refusal lost exact phase, cause or pre-Post boundary", phase, failure)
				}
			}
			if owned {
				keys, usesOwnership, err := taskCompletionOwnershipKeys(worker.targets[results[0].task.FunctionName], results[0].task, results[0].resultJson, true)
				if err != nil || !usesOwnership {
					t.Fatal("owned phase fixture missing declaration", err)
				}
				server.OwnedTx(ctx, keys, func(server.PgTx) { attempt() }, server.TxReadCommitted, server.OptNoRetry())
			} else {
				server.Db(ctx, func(server.PgConn) { attempt() }, server.OptNoRetry())
			}
			if GetTasks(ctx, ids...)[ids[0]] == nil || len(GetFinishedTasks(ctx, ids...)) != 0 {
				t.Fatal("phase observation changed a refused attempt's durable outcome")
			}
			worker.settings.FinalizeTimeout = DefaultTaskFinalizeTimeout
			worker.finalizeTask(results[0])
			if posts != 1 || GetFinishedTasks(ctx, ids...)[ids[0]] == nil || len(GetTasks(ctx, ids...)) != 0 || testutil.ToFloat64(metric) != before+1 {
				t.Fatal("released resource failed completion or successful attempt counted as failure")
			}
		})
	}
}

// A transactional Post panic is distinct from its later durable retry path.
// The original panic identity and actual rollback remain authoritative.
func TestTaskFinalizationPhaseIdentifiesTransactionalPost(t *testing.T) {
	runOnceGenerationEnv(t, func(t testing.TB, ctx context.Context) {
		worker, guard, owner, ids, _, results := runOnceGenerationClaimResults(t, ctx, false)
		defer worker.Close()
		defer guard.release()
		defer owner.Cancel()
		results[0].runPost = func(server.PgTx) ([]server.PostFunction, error) { panic(context.DeadlineExceeded) }
		metric := taskFinalizationPhaseErrorsTotal.WithLabelValues(worker.metricName(results[0].task.FunctionName), "transactional_post", "deadline")
		before := testutil.ToFloat64(metric)
		failure := server.HandleError(func() { worker.finalizeTask(results[0]) })
		if failure != context.DeadlineExceeded || testutil.ToFloat64(metric) != before+1 || GetTasks(ctx, ids...)[ids[0]] == nil || len(GetFinishedTasks(ctx, ids...)) != 0 {
			t.Fatal("Post phase changed panic identity, attempt count or transactional outcome")
		}
	})
}

// A retained direct execution guard can exhaust a one-entry maintenance pool.
// That failure is an ownership Acquire, not a refused business advisory key or
// an ordinary PgBouncer checkout. The real guard stays held until test cleanup.
func TestTaskFinalizationPhaseIdentifiesOwnershipAcquire(t *testing.T) {
	runOnceGenerationEnv(t, func(t testing.TB, ctx context.Context) {
		pop := server.Config.PushSimpleResource(server.MaintenancePgConfigResourceName, []byte("min_connections: 0\nmax_connections: 1\n"))
		server.PgReset()
		defer func() { pop(); server.PgReset() }()
		worker, guard, owner, ids, _, results := runOnceGenerationClaimResults(t, ctx, false)
		defer worker.Close()
		defer guard.release()
		defer owner.Cancel()
		worker.AddTargets(&finalizationPhaseOwnedTarget{Target: NewTaskTarget(runOnceGenerationWork)})
		worker.settings.FinalizeTimeout = 150 * time.Millisecond
		metric := taskFinalizationPhaseErrorsTotal.WithLabelValues(worker.metricName(results[0].task.FunctionName), "ownership_acquire", "deadline")
		before := testutil.ToFloat64(metric)
		failure := server.HandleError(func() { worker.finalizeTask(results[0]) })
		cause, _ := failure.(error)
		var alive bool
		server.Raise(guard.conn.QueryRow(ctx, `SELECT true`).Scan(&alive))
		if !errors.Is(cause, context.DeadlineExceeded) || !alive || testutil.ToFloat64(metric) != before+1 || GetTasks(ctx, ids...)[ids[0]] == nil || len(GetFinishedTasks(ctx, ids...)) != 0 {
			t.Fatal("direct acquisition phase changed guard custody or durable outcome", failure)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var available bool
			server.Raise(conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, taskAdvisoryLockKey(ids[0])).Scan(&available))
			if available {
				server.RaisePgResult(conn.Exec(ctx, `SELECT pg_advisory_unlock($1)`, taskAdvisoryLockKey(ids[0])))
				t.Fatal("failed finalization released the active execution guard")
			}
		}, server.OptNoRetry())
	})
}

func TestTaskFinalizationPhaseMetricLabelsStayFinite(t *testing.T) {
	registry := prometheus.NewPedanticRegistry()
	registry.MustRegister(taskFinalizationPhaseErrorsTotal, taskFinalizationFailureSeconds)
	worker := NewTaskWorkerWithDefaults(t.Context())
	defer worker.Close()
	result := &taskExecutionResult{task: &Task{TaskId: server.NewId(), FunctionName: "synthetic_private_function"}}
	observation := newTaskFinalizationObservation()
	worker.observeTaskFinalizationFailure([]*taskExecutionResult{result, result}, errors.New("synthetic_private_error"), observation)
	families, err := registry.Gather()
	if err != nil || len(families) != 2 {
		t.Fatal("phase metric descriptors invalid", err)
	}
	for _, family := range families {
		for _, metric := range family.Metric {
			expectedLabels := 3
			if metric.Histogram != nil {
				expectedLabels = 1
			}
			if len(metric.Label) != expectedLabels {
				t.Fatal("phase labels expanded")
			}
			for _, label := range metric.Label {
				if (label.GetName() != "task" && label.GetName() != "phase" && label.GetName() != "cause") || strings.Contains(label.GetValue(), "synthetic_private") {
					t.Fatal("private identity, diagnostic or argument became a phase label")
				}
			}
			if h := metric.Histogram; h != nil && (h.GetSampleCount() == 0 || h.GetSampleSum() < 0 || len(h.Bucket) != 12) {
				t.Fatal("failed invocation duration observation is not finite and bounded")
			}
		}
	}
}
