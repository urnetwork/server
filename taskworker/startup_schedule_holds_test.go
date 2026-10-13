// Regressions for the 2026-10-12 taskworker deploy, whose candidates kept
// failing readiness. Startup scheduling ran the retired-row reap, a scan of
// the whole queue, in the transaction that had already upserted every shared
// run-once row. Sibling candidates and live workers waited on those rows for
// the whole scan, warp stopped each candidate at its poll timeout, and each
// stopped candidate's backend kept running and waiting after its process was
// gone. Barriers here are real row locks held by a second session.
package taskworker

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"testing"
	"time"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
	"github.com/urnetwork/server/taskworker/work"
)

// Arms a pending row under the exact production name of a removed target,
// the way production acquired one, and returns its id.
func seedRetiredTaskForTest(ctx context.Context) (retiredTaskId server.Id) {
	clientSession := session.NewLocalClientSession(ctx, "192.0.2.1:1", nil)
	defer clientSession.Cancel()
	server.Tx(ctx, func(tx server.PgTx) {
		retiredTaskId = task.ScheduleTaskInTx(tx, orphanSeedTask, &orphanSeedArgs{}, clientSession)
		server.RaisePgResult(tx.Exec(
			ctx,
			`UPDATE pending_task SET function_name = $2 WHERE task_id = $1`,
			retiredTaskId,
			"github.com/urnetwork/server/controller.RefreshTransferBalances",
		))
	})
	return
}

// Holds the row locks taken by lockSql in an open transaction on a second
// session. The release is idempotent and safe to defer.
func holdPendingTaskRowsForTest(t testing.TB, ctx context.Context, lockSql string, args ...any) (holderPid int32, release func()) {
	t.Helper()
	holder, err := server.AcquireMaintenanceDbConn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	holderTx, err := holder.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadWrite})
	if err != nil {
		holder.Release()
		t.Fatal(err)
	}
	released := false
	release = func() {
		if !released {
			released = true
			_ = holderTx.Rollback(context.Background())
			holder.Release()
		}
	}
	server.Raise(holderTx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&holderPid))
	tag, err := holderTx.Exec(ctx, lockSql, args...)
	if err != nil || tag.RowsAffected() != 1 {
		release()
		t.Fatalf("holder did not lock exactly one row: rows=%d err=%v", tag.RowsAffected(), err)
	}
	return
}

// Reports whether a sibling candidate's upsert of the export_stats run-once
// row could proceed now, without waiting for its lock.
func probeExportStatsRowForTest(ctx context.Context) error {
	conn, err := server.AcquireMaintenanceDbConn(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	probeTx, err := conn.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadWrite})
	if err != nil {
		return err
	}
	defer probeTx.Rollback(context.Background())
	var taskId server.Id
	return probeTx.QueryRow(ctx, `SELECT task_id FROM pending_task WHERE run_once_key = '["export_stats"]' FOR UPDATE NOWAIT`).Scan(&taskId)
}

