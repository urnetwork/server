// Cleanup retains unfinished Posts for their longer recovery window.
package task

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// Success history, recoverable Post failure, expired Post failure, and future
// history coexist. Real execution and replay must preserve exactly the latter
// recoverable rows and one future cleanup successor.
func TestTaskCleanupRetainsRecoverablePostsAndRearms(t *testing.T) {
	runOnceGenerationEnv(t, func(t testing.TB, ctx context.Context) {
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		ids := []server.Id{server.NewId(), server.NewId(), server.NewId(), server.NewId()}
		past := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
		recentFailure := server.NowUtc().Add(-2 * 24 * time.Hour)
		server.Tx(ctx, func(tx server.PgTx) {
			for i, id := range ids {
				at := past
				if i == 1 {
					at = recentFailure
				} else if i == 3 {
					at = time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)
				}
				var postError *string
				if i == 1 || i == 2 {
					message := "synthetic recoverable post failure"
					postError = &message
				}
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO finished_task(task_id,function_name,args_json,client_address,
					run_at,run_priority,run_max_time_seconds,run_start_time,run_end_time,result_json,post_error,post_completed)
					VALUES($1,'synthetic.completed.task','{}','',$2,10,120,$2,$2,'{}',$3,$4)`, id, at, postError, postError == nil))
			}
		})
		worker := NewTaskWorkerWithDefaults(ctx)
		defer worker.Close()
		target := NewTaskTargetWithPost(TaskCleanup, TaskCleanupPost)
		worker.AddTargets(target)
		for range 2 {
			id := ScheduleTask(TaskCleanup, &TaskCleanupArgs{}, owner, RunAt(past))
			finished, retried, posts, err := worker.EvalTasks(1)
			if err != nil || len(finished) != 1 || finished[0] != id || len(retried)+len(posts) != 0 {
				t.Fatal("cleanup did not finish", finished, retried, posts, err)
			}
			retained := GetFinishedTasks(ctx, ids...)
			if len(retained) != 2 || retained[ids[1]] == nil || retained[ids[3]] == nil {
				t.Fatal("cleanup discarded recoverable work or retained expired history", len(retained))
			}
			server.Db(ctx, func(conn server.PgConn) {
				var count int
				var future bool
				server.Raise(conn.QueryRow(ctx, `SELECT count(*),COALESCE(bool_and(run_at>$2),false)
					FROM pending_task WHERE function_name=$1`, target.TargetFunctionName(), server.NowUtc()).Scan(&count, &future))
				if count != 1 || !future {
					t.Fatal("cleanup lost or duplicated recurrence", count, future)
				}
			})
		}
	})
}
