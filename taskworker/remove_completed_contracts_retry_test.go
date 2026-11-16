package taskworker

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
	"github.com/urnetwork/server/taskworker/work"
)

// A RemoveCompletedContracts run canceled at its MaxTime keeps no Post to
// re-arm its 30-minute gap. With the production registration it keeps its
// failure, count and RunOnce identity, and waits that gap before it retries,
// where ordinary backoff would restart it within seconds (Main, 2026-11-02: a
// timed-out run restarted after three seconds). The fixture shortens only the
// timer and holds the run's first read.
func TestRemoveCompletedContractsTimeoutRetriesAfterChainGap(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
		defer cancel()
		s := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer s.Cancel()
		oldMax, oldBase, oldCap := task.DefaultMaxTime, task.RescheduleTimeout, task.RescheduleBackoffMaxTimeout
		task.DefaultMaxTime, task.RescheduleTimeout, task.RescheduleBackoffMaxTimeout = 20*time.Millisecond, 2*time.Second, time.Hour
		defer func() {
			task.DefaultMaxTime, task.RescheduleTimeout, task.RescheduleBackoffMaxTimeout = oldMax, oldBase, oldCap
		}()
		const key = `["remove_completed_contracts"]`
		var id server.Id
		server.Tx(ctx, func(tx server.PgTx) {
			work.ScheduleRemoveCompletedContracts(s, tx)
			server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET
				run_at=now()-interval '5 seconds',release_time=now()-interval '5 seconds',
				run_max_time_seconds=0 WHERE run_once_key=$1`, key))
			server.Raise(tx.QueryRow(ctx, `SELECT task_id FROM pending_task WHERE run_once_key=$1`, key).Scan(&id))
		})
		worker := InitTaskWorker(ctx)
		server.Db(ctx, func(conn server.PgConn) {
			held, err := conn.Begin(ctx)
			server.Raise(err)
			defer held.Rollback(context.Background())
			// the run's first read, of completed balances, waits here past its MaxTime
			server.RaisePgResult(held.Exec(ctx, `LOCK TABLE transfer_balance IN ACCESS EXCLUSIVE MODE`))
			finished, retried, postRetries, err := worker.EvalTasks(1)
			if err != nil || len(finished) != 0 || len(retried) != 1 || retried[0] != id || len(postRetries) != 0 {
				t.Fatalf("the timeout was masked or its post ran: finished=%v retried=%v post=%v err=%v", finished, retried, postRetries, err)
			}
			var runAt, released time.Time
			var count, copies int
			var failure string
			server.Raise(conn.QueryRow(ctx, `SELECT run_at,release_time,reschedule_error_count,coalesce(reschedule_error,''),
				(SELECT count(*) FROM pending_task WHERE run_once_key=$2)
				FROM pending_task WHERE task_id=$1`, id, key).Scan(&runAt, &released, &count, &failure, &copies))
			if count != 1 || copies != 1 || !strings.Contains(failure, "Timeout") {
				t.Fatalf("the timeout lost its failure or RunOnce identity: count=%d copies=%d error=%q", count, copies, failure)
			}
			if delay := runAt.Sub(released); delay < 30*time.Minute {
				t.Fatalf("a run canceled at its MaxTime retries after %s, inside the chain's 30-minute gap", delay)
			}
		})
	})
}