// A retired-row reap that cannot finish must neither keep the shared run-once
// rows locked nor hold back the scheduling that readiness waits on. A second
// session locks the retired row, so a reap inside scheduling waits there,
// exactly as the production reap kept scanning. The export_stats chain is
// armed first, as every recurring chain is in production.
func TestStartupSchedulingHoldsNoRunOnceRowWhileRetiredReapWaits(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		clientSession := session.NewLocalClientSession(ctx, "192.0.2.1:1", nil)
		defer clientSession.Cancel()
		server.Tx(ctx, func(tx server.PgTx) {
			work.ScheduleExportStats(clientSession, tx)
		})
		retiredTaskId := seedRetiredTaskForTest(ctx)
		holderPid, release := holdPendingTaskRowsForTest(t, ctx, `SELECT 1 FROM pending_task WHERE task_id = $1 FOR UPDATE`, retiredTaskId)
		defer release()

		var scheduleErr error
		scheduled := make(chan struct{})
		go func() {
			defer close(scheduled)
			defer func() {
				if recovered := recover(); recovered != nil {
					scheduleErr = fmt.Errorf("startup scheduling panic: %v", recovered)
				}
			}()
			scheduleErr = initTaskScheduleForProfile(ctx, WorkloadProfileProduction)
		}()
		defer func() {
			cancel()
			release()
			<-scheduled
		}()

		// Scheduling either commits, or a statement waits behind the holder.
		server.Db(ctx, func(observer server.PgConn) {
			for {
				select {
				case <-scheduled:
					return
				default:
				}
				var waitingCount int
				server.Raise(observer.QueryRow(ctx, `
					SELECT count(*) FROM pg_stat_activity WHERE $1::int = ANY(pg_blocking_pids(pid))
				`, holderPid).Scan(&waitingCount))
				if 0 < waitingCount {
					t.Fatalf(
						"startup scheduling waits on the retired-row reap; a sibling candidate's export_stats upsert meanwhile gets: %v",
						probeExportStatsRowForTest(ctx),
					)
				}
				select {
				case <-ctx.Done():
					t.Fatalf("startup scheduling neither committed nor waited on the retired row: %v", ctx.Err())
				default:
					runtime.Gosched()
				}
			}
		})
		if scheduleErr != nil {
			t.Fatal(scheduleErr)
		}
		if err := probeExportStatsRowForTest(ctx); err != nil {
			t.Fatalf("committed startup scheduling left the export_stats run-once row locked: %v", err)
		}
		var retiredCount int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM pending_task WHERE task_id = $1`, retiredTaskId).Scan(&retiredCount))
		})
		if retiredCount != 1 {
			t.Fatalf("startup scheduling touched the held retired row: remaining=%d", retiredCount)
		}
	})
}

// Production profile: another owner holding a shared run-once row.
func TestStartupSchedulingBoundsProductionWaitOnHeldRunOnceRow(t *testing.T) {
	testStartupSchedulingBoundsWaitOnHeldRunOnceRow(t, WorkloadProfileProduction)
}

// The narrower profile uses its own scheduling transaction and the same bound.
func TestStartupSchedulingBoundsSubnetOperatorWaitOnHeldRunOnceRow(t *testing.T) {
	testStartupSchedulingBoundsWaitOnHeldRunOnceRow(t, WorkloadProfileSubnetOperator)
}

// A stopped candidate only closes its socket, so the server must end its
// wait. Another owner holding a shared run-once row ends the scheduling
// attempt with a statement timeout while its caller is still live, and the
// startup owner retries that as an unavailable read. Without a bound the
// attempt waited until its caller's deadline, and once its process stopped
// the backend went on waiting for the holder.
func testStartupSchedulingBoundsWaitOnHeldRunOnceRow(t *testing.T, profile WorkloadProfile) {
	t.Helper()
	previousTimeout := taskworkerStartupStatementTimeout
	taskworkerStartupStatementTimeout = time.Second
	defer func() {
		taskworkerStartupStatementTimeout = previousTimeout
	}()
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		clientSession := session.NewLocalClientSession(ctx, "192.0.2.1:1", nil)
		defer clientSession.Cancel()
		server.Tx(ctx, func(tx server.PgTx) {
			work.ScheduleExportStats(clientSession, tx)
		})
		_, release := holdPendingTaskRowsForTest(t, ctx, `UPDATE pending_task SET args_json = args_json WHERE run_once_key = '["export_stats"]'`)
		defer release()

		admission, cancelAdmission := context.WithCancel(ctx)
		defer cancelAdmission()
		stopCount := 0
		worker, err := startTaskworkerRuntime(admission, ctx, func() { stopCount++ }, RunOptions{Port: 8080, Count: 1, BatchSize: 1, WorkloadProfile: profile})
		var pgErr *pgconn.PgError
		if admission.Err() != nil || !errors.As(err, &pgErr) || pgErr.Code != pgerrcode.QueryCanceled {
			t.Fatalf("%q startup scheduling waited on a held run-once row past its statement bound: caller=%v err=%v", profile, admission.Err(), err)
		}
		if worker != nil || stopCount != 0 || !router.RetryableStartupReadinessError(errors.Join(err, admission.Err())) {
			t.Fatalf("%q bounded scheduling wait was not a retryable startup read without a runtime: worker=%v stops=%d err=%v", profile, worker != nil, stopCount, err)
		}
	})
}

// The reap runs after admission in its own bounded transactions. A retired
// row that another owner holds ends that reap with the row kept for a later
// start; once the row is free, the next reap removes exactly the retired rows.
func TestRetiredTaskReapIsBoundedAndConverges(t *testing.T) {
	previousTimeout := taskworkerStartupStatementTimeout
	taskworkerStartupStatementTimeout = time.Second
	defer func() {
		taskworkerStartupStatementTimeout = previousTimeout
	}()
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		retiredTaskId := seedRetiredTaskForTest(ctx)
		var survivorTaskId server.Id
		clientSession := session.NewLocalClientSession(ctx, "192.0.2.1:1", nil)
		defer clientSession.Cancel()
		server.Tx(ctx, func(tx server.PgTx) {
			survivorTaskId = task.ScheduleTaskInTx(tx, orphanSeedTask, &orphanSeedArgs{}, clientSession)
		})
		countTask := func(taskId server.Id) (count int) {
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM pending_task WHERE task_id = $1`, taskId).Scan(&count))
			})
			return
		}

		_, release := holdPendingTaskRowsForTest(t, ctx, `SELECT 1 FROM pending_task WHERE task_id = $1 FOR UPDATE`, retiredTaskId)
		defer release()
		reapRetiredTasks(ctx)
		if ctx.Err() != nil || countTask(retiredTaskId) != 1 {
			t.Fatalf("reap of a held retired row did not end within its bound with the row kept: caller=%v", ctx.Err())
		}

		release()
		reapRetiredTasks(ctx)
		if retiredCount, survivorCount := countTask(retiredTaskId), countTask(survivorTaskId); retiredCount != 0 || survivorCount != 1 {
			t.Fatalf("free reap did not remove exactly the retired row: retired=%d survivor=%d", retiredCount, survivorCount)
		}
	})
}

