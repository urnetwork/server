package task

import (
	"context"
	"testing"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
)

type removedTargetArgs struct{}
type removedTargetResult struct{}

func removedTargetTask(args *removedTargetArgs, clientSession *session.ClientSession) (*removedTargetResult, error) {
	return &removedTargetResult{}, nil
}

func survivorTargetTask(args *removedTargetArgs, clientSession *session.ClientSession) (*removedTargetResult, error) {
	return &removedTargetResult{}, nil
}

// TestRemovePendingTasksForFunction covers the reaper for targets deleted
// from the codebase (wired into taskworker InitTasks): rows for the named
// function are removed — claimed or not — and rows for other functions are
// untouched. Without the reaper such rows error with ErrTargetNotFound and
// reschedule on the clamped skew backoff forever.
func TestRemovePendingTasksForFunction(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer clientSession.Cancel()

		var removedName string
		var survivorTaskId server.Id
		server.Tx(ctx, func(tx server.PgTx) {
			removedTaskId := ScheduleTaskInTx(tx, removedTargetTask, &removedTargetArgs{}, clientSession)
			survivorTaskId = ScheduleTaskInTx(tx, survivorTargetTask, &removedTargetArgs{}, clientSession)

			result, err := tx.Query(ctx,
				`SELECT function_name FROM pending_task WHERE task_id = $1`,
				removedTaskId,
			)
			server.WithPgResult(result, err, func() {
				connect.AssertEqual(t, result.Next(), true)
				server.Raise(result.Scan(&removedName))
			})
		})
		connect.AssertNotEqual(t, removedName, "")

		var removedCount int64
		server.Tx(ctx, func(tx server.PgTx) {
			removedCount = RemovePendingTasksForFunctionInTx(ctx, tx, removedName)
		})
		connect.AssertEqual(t, removedCount, int64(1))

		countByTaskId := func(taskId server.Id) (count int) {
			server.Db(ctx, func(conn server.PgConn) {
				result, err := conn.Query(ctx,
					`SELECT COUNT(*) FROM pending_task WHERE task_id = $1`,
					taskId,
				)
				server.WithPgResult(result, err, func() {
					connect.AssertEqual(t, result.Next(), true)
					server.Raise(result.Scan(&count))
				})
			})
			return
		}

		var removedRemaining int
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(ctx,
				`SELECT COUNT(*) FROM pending_task WHERE function_name = $1`,
				removedName,
			)
			server.WithPgResult(result, err, func() {
				connect.AssertEqual(t, result.Next(), true)
				server.Raise(result.Scan(&removedRemaining))
			})
		})
		connect.AssertEqual(t, removedRemaining, 0)
		connect.AssertEqual(t, countByTaskId(survivorTaskId), 1)

		// idempotent on an already-clean table
		server.Tx(ctx, func(tx server.PgTx) {
			connect.AssertEqual(t, RemovePendingTasksForFunctionInTx(ctx, tx, removedName), int64(0))
		})
	})
}
