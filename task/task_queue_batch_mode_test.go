// A generic batch certification does not opt its owner into another target's
// backend or isolation policy. Both collectors retain that boundary when all
// mixed results are already published before their first receive.
package task

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

func taskQueueGenericBatchWork(*runOnceGenerationArgs, *session.ClientSession) (*struct{}, error) {
	return &struct{}{}, nil
}

// The trigger observes actual committed handback transactions, including the
// batch CTE; it does not add a Post to a target certified to have no Post.
func runTaskQueueMixedBatchModes(t *testing.T, run bool) {
	runOnceGenerationEnv(t, func(t testing.TB, ctx context.Context) {
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `CREATE TABLE test_task_queue_completion_mode (
                task_id uuid PRIMARY KEY, isolation text NOT NULL, transaction_id bigint NOT NULL)`))
			server.RaisePgResult(tx.Exec(ctx, `CREATE FUNCTION test_task_queue_completion_mode_record() RETURNS trigger
                LANGUAGE plpgsql AS $$ BEGIN
                INSERT INTO test_task_queue_completion_mode VALUES
                  (NEW.task_id,current_setting('transaction_isolation'),txid_current());
                RETURN NEW; END $$`))
			server.RaisePgResult(tx.Exec(ctx, `CREATE TRIGGER test_task_queue_completion_mode_trigger
                AFTER INSERT ON finished_task FOR EACH ROW EXECUTE FUNCTION test_task_queue_completion_mode_record()`))
		}, server.TxReadCommitted, server.OptNoRetry())
		var reruns atomic.Int64
		ctx = server.Testing_WithTxRerunHook(ctx, func() { reruns.Add(1) })
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		settings := DefaultTaskWorkerSettings()
		settings.BatchSize = 4
		settings.ClaimRegisteredTargetsOnly = true
		worker := NewTaskWorker(ctx, settings)
		defer worker.Close()
		worker.AddTargets(
			&taskQueueProtocolTarget{runOnceGenerationTarget: &runOnceGenerationTarget{
				Target: NewTaskTarget(runOnceGenerationWork), batch: true,
			}},
			&runOnceGenerationTarget{Target: NewTaskTarget(taskQueueGenericBatchWork), batch: true},
		)
		ordinaryFailures := taskFinalizationErrorsTotal.WithLabelValues(worker.metricName(NewTaskTarget(runOnceGenerationWork).TargetFunctionName()), "other")
		genericFailures := taskFinalizationErrorsTotal.WithLabelValues(worker.metricName(NewTaskTarget(taskQueueGenericBatchWork).TargetFunctionName()), "other")
		beforeOrdinary, beforeGeneric := testutil.ToFloat64(ordinaryFailures), testutil.ToFloat64(genericFailures)
		published := make(chan struct{}, 4)
		worker.completionResultPublished = func() { published <- struct{}{} }
		worker.taskSlotEventPublished = func() { published <- struct{}{} }
		never := make(chan time.Time)
		first := true
		drainDone := make(chan struct{})
		worker.heartbeatAfter = func(time.Duration) <-chan time.Time {
			if first {
				first = false
				for range 4 {
					taskQueueWait(ctx, published)
				}
				if run {
					// Phase one stops further admission while these four exact
					// published results still own their detached handbacks.
					go func() { defer close(drainDone); worker.Drain() }()
					taskQueueWait(ctx, worker.runCtx.Done())
				}
			}
			return never
		}
		batchCommits := 0
		worker.completionBatchCommitReturned = func() { batchCommits++ }
		ids := make([]server.Id, 4)
		for index := range ids {
			work := runOnceGenerationWork
			if index >= 2 {
				work = taskQueueGenericBatchWork
			}
			ids[index] = ScheduleTask(work, &runOnceGenerationArgs{Scope: server.NewId()}, owner,
				RunAt(server.NowUtc().Add(-time.Hour)))
		}
		if run {
			done := make(chan error, 1)
			go func() {
				var resultErr error
				server.HandleError(worker.Run, func(err error) { resultErr = err })
				done <- resultErr
			}()
			if err := taskQueueError(ctx, done); err != nil {
				t.Fatal("mixed-mode Run did not join", err)
			}
			taskQueueWait(ctx, drainDone)
			if !worker.WaitFinalHandback() || worker.InflightCount() != 0 {
				t.Fatal("mixed-mode Run left an unjoined owner")
			}
		} else {
			finished, retried, posts, err := worker.EvalTasks(4)
			if err != nil || len(finished) != 4 || len(retried)+len(posts) != 0 {
				t.Fatalf("mixed-mode finite handback failed: finished=%d error=%v", len(finished), err)
			}
		}
		if testutil.ToFloat64(ordinaryFailures) != beforeOrdinary || testutil.ToFloat64(genericFailures) != beforeGeneric {
			t.Fatal("healthy pre-transaction compatibility fallback emitted finalization failures")
		}
		if len(GetTasks(ctx, ids...)) != 0 || len(GetFinishedTasks(ctx, ids...)) != 4 {
			t.Fatal("mixed modes lost exact durable completion custody")
		}
		transactions := map[int64]bool{}
		server.Tx(ctx, func(tx server.PgTx) {
			for index, id := range ids {
				var isolation string
				var transactionId int64
				server.Raise(tx.QueryRow(ctx, `SELECT isolation,transaction_id FROM test_task_queue_completion_mode WHERE task_id=$1`, id).
					Scan(&isolation, &transactionId))
				want := "read committed"
				if index >= 2 {
					want = "repeatable read"
				}
				if isolation != want {
					t.Fatalf("mixed completion batch changed target isolation: index=%d got=%s want=%s", index, isolation, want)
				}
				transactions[transactionId] = true
			}
		}, server.TxReadCommitted, server.OptNoRetry())
		if batchCommits != 0 || len(transactions) != 4 || reruns.Load() != 0 {
			t.Fatalf("mixed completion modes did not take independent no-replay handbacks: batches=%d transactions=%d reruns=%d",
				batchCommits, len(transactions), reruns.Load())
		}
	})
}

func TestTaskQueueMixedBatchModesKeepFiniteCompletionIsolation(t *testing.T) {
	runTaskQueueMixedBatchModes(t, false)
}

func TestTaskQueueMixedBatchModesKeepRunCompletionIsolation(t *testing.T) {
	runTaskQueueMixedBatchModes(t, true)
}