// The readiness gate itself: the real runtime constructor, whose return
// latches readiness, admits its worker while a retired row is held, and its
// asynchronous reap removes that row once the holder lets go.
func TestTaskworkerRuntimeAdmitsBeforeRetiredReap(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		clientSession := session.NewLocalClientSession(ctx, "192.0.2.1:1", nil)
		defer clientSession.Cancel()
		server.Tx(ctx, func(tx server.PgTx) {
			work.ScheduleExportStats(clientSession, tx)
		})
		retiredTaskId := seedRetiredTaskForTest(ctx)
		holderPid, release := holdPendingTaskRowsForTest(t, ctx, `SELECT 1 FROM pending_task WHERE task_id = $1 FOR UPDATE`, retiredTaskId)
		defer release()

		lifetime, cancelLifetime := context.WithCancel(ctx)
		defer cancelLifetime()
		var runtimeOwner taskworkerRuntime
		var startErr error
		started := make(chan struct{})
		go func() {
			defer close(started)
			runtimeOwner, startErr = startTaskworkerRuntime(ctx, lifetime, cancelLifetime, RunOptions{Port: 8080, Count: 1, BatchSize: 1})
		}()
		defer func() {
			cancel()
			release()
			<-started
			if runtimeOwner != nil {
				runtimeOwner.Drain()
				runtimeOwner.WaitFinalHandback()
			}
			cancelLifetime()
		}()

		// A reap waiting behind the holder is acceptable only outside the
		// scheduling transaction, which holds the export_stats row.
		server.Db(ctx, func(observer server.PgConn) {
			for {
				select {
				case <-started:
					return
				default:
				}
				var waitingCount int
				server.Raise(observer.QueryRow(ctx, `
					SELECT count(*) FROM pg_stat_activity WHERE $1::int = ANY(pg_blocking_pids(pid))
				`, holderPid).Scan(&waitingCount))
				if 0 < waitingCount {
					if err := probeExportStatsRowForTest(ctx); err != nil {
						t.Fatalf("runtime construction waits on the retired-row reap with the export_stats run-once row held: %v", err)
					}
				}
				select {
				case <-ctx.Done():
					t.Fatalf("runtime construction did not admit while the retired row was held: %v", ctx.Err())
				default:
					runtime.Gosched()
				}
			}
		})
		if startErr != nil || runtimeOwner == nil {
			t.Fatalf("runtime construction failed while the retired row was held: %v", startErr)
		}

		release()
		server.Db(ctx, func(observer server.PgConn) {
			for {
				var retiredCount int
				server.Raise(observer.QueryRow(ctx, `SELECT count(*) FROM pending_task WHERE task_id = $1`, retiredTaskId).Scan(&retiredCount))
				if retiredCount == 0 {
					return
				}
				select {
				case <-ctx.Done():
					t.Fatalf("admitted runtime never reaped the released retired row: %v", ctx.Err())
				default:
					runtime.Gosched()
				}
			}
		})
	})
}
