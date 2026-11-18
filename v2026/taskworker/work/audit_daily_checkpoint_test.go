package work

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
	"github.com/urnetwork/server/v2026/task"
)

type auditDailyCheckpointEval struct {
	finished, retried, postRetried []server.Id
	err                            error
	panicValue                     any
}

func startAuditDailyCheckpointEval(worker *task.TaskWorker) <-chan auditDailyCheckpointEval {
	done := make(chan auditDailyCheckpointEval, 1)
	go func() {
		result := auditDailyCheckpointEval{}
		defer func() {
			result.panicValue = recover()
			done <- result
		}()
		result.finished, result.retried, result.postRetried, result.err = worker.EvalTasks(1)
	}()
	return done
}

// Hold the second day's existing audit row. The first day is a real committed
// model rollup, and the ordinary task evaluator must durably hand off its next
// day before touching that held row. This makes the former repeated-prefix
// mechanism fail at an actual database boundary, without timing a large scan.
func TestTransferAuditRollupDailyCheckpointSurvivesInterruptedNextDay(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
		defer cancel()
		today := server.NowUtc().UTC().Truncate(24 * time.Hour)
		firstDay := today.Add(-3 * 24 * time.Hour)
		secondDay := firstDay.Add(24 * time.Hour)
		thirdDay := secondDay.Add(24 * time.Hour)
		if model.RollupTransferAuditEvents(ctx, secondDay, thirdDay) != 1 {
			t.Fatal("could not seed the held second-day replacement")
		}
		rowID := func(day time.Time) server.Id {
			var id server.Id
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT event_id FROM audit_contract_event
					WHERE event_details=$1 AND event_time >= $2 AND event_time < $3`,
					model.AuditEventDetailsTransferRollup, day, day.Add(24*time.Hour)).Scan(&id))
			})
			return id
		}
		secondBefore := rowID(secondDay)
		holder, err := server.AcquireMaintenanceDbConn(ctx)
		server.Raise(err)
		defer holder.Release()
		hold, err := holder.Begin(ctx)
		server.Raise(err)
		defer func() {
			cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer stop()
			_ = hold.Rollback(cleanup)
		}()
		var heldID server.Id
		server.Raise(hold.QueryRow(ctx, `SELECT event_id FROM audit_contract_event WHERE event_id=$1 FOR UPDATE`, secondBefore).Scan(&heldID))
		if heldID != secondBefore {
			t.Fatal("second-day barrier lost its exact audit row")
		}
		holderPID := int32(hold.Conn().PgConn().PID())
		isBlocked := func() bool {
			var blocked bool
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT EXISTS (
					SELECT 1 FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)))`, holderPID).Scan(&blocked))
			})
			return blocked
		}
		target := task.NewTaskTargetWithPost(RollupTransferAuditEvents, RollupTransferAuditEventsPost)
		newWorker := func(workerCtx context.Context) *task.TaskWorker {
			settings := task.DefaultTaskWorkerSettings()
			settings.ClaimRegisteredTargetsOnly = true
			worker := task.NewTaskWorker(workerCtx, settings)
			worker.AddTargets(target)
			return worker
		}
		pending := func() (*task.Task, *RollupTransferAuditEventsArgs) {
			var found *task.Task
			for _, queued := range task.GetTasks(ctx, task.ListPendingTasks(ctx)...) {
				if queued.FunctionName == target.TargetFunctionName() {
					if found != nil {
						t.Fatal("daily continuation duplicated its RunOnce queue owner")
					}
					found = queued
				}
			}
			if found == nil {
				t.Fatal("daily continuation lost its durable queue owner")
			}
			var args RollupTransferAuditEventsArgs
			server.Raise(json.Unmarshal([]byte(found.ArgsJson), &args))
			return found, &args
		}
		waitForEligibility := func() {
			queued, _ := pending()
			var availableBlock int64
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT available_block FROM pending_task WHERE task_id=$1`, queued.TaskId).Scan(&availableBlock))
			})
			// Finite EvalTasks does not wait for the generated queue boundary,
			// which rounds beyond run_at. Observe that boundary without changing
			// the real Post cursor, retry arguments, lease, or scheduled time.
			delay := time.Until(time.Unix(availableBlock*task.BlockSizeSeconds, 0))
			if delay <= 0 {
				return
			}
			timer := time.NewTimer(delay)
			defer timer.Stop()
			select {
			case <-timer.C:
			case <-ctx.Done():
				t.Fatal("queued daily continuation never became eligible")
			}
		}
		owner := session.NewLocalClientSession(ctx, "192.0.2.1:0", nil)
		defer owner.Cancel()
		server.Tx(ctx, func(tx server.PgTx) {
			scheduleRollupTransferAuditEvents(owner, tx, &RollupTransferAuditEventsArgs{MinTime: &firstDay}, server.NowUtc().Add(-time.Minute))
		})
		firstCtx, cancelFirst := context.WithCancel(ctx)
		defer cancelFirst()
		firstWorker := newWorker(firstCtx)
		defer firstWorker.Close()
		firstDone := startAuditDailyCheckpointEval(firstWorker)
		var first auditDailyCheckpointEval
		for {
			select {
			case first = <-firstDone:
				goto firstFinished
			case <-ctx.Done():
				t.Fatal("first daily checkpoint did not terminate")
			case <-time.After(10 * time.Millisecond):
				if isBlocked() {
					// RED source enters day two before publishing day one's cursor.
					_ = rowID(firstDay)
					cancelFirst()
					select {
					case <-firstDone:
					case <-ctx.Done():
						t.Fatal("blocked first invocation did not unwind")
					}
					t.Fatal("completed first day entered held second day before durable checkpoint")
				}
			}
		}
	firstFinished:
		if first.panicValue != nil || first.err != nil || len(first.finished) != 1 || len(first.retried)+len(first.postRetried) != 0 {
			t.Fatalf("first day did not finish through ordinary task handback: finished=%d retried=%d post=%d error_type=%T panic_type=%T", len(first.finished), len(first.retried), len(first.postRetried), first.err, first.panicValue)
		}
		firstID := rowID(firstDay)
		queued, args := pending()
		if args.MinTime == nil || !args.MinTime.Equal(secondDay) || queued.RunAt.After(server.NowUtc()) || queued.RunMaxTimeSeconds != 3600 {
			t.Fatal("first committed day did not publish an immediately due bounded second-day owner")
		}
		secondCtx, cancelSecond := context.WithCancel(ctx)
		defer cancelSecond()
		secondWorker := newWorker(secondCtx)
		defer secondWorker.Close()
		waitForEligibility()
		secondDone := startAuditDailyCheckpointEval(secondWorker)
		for !isBlocked() {
			select {
			case result := <-secondDone:
				t.Fatalf("second-day invocation bypassed its actual database barrier: finished=%d retried=%d post=%d error_type=%T panic_type=%T", len(result.finished), len(result.retried), len(result.postRetried), result.err, result.panicValue)
			case <-ctx.Done():
				t.Fatal("second-day invocation never reached its database barrier")
			case <-time.After(10 * time.Millisecond):
			}
		}
		// Independent peers cannot duplicate the live RunOnce task even when its
		// body has stopped making progress. This qualifies current source only.
		for range 4 {
			peer := newWorker(ctx)
			finished, retried, post, err := peer.EvalTasks(1)
			peer.Close()
			if err != nil || len(finished)+len(retried)+len(post) != 0 {
				t.Fatalf("peer bypassed live daily task ownership: finished=%d retried=%d post=%d error_type=%T", len(finished), len(retried), len(post), err)
			}
		}
		cancelSecond()
		select {
		case result := <-secondDone:
			if result.panicValue != nil || result.err != nil || len(result.retried) != 1 || len(result.finished)+len(result.postRetried) != 0 {
				t.Fatalf("interrupted day did not use ordinary durable rescheduling: finished=%d retried=%d post=%d error_type=%T panic_type=%T", len(result.finished), len(result.retried), len(result.postRetried), result.err, result.panicValue)
			}
		case <-ctx.Done():
			t.Fatal("interrupted daily task did not unwind")
		}
		_, args = pending()
		if args.MinTime == nil || !args.MinTime.Equal(secondDay) || rowID(firstDay) != firstID || rowID(secondDay) != secondBefore {
			t.Fatal("interrupted day replayed its committed prefix or advanced uncommitted work")
		}
		server.Raise(hold.Rollback(ctx))
		if task.KickTasks(ctx, task.RunOnce("rollup_transfer_audit_events").String()) != 1 {
			t.Fatal("could not re-admit the retained retry without replacing its arguments")
		}
		lastWorker := newWorker(ctx)
		defer lastWorker.Close()
		for index := range 2 {
			waitForEligibility()
			finished, retried, post, err := lastWorker.EvalTasks(1)
			if err != nil || len(finished) != 1 || len(retried)+len(post) != 0 {
				t.Fatalf("remaining day failed through ordinary task completion: finished=%d retried=%d post=%d error_type=%T", len(finished), len(retried), len(post), err)
			}
			queued, args = pending()
			if index == 0 && (args.MinTime == nil || !args.MinTime.Equal(thirdDay) || queued.RunAt.After(server.NowUtc())) {
				t.Fatal("second completed day lost the final continuation")
			}
		}
		if args.MinTime != nil || queued.RunAt.Before(server.NowUtc().Add(5*time.Hour)) || rowID(firstDay) != firstID || rowID(secondDay) == secondBefore {
			t.Fatal("completed range changed its committed prefix or failed to restore ordinary cadence")
		}
		_ = rowID(thirdDay)
	})
}
