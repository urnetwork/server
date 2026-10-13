// The concurrency-cap count reads the function poll index by canonical name;
// its result must still be exactly the runnable rows of the exact function.
package task

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// Only rows of the exact function whose run_at has passed count. A future
// row, another function's row, and a row whose name shares the canonical
// name through a version path segment do not.
func TestCountAvailableByFunctionNameCountsExactRunnableRows(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession := session.NewLocalClientSession(ctx, "192.0.2.1:1", nil)
		defer clientSession.Cancel()

		var versionedTaskId server.Id
		server.Tx(ctx, func(tx server.PgTx) {
			ScheduleTaskInTx(tx, removedTargetTask, &removedTargetArgs{}, clientSession, RunAt(server.NowUtc().Add(-time.Minute)))
			ScheduleTaskInTx(tx, removedTargetTask, &removedTargetArgs{}, clientSession, RunAt(server.NowUtc().Add(-time.Second)))
			ScheduleTaskInTx(tx, removedTargetTask, &removedTargetArgs{}, clientSession, RunAt(server.NowUtc().Add(time.Hour)))
			ScheduleTaskInTx(tx, survivorTargetTask, &removedTargetArgs{}, clientSession, RunAt(server.NowUtc().Add(-time.Minute)))
			versionedTaskId = ScheduleTaskInTx(tx, survivorTargetTask, &removedTargetArgs{}, clientSession, RunAt(server.NowUtc().Add(-time.Minute)))
			// same canonical name as removedTargetTask, different exact name
			server.RaisePgResult(tx.Exec(ctx, `
				UPDATE pending_task
				SET function_name = regexp_replace($2::text, '^(.*)/([^/]+)$', '\1/v2/\2')
				WHERE task_id = $1
			`, versionedTaskId, NewTaskTarget(removedTargetTask).TargetFunctionName()))
			var canonicalMatchCount int
			server.Raise(tx.QueryRow(ctx, `
				SELECT count(*) FROM pending_task
				WHERE task_id = $1 AND regexp_replace(function_name, '/v[0-9]+', '', 'g') = $2 AND function_name <> $2
			`, versionedTaskId, NewTaskTarget(removedTargetTask).TargetFunctionName()).Scan(&canonicalMatchCount))
			if canonicalMatchCount != 1 {
				t.Fatal("synthetic versioned row does not share the canonical name")
			}
		})

		count, err := CountAvailableByFunctionName(ctx, removedTargetTask)
		if err != nil || count != 2 {
			t.Fatalf("counted %d runnable rows of the exact function (err=%v); want 2", count, err)
		}
	})
}
