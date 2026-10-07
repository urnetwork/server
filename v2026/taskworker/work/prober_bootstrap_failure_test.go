package work

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
	"github.com/urnetwork/server/v2026/task"
)

// An actual blocked model query drives the real target timeout, worker
// finalization, and RunOnce row. This distinguishes durable backoff from a lost
// recurring chain without waiting for a production two-minute timeout.
func TestProberBootstrapTimeoutRetainsTaskAndInheritedBackoff(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		s := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer s.Cancel()
		oldMax, oldBase, oldCap := task.DefaultMaxTime, task.RescheduleTimeout, task.RescheduleBackoffMaxTimeout
		task.DefaultMaxTime, task.RescheduleTimeout, task.RescheduleBackoffMaxTimeout = 20*time.Millisecond, 2*time.Second, time.Hour
		defer func() {
			task.DefaultMaxTime, task.RescheduleTimeout, task.RescheduleBackoffMaxTimeout = oldMax, oldBase, oldCap
		}()
		server.Tx(ctx, func(tx server.PgTx) {
			ScheduleProberBootstrap(s, tx)
			server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET
				run_at=now()-interval '5 seconds',release_time=now()-interval '5 seconds',
				run_max_time_seconds=0,reschedule_error_count=10
				WHERE run_once_key='["prober_bootstrap"]'`))
		})
		worker := task.NewTaskWorkerWithDefaults(ctx)
		worker.AddTargets(task.NewTaskTargetWithPost(ProberBootstrap, ProberBootstrapPost))
		server.Db(ctx, func(conn server.PgConn) {
			held, err := conn.Begin(ctx)
			server.Raise(err)
			defer held.Rollback(context.Background())
			server.RaisePgResult(held.Exec(ctx, `LOCK TABLE prober_shard_run IN ACCESS EXCLUSIVE MODE`))
			before := server.NowUtc()
			finished, retried, postRetries, err := worker.EvalTasks(1)
			if err != nil || len(finished) != 0 || len(retried) != 1 || len(postRetries) != 0 {
				t.Fatalf("timeout did not retain its failed task: finished=%v retried=%v post=%v err=%v", finished, retried, postRetries, err)
			}
			var runAt, releaseTime time.Time
			var count int
			var failure string
			server.Raise(conn.QueryRow(ctx, `SELECT run_at,release_time,reschedule_error_count,reschedule_error
				FROM pending_task WHERE task_id=$1`, retried[0]).Scan(&runAt, &releaseTime, &count, &failure))
			if count != 11 || !strings.Contains(failure, "Timeout") || releaseTime.After(server.NowUtc()) ||
				runAt.Before(before.Add(2048*time.Second)) || runAt.After(server.NowUtc().Add(2051*time.Second)) {
				t.Fatalf("unexpected durable timeout retry: delay=%s errors=%d released=%s failure=%q", runAt.Sub(before), count, releaseTime, failure)
			}
			t.Logf("actual blocked Bootstrap: same task retained, count=%d, retry_delay=%s, no success/post", count, runAt.Sub(before))
			server.Raise(held.Rollback(ctx))
			// Startup brings the pending task forward without erasing failure
			// evidence. A subsequent failure still inherits the high count.
			server.Tx(ctx, func(tx server.PgTx) { ScheduleProberBootstrap(s, tx) })
			server.Raise(conn.QueryRow(ctx, `SELECT run_at,reschedule_error_count FROM pending_task WHERE task_id=$1`, retried[0]).Scan(&runAt, &count))
			if runAt.After(server.NowUtc()) || count != 11 {
				t.Fatalf("startup changed the wrong retry state: run_at=%s errors=%d", runAt, count)
			}
		})
	})
}
