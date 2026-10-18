// A Drain cannot close the session a Run collector lends to finalization, and
// finalization never borrows a session pgx has closed or one in a transaction.
package task

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// Records the context of the latest claim cursor declaration.
type taskClaimDeclarationTrace struct {
	stateLock sync.Mutex
	ctx       context.Context
}

func (self *taskClaimDeclarationTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.HasPrefix(data.SQL, `DECLARE pending_task_claim_candidates`) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.ctx = ctx
	}
	return ctx
}

func (self *taskClaimDeclarationTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {
}

func (self *taskClaimDeclarationTrace) declared() context.Context {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.ctx
}

// Polls until the query reports true.
func taskSessionAwait(ctx context.Context, query string, args ...any) {
	for {
		var done bool
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, query, args...).Scan(&done))
		}, server.OptNoRetry())
		if done {
			return
		}
		select {
		case <-ctx.Done():
			panic(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// One live task runs on the collector's session while its refill claim's cursor
// declaration waits on a table lock, and Drain cancels the run. pgx closed a
// session whose statement the cancellation interrupted, which dropped the live
// execution lock and left the live task's finalization a closed session: Run
// failed with "database ownership session is not idle". The refill must keep
// its statement, and the live task must finish once on that session.
func TestTaskRunDrainDuringRefillClaimKeepsFinalizationSession(t *testing.T) {
	runOnceGenerationEnv(t, func(t testing.TB, ctx context.Context) {
		client := session.NewLocalClientSession(ctx, "", nil)
		defer client.Cancel()
		scope := server.NewId()
		id := ScheduleTask(runOnceGenerationWork, &runOnceGenerationArgs{Scope: scope}, client,
			runOnceGenerationKey(scope), RunAt(server.NowUtc().Add(-time.Hour)))
		trace := &taskClaimDeclarationTrace{}
		queries, err := server.NewTestPgQueryScope(ctx, trace)
		server.Raise(err)
		defer func() { server.Raise(queries.Close()) }()

		entered, release := make(chan struct{}), make(chan struct{})
		refilling, locked := make(chan struct{}), make(chan struct{})
		tableLocked, unlockTable, tableUnlocked := make(chan struct{}), make(chan struct{}), make(chan struct{})
		var releaseOnce, lockedOnce, unlockOnce sync.Once
		unblock := func() { releaseOnce.Do(func() { close(release) }) }
		claimResumes := func() { lockedOnce.Do(func() { close(locked) }) }
		tableResumes := func() { unlockOnce.Do(func() { close(unlockTable) }) }
		var calls atomic.Int32
		var finalizedPid uint32
		target := &taskQueueProtocolTarget{runOnceGenerationTarget: &runOnceGenerationTarget{
			Target: NewTaskTarget(runOnceGenerationWork),
			before: func(taskCtx context.Context, _ *Task) error {
				if calls.Add(1) == 1 {
					close(entered)
				}
				select {
				case <-release:
					return nil
				case <-taskCtx.Done():
					return taskCtx.Err()
				}
			},
			after: func(tx server.PgTx, _ *Task) error {
				finalizedPid = tx.Conn().PgConn().PID()
				return nil
			},
		}}
		settings := DefaultTaskWorkerSettings()
		settings.BatchSize = 2
		settings.ClaimRegisteredTargetsOnly = true
		// Production refills a free slot right after each launch.
		settings.FairClaimFunctions = true
		worker := NewTaskWorker(ctx, settings)
		defer worker.Close()
		worker.AddTargets(target)
		var guardPid uint32
		worker.claimAfterCommit = func(guard *taskClaimGuard) {
			if guardPid == 0 {
				guardPid = guard.conn.Conn().PgConn().PID()
			}
		}
		claims := 0
		worker.claimBeforeQuery = func(server.PgTx) error {
			claims++
			if claims == 2 {
				close(refilling)
				taskQueueWait(ctx, locked)
			}
			return nil
		}

		done := make(chan any, 1)
		go func() { done <- server.HandleError(worker.Run) }()
		joined := false
		defer func() {
			claimResumes()
			tableResumes()
			unblock()
			if !joined {
				worker.runCancel()
				<-done
			}
		}()
		taskQueueWait(ctx, entered)
		taskQueueWait(ctx, refilling)

		// Hold every claim statement on pending_task until the drain lands.
		go func() {
			defer close(tableUnlocked)
			server.HandleError(func() {
				server.Tx(ctx, func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(ctx, `LOCK TABLE pending_task IN ACCESS EXCLUSIVE MODE`))
					close(tableLocked)
					taskQueueWait(ctx, unlockTable)
				}, server.OptNoRetry())
			})
		}()
		taskQueueWait(ctx, tableLocked)
		claimResumes()
		taskSessionAwait(ctx, `SELECT EXISTS (SELECT 1 FROM pg_locks WHERE pid=$1 AND NOT granted)`, guardPid)

		// Drain's cancellation, while the declaration waits in the server.
		worker.runCancel()
		if declared := trace.declared(); declared == nil {
			t.Fatal("refill claim did not declare its cursor")
		} else if declared.Err() != nil {
			// The cancellation reached the waiting statement. Let pgx close the
			// session before the declaration can resume.
			taskSessionAwait(ctx, `SELECT NOT EXISTS (SELECT 1 FROM pg_stat_activity WHERE pid=$1)`, guardPid)
		}
		tableResumes()
		taskQueueWait(ctx, tableUnlocked)
		unblock()
		worker.Drain()
		recovered := <-done
		joined = true

		if recovered != nil {
			t.Fatal("Run failed after Drain interrupted its refill claim", recovered)
		}
		finished := GetFinishedTasks(ctx, id)
		if calls.Load() != 1 || len(GetTasks(ctx, id)) != 0 || len(finished) != 1 || finished[id].RescheduleError != "" {
			t.Fatal("live task did not finish exactly once behind the drained refill", calls.Load(), len(finished))
		}
		if guardPid == 0 || finalizedPid != guardPid {
			t.Fatal("live task did not finalize on the claim session it ran under", guardPid, finalizedPid)
		}
	})
}

