package task

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// TestRunPostOrphanedFinishedTaskCompletes is the regression test for a
// forever-rescheduling task.
//
// `RemoveFinishedTasks` reaps a finished_task row unconditionally past
// postErrorMinTime, explicitly so a repeatedly-failing post "cannot strand
// forever". But the RunPost pending task that references the row was left
// behind, and RunPost errored with "Finished task not found." — a condition
// that can never clear, because RunPost is scheduled in the same tx that
// writes the row, so its absence always means the reaper got there first. The
// task then rescheduled against a row that will never return (observed at
// ~8/min in production).
//
// Without the fix RunPost returns an error here and the caller reschedules.
func TestRunPostOrphanedFinishedTaskCompletes(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer clientSession.Cancel()

		taskWorker := NewTaskWorkerWithDefaults(ctx)

		// a task id with no finished_task row: exactly the post-reap state
		orphanTaskId := server.NewId()

		before := testutil.ToFloat64(orphanedRunPostCounter)
		result, err := taskWorker.RunPost(&RunPostArgs{TaskId: orphanTaskId}, clientSession)
		if err != nil {
			t.Fatalf("an orphaned run post must complete so the pending task clears, got err = %s", err)
		}
		connect.AssertNotEqual(t, result, nil)
		if after := testutil.ToFloat64(orphanedRunPostCounter); after != before+1 {
			t.Fatalf("orphaned run post counter = %v, want %v", after, before+1)
		}

		// RunPostPost is the completion write; it must also tolerate the
		// missing row, or the task fails at the post step instead
		server.Tx(ctx, func(tx server.PgTx) {
			if err := taskWorker.RunPostPost(
				&RunPostArgs{TaskId: orphanTaskId},
				result,
				clientSession,
				tx,
			); err != nil {
				t.Fatalf("run post post must tolerate the reaped row, got err = %s", err)
			}
		})
	})
}
