// Real queue writers and completion owners must publish once at commit.
package task

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// Tests run sequentially and compare process counters around exact owned work.
type taskLifecycleTestCounts struct{ submitted, finished, balked uint64 }

// Capture only acknowledged events; uncertain replies cannot become completions.
func taskLifecycleCounts() taskLifecycleTestCounts {
	return taskLifecycleTestCounts{submitted: taskSubmittedCounter.ConfirmedCount(),
		finished: taskFinishedCounter.ConfirmedCount(), balked: taskBalkedCounter.ConfirmedCount()}
}

// Require the exact delta, including zero before commit and after rollback.
func requireTaskLifecycleDelta(t testing.TB, before taskLifecycleTestCounts, submitted, finished, balked uint64) {
	t.Helper()
	after := taskLifecycleCounts()
	if after.submitted-before.submitted != submitted || after.finished-before.finished != finished || after.balked-before.balked != balked {
		t.Fatalf("task lifecycle delta=%+v -> %+v, want %d/%d/%d", before, after, submitted, finished, balked)
	}
}

// Direct and IfAbsent conflicts are committed events; a retry discards its
// first attempt instead of counting a submission that never became durable.
func TestTaskLifecycleSubmissionCommitRollbackRetry(t *testing.T) {
	runOnceGenerationEnv(t, func(t testing.TB, ctx context.Context) {
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		first, second := server.NewId(), server.NewId()
		before := taskLifecycleCounts()
		server.Tx(ctx, func(tx server.PgTx) {
			ScheduleTaskInTx(tx, runOnceGenerationWork, &runOnceGenerationArgs{}, owner)
			for range 2 {
				ScheduleTaskInTx(tx, runOnceGenerationWork, &runOnceGenerationArgs{Scope: first}, owner, runOnceGenerationKey(first))
			}
			for _, want := range []bool{true, false} {
				got, _ := ScheduleTaskInTxIfAbsent(tx, runOnceGenerationWork, &runOnceGenerationArgs{Scope: second}, owner, runOnceGenerationKey(second))
				if got != want {
					t.Fatal("IfAbsent changed insertion semantics")
				}
			}
			requireTaskLifecycleDelta(t, before, 0, 0, 0)
		})
		requireTaskLifecycleDelta(t, before, 3, 0, 2)
		before = taskLifecycleCounts()
		failure := errors.New("synthetic task submission rollback")
		var caught error
		server.HandleError(func() {
			server.Tx(ctx, func(tx server.PgTx) {
				ScheduleTaskInTx(tx, runOnceGenerationWork, &runOnceGenerationArgs{}, owner)
				ScheduleTaskInTx(tx, runOnceGenerationWork, &runOnceGenerationArgs{Scope: first}, owner, runOnceGenerationKey(first))
				panic(failure)
			}, server.OptNoRetry())
		}, func(err error) { caught = err })
		if !errors.Is(caught, failure) {
			t.Fatal("rollback lost its original failure", caught)
		}
		requireTaskLifecycleDelta(t, before, 0, 0, 0)
		attempts := 0
		retryScope := server.NewId()
		server.Tx(ctx, func(tx server.PgTx) {
			attempts++
			ScheduleTaskInTx(tx, runOnceGenerationWork, &runOnceGenerationArgs{Scope: retryScope}, owner, runOnceGenerationKey(retryScope))
			requireTaskLifecycleDelta(t, before, 0, 0, 0)
			if attempts == 1 {
				server.Raise(&pgconn.PgError{Code: "40001", Message: "synthetic retry after task insert"})
			}
		})
		if attempts != 2 || len(runOnceGenerationPending(ctx, []server.Id{retryScope})) != 1 {
			t.Fatal("retry did not retain one durable owner", attempts)
		}
		requireTaskLifecycleDelta(t, before, 1, 0, 0)
	})
}

