// A failure keeps a newer producer's minimum without reusing the consumed wake.
package task

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
)

func TestRunOnceFailedExecutionKeepsEarliestFutureWake(t *testing.T) {
	runOnceGenerationEnv(t, func(t testing.TB, ctx context.Context) {
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		now := server.NowUtc().Truncate(time.Microsecond)
		wakeAt := now.Add(5 * time.Minute)
		scope := server.NewId()
		id := ScheduleTask(runOnceGenerationWork, &runOnceGenerationArgs{Scope: scope, Cursor: 7}, owner,
			runOnceGenerationKey(scope), RunAt(now.Add(-time.Hour)))
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET reschedule_error_count=19 WHERE task_id=$1`, id))
		})
		before := GetTasks(ctx, id)[id]
		if before == nil {
			t.Fatal("fixture lost its pending owner")
		}
		calls := 0
		target := &runOnceGenerationTarget{Target: NewTaskTarget(runOnceGenerationWork)}
		target.before = func(runCtx context.Context, claimed *Task) error {
			calls++
			if calls != 1 {
				return nil
			}
			if claimed.ClaimGeneration != 1 || claimed.RunOnceGeneration != 0 || claimed.RescheduleErrorCount != 19 {
				return errors.New("fixture did not execute the original captured claim")
			}
			for _, at := range []time.Time{wakeAt.Add(time.Hour), wakeAt, wakeAt.Add(time.Minute)} {
				ScheduleTask(runOnceGenerationWork, &runOnceGenerationArgs{Scope: scope, Cursor: 999}, owner,
					runOnceGenerationKey(scope), RunAt(at))
			}
			if runCtx.Err() != nil {
				return errors.New("RunOnce producer canceled the active body")
			}
			return io.ErrUnexpectedEOF
		}
		worker := runOnceGenerationWorker(ctx, target)
		defer worker.Close()
		claimAt := now
		worker.claimNow = func() time.Time { return claimAt }
		finished, retried, posts, err := worker.EvalTasks(1)
		pending := GetTasks(ctx, id)[id]
		var wake *time.Time
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT run_once_wake_at FROM pending_task WHERE task_id=$1`, id).Scan(&wake))
		})
		if err != nil || len(finished)+len(posts) != 0 || len(retried) != 1 || retried[0] != id || pending == nil ||
			pending.RescheduleError != io.ErrUnexpectedEOF.Error() || pending.RescheduleErrorCount != 20 ||
			pending.ClaimGeneration != 1 || pending.RunOnceGeneration != 3 || pending.ArgsJson != before.ArgsJson ||
			!pending.RunAt.Equal(wakeAt) || wake == nil || !wake.Equal(wakeAt) || len(GetFinishedTasks(ctx, id)) != 0 {
			t.Fatal("failed invocation lost the future RunOnce minimum, arguments, or exact retry owner", err, pending)
		}
		claimAt = wakeAt.Add(-2 * time.Second)
		if finished, retried, posts, err := worker.EvalTasks(1); err != nil || len(finished)+len(retried)+len(posts) != 0 || calls != 1 {
			t.Fatal("future minimum became eligible before its due block", err)
		}
		claimAt = wakeAt.Add(2 * time.Second)
		finished, retried, posts, err = worker.EvalTasks(1)
		if err != nil || len(finished) != 1 || finished[0] != id || len(retried)+len(posts) != 0 || calls != 2 ||
			len(GetFinishedTasks(ctx, id)) != 1 || len(runOnceGenerationPending(ctx, []server.Id{scope})) != 0 {
			t.Fatal("ordinary future retry did not absorb the exact wake once", err)
		}
	})
}

func TestRunOnceFailedExecutionDoesNotReuseConsumedWake(t *testing.T) {
	for _, failure := range []error{io.ErrUnexpectedEOF, context.Canceled, context.DeadlineExceeded,
		errors.Join(context.Canceled, &pgconn.PgError{Code: "40001"})} {
		runOnceGenerationEnv(t, func(t testing.TB, ctx context.Context) {
			owner := session.NewLocalClientSession(ctx, "", nil)
			defer owner.Cancel()
			scope := server.NewId()
			at := server.NowUtc().Add(-time.Hour)
			id := ScheduleTask(runOnceGenerationWork, &runOnceGenerationArgs{Scope: scope}, owner, runOnceGenerationKey(scope), RunAt(at))
			ScheduleTask(runOnceGenerationWork, &runOnceGenerationArgs{Scope: scope}, owner, runOnceGenerationKey(scope), RunAt(at))
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET reschedule_error_count=19 WHERE task_id=$1`, id))
			})
			target := &runOnceGenerationTarget{Target: NewTaskTarget(runOnceGenerationWork)}
			target.before = func(context.Context, *Task) error { return failure }
			worker := runOnceGenerationWorker(ctx, target)
			defer worker.Close()
			finished, retried, posts, err := worker.EvalTasks(1)
			pending := GetTasks(ctx, id)[id]
			var wake *time.Time
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT run_once_wake_at FROM pending_task WHERE task_id=$1`, id).Scan(&wake))
			})
			if err != nil || len(finished)+len(posts) != 0 || len(retried) != 1 || retried[0] != id || pending == nil ||
				pending.RescheduleError != failure.Error() || pending.RescheduleErrorCount != 20 ||
				pending.RunOnceGeneration != 1 || pending.ClaimGeneration != 1 || wake != nil || len(GetFinishedTasks(ctx, id)) != 0 {
				t.Fatal("ordinary failed execution changed retry/error or retained a consumed wake", err)
			}
			lower := errorRescheduleDelay(RescheduleTimeout, RescheduleBackoffMaxTimeout, 19, rescheduleBackoffMaxExponent, 0)
			upper := errorRescheduleDelay(RescheduleTimeout, RescheduleBackoffMaxTimeout, 19, rescheduleBackoffMaxExponent, 1)
			if delay := pending.RunAt.Sub(pending.ReleaseTime); delay < lower-time.Microsecond || upper+time.Microsecond < delay {
				t.Fatal("ordinary failure bypassed saturated backoff without a new producer or collector authority", delay)
			}
		})
	}
}