// A claim session pgx has closed, or one inside a transaction, is in use and
// never lent. The handback takes its own ownership session and commits once.
func TestTaskFinalizationNeverBorrowsAClosedOrBusyClaimSession(t *testing.T) {
	for _, state := range []string{"closed", "in transaction"} {
		t.Run(state, func(t *testing.T) {
			runOnceGenerationEnv(t, func(t testing.TB, ctx context.Context) {
				client := session.NewLocalClientSession(ctx, "", nil)
				defer client.Cancel()
				scope := server.NewId()
				id := ScheduleTask(runOnceGenerationWork, &runOnceGenerationArgs{Scope: scope}, client,
					runOnceGenerationKey(scope), RunAt(server.NowUtc().Add(-time.Hour)))
				worker := NewTaskWorkerWithDefaults(ctx)
				defer worker.Close()
				var finalizedPid uint32
				target := &taskQueueProtocolTarget{runOnceGenerationTarget: &runOnceGenerationTarget{
					Target: NewTaskTarget(runOnceGenerationWork),
					after: func(tx server.PgTx, _ *Task) error {
						finalizedPid = tx.Conn().PgConn().PID()
						return nil
					},
				}}
				worker.AddTargets(target)
				tasks, guard, err := worker.takeTasks(1)
				if guard != nil {
					defer guard.release()
				}
				if err != nil || len(tasks) != 1 || tasks[id] == nil {
					t.Fatal("claim fixture failed", err)
				}
				result := worker.executeTask(ctx, tasks[id], target)
				if result.err != nil {
					t.Fatal("fixture body failed", result.err)
				}
				guardPid := guard.conn.Conn().PgConn().PID()
				if state == "closed" {
					server.Raise(guard.conn.Conn().Close(ctx))
				} else {
					server.RaisePgResult(guard.conn.Exec(ctx, `BEGIN`))
				}

				var postRescheduled bool
				failure := server.HandleError(func() { _, postRescheduled = worker.finalizeTaskWithGuard(result, guard) })
				if failure != nil || postRescheduled {
					t.Fatal("finalization borrowed its claim session in use", failure)
				}
				if finalizedPid == 0 || finalizedPid == guardPid || guard.completionSessionError() != nil {
					t.Fatal("finalization did not take its own ownership session", guardPid, finalizedPid)
				}
				if len(GetTasks(ctx, id)) != 0 || len(GetFinishedTasks(ctx, id)) != 1 {
					t.Fatal("handback on its own session did not finish the task once")
				}
				if state == "in transaction" {
					server.RaisePgResult(guard.conn.Exec(ctx, `ROLLBACK`))
				}
			})
		})
	}
}