// All three batch APIs use the supplied outer owner; a later immutable conflict
// rolls back even events already observed by an earlier batch callback.
func TestTaskLifecycleBatchSubmissionAndRollback(t *testing.T) {
	runOnceGenerationEnv(t, func(t testing.TB, ctx context.Context) {
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		ids := []server.Id{server.NewId(), server.NewId(), server.NewId(), server.NewId()}
		before := taskLifecycleCounts()
		server.Tx(ctx, func(tx server.PgTx) {
			server.BatchInTx(ctx, tx, func(batch server.PgBatch) {
				for range 2 {
					QueueTaskInBatch(tx, batch, runOnceGenerationWork, &runOnceGenerationArgs{Scope: ids[0]}, owner, runOnceGenerationKey(ids[0]))
				}
				QueueRequiredTaskInBatch(tx, batch, runOnceGenerationWork, &runOnceGenerationArgs{Scope: ids[1]}, owner, runOnceGenerationKey(ids[1]))
				QueueRequiredTasksInBatch(tx, batch, runOnceGenerationWork, []RequiredTaskBatchItem[*runOnceGenerationArgs]{
					{Args: &runOnceGenerationArgs{Scope: ids[2]}, RunOnce: runOnceGenerationKey(ids[2])},
					{Args: &runOnceGenerationArgs{Scope: ids[3]}, RunOnce: runOnceGenerationKey(ids[3])},
				}, owner)
			})
			requireTaskLifecycleDelta(t, before, 0, 0, 0)
		})
		requireTaskLifecycleDelta(t, before, 4, 0, 1)
		before = taskLifecycleCounts()
		var caught error
		newScope := server.NewId()
		server.HandleError(func() {
			server.Tx(ctx, func(tx server.PgTx) {
				server.BatchInTx(ctx, tx, func(batch server.PgBatch) {
					QueueTaskInBatch(tx, batch, runOnceGenerationWork, &runOnceGenerationArgs{Scope: newScope}, owner, runOnceGenerationKey(newScope))
					QueueRequiredTaskInBatch(tx, batch, runOnceGenerationWork, &runOnceGenerationArgs{Scope: ids[1]}, owner, runOnceGenerationKey(ids[1]))
				})
			}, server.OptNoRetry())
		}, func(err error) { caught = err })
		if caught == nil || len(runOnceGenerationPending(ctx, []server.Id{newScope})) != 0 {
			t.Fatal("required conflict did not roll back its batch", caught)
		}
		requireTaskLifecycleDelta(t, before, 0, 0, 0)
	})
}

// Claim snapshots make active conflicts and their later durable successors
// distinct. Finishing the same old owner again cannot publish another event.
func taskLifecycleCompletionWake(t *testing.T, batch bool) {
	t.Helper()
	runOnceGenerationEnv(t, func(t testing.TB, ctx context.Context) {
		worker, guard, owner, ids, scopes, results := runOnceGenerationClaimResults(t, ctx, batch)
		defer worker.Close()
		defer guard.release()
		defer owner.Cancel()
		before := taskLifecycleCounts()
		for _, scope := range scopes {
			ScheduleTask(runOnceGenerationWork, &runOnceGenerationArgs{Scope: scope}, owner,
				runOnceGenerationKey(scope), RunAt(server.NowUtc().Add(time.Hour)))
		}
		count := uint64(len(ids))
		requireTaskLifecycleDelta(t, before, 0, 0, count)
		if batch {
			if retry, err := worker.finalizeTaskBatch(results); retry || err != nil {
				t.Fatal("completion failed", retry, err)
			}
		} else {
			worker.finalizeTask(results[0])
		}
		requireTaskLifecycleDelta(t, before, count, count, count)
		if len(GetFinishedTasks(ctx, ids...)) != len(ids) || len(runOnceGenerationPending(ctx, scopes)) != len(ids) {
			t.Fatal("counts do not match finished and successor rows")
		}
		var replayErr error
		server.HandleError(func() {
			if batch {
				_, replayErr = worker.finalizeTaskBatch(results)
			} else {
				worker.finalizeTask(results[0])
			}
		}, func(err error) { replayErr = err })
		if replayErr == nil {
			t.Fatal("old owner replay was accepted")
		}
		requireTaskLifecycleDelta(t, before, count, count, count)
	})
}

// Exercise the ordinary committed move and wake.
func TestTaskLifecycleOrdinaryCompletionAndActiveWake(t *testing.T) {
	taskLifecycleCompletionWake(t, false)
}

// Exercise the shared committed move and each member's wake.
func TestTaskLifecycleBatchCompletionAndActiveWake(t *testing.T) {
	taskLifecycleCompletionWake(t, true)
}

// All three counters exist at zero and expose no variable labels or buckets.
func TestTaskLifecycleMetricsAreSimpleCounters(t *testing.T) {
	registry := prometheus.NewPedanticRegistry()
	registry.MustRegister(taskSubmittedMetric, taskFinishedMetric, taskBalkedMetric)
	families, err := registry.Gather()
	if err != nil || len(families) != 3 {
		t.Fatal("task lifecycle counters missing", err)
	}
	for _, family := range families {
		if family.GetType().String() != "COUNTER" || len(family.Metric) != 1 || len(family.Metric[0].Label) != 0 {
			t.Fatal("task lifecycle metric expanded its type or labels")
		}
	}
}
