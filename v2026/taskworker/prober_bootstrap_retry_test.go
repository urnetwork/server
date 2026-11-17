package taskworker

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
	"github.com/urnetwork/server/v2026/task"
	"github.com/urnetwork/server/v2026/taskworker/work"
)

func bootstrapRetryUnrelatedTimeout(_ struct{}, s *session.ClientSession) (struct{}, error) {
	<-s.Ctx.Done()
	return struct{}{}, s.Ctx.Err()
}

// Run the actual production target registration and blocked model query. The
// fixture shortens only the timer; durable errors, task IDs and SQL finalization
// exercise the same path as the default two-minute timeout.
func TestProberBootstrapTimeoutRetryCapPreservesFailuresAndRunOnce(t *testing.T) {
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
		originalIds := map[string]server.Id{}
		server.Tx(ctx, func(tx server.PgTx) {
			work.ScheduleProberBootstrap(s, tx)
			task.ScheduleTaskInTx(tx, bootstrapRetryUnrelatedTimeout, struct{}{}, s, task.RunOnce("bootstrap_retry_unrelated"))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET
				run_at=now()-interval '5 seconds',release_time=now()-interval '5 seconds',
				run_max_time_seconds=0,reschedule_error_count=16`))
			for _, key := range []string{`["prober_bootstrap"]`, `["bootstrap_retry_unrelated"]`} {
				var id server.Id
				server.Raise(tx.QueryRow(ctx, `SELECT task_id FROM pending_task WHERE run_once_key=$1`, key).Scan(&id))
				originalIds[key] = id
			}
		})
		worker := InitTaskWorker(ctx)
		worker.AddTargets(task.NewTaskTarget(bootstrapRetryUnrelatedTimeout))
		server.Db(ctx, func(conn server.PgConn) {
			held, err := conn.Begin(ctx)
			server.Raise(err)
			defer held.Rollback(context.Background())
			server.RaisePgResult(held.Exec(ctx, `LOCK TABLE prober_shard_run IN ACCESS EXCLUSIVE MODE`))
			finished, retried, postRetries, err := worker.EvalTasks(2)
			if err != nil || len(finished) != 0 || len(retried) != 2 || len(postRetries) != 0 {
				t.Fatalf("timeout was masked or post was run: finished=%v retried=%v post=%v err=%v", finished, retried, postRetries, err)
			}
			var bootstrapId server.Id
			for _, key := range []string{`["prober_bootstrap"]`, `["bootstrap_retry_unrelated"]`} {
				var id server.Id
				var runAt, released time.Time
				var count, copies int
				var failure string
				server.Raise(conn.QueryRow(ctx, `SELECT task_id,run_at,release_time,reschedule_error_count,reschedule_error,
					(SELECT count(*) FROM pending_task WHERE run_once_key=$1)
					FROM pending_task WHERE run_once_key=$1`, key).Scan(&id, &runAt, &released, &count, &failure, &copies))
				if id != originalIds[key] || count != 17 || copies != 1 || !strings.Contains(failure, "Timeout") || !strings.Contains(failure, "context canceled") {
					t.Fatalf("retry lost durable failure or RunOnce identity: key=%s count=%d copies=%d error=%q", key, count, copies, failure)
				}
				delay := runAt.Sub(released)
				if key == `["prober_bootstrap"]` {
					bootstrapId = id
					if delay != work.ProberBootstrapTimeout {
						t.Fatalf("Bootstrap retry=%s, want 5m", delay)
					}
				} else if delay < 30*time.Minute || delay > 90*time.Minute+2*time.Second {
					t.Fatalf("unrelated timeout lost ordinary backoff: %s", delay)
				}
				t.Logf("%s count=%d retry_delay=%s failure_retained=true", key, count, delay)
			}
			server.Raise(held.Rollback(ctx))
			// Only the failure phase needs the shortened timer. Give normal
			// recovery its production budget even on a busy test host.
			task.DefaultMaxTime = oldMax
			server.Tx(ctx, func(tx server.PgTx) {
				work.ScheduleProberBootstrap(s, tx)
				server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET run_at=now()-interval '5 seconds',release_time=now()-interval '5 seconds' WHERE task_id=$1`, bootstrapId))
			})
			// Normal success still closes the same pending task and its Post
			// schedules one fresh recurring task at the original five minutes.
			finished, retried, postRetries, err = worker.EvalTasks(1)
			if err != nil || len(finished) != 1 || finished[0] != bootstrapId || len(retried) != 0 || len(postRetries) != 0 {
				t.Fatalf("successful recovery lost its post: finished=%v retried=%v post=%v err=%v", finished, retried, postRetries, err)
			}
			var count int
			var nextId server.Id
			var nextRun time.Time
			server.Raise(conn.QueryRow(ctx, `SELECT task_id,run_at,reschedule_error_count FROM pending_task WHERE run_once_key='["prober_bootstrap"]'`).Scan(&nextId, &nextRun, &count))
			if nextId == bootstrapId || count != 0 || nextRun.Before(server.NowUtc().Add(work.ProberBootstrapTimeout-time.Second)) {
				t.Fatal("success did not preserve recurring RunOnce semantics")
			}
		})
	})
}
