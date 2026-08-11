package taskworker

import (
	"context"
	"testing"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

type orphanSeedArgs struct{}
type orphanSeedResult struct{}

func orphanSeedTask(args *orphanSeedArgs, clientSession *session.ClientSession) (*orphanSeedResult, error) {
	return &orphanSeedResult{}, nil
}

// TestInitTasksReapsRemovedTargets is the integration test for the
// removed-target reap wiring: a pending row carrying the exact production
// function name of a removed target survives nothing but an InitTasks run.
// The name is spelled out literally so a typo in removedTaskTargets — which
// would silently never match the production rows — fails here.
func TestInitTasksReapsRemovedTargets(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		const removedTarget = "github.com/urnetwork/server/controller.RefreshTransferBalances"

		clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer clientSession.Cancel()

		// seed an orphan the way production got one: a normally scheduled
		// task whose function later ceased to exist
		var taskId server.Id
		server.Tx(ctx, func(tx server.PgTx) {
			taskId = task.ScheduleTaskInTx(tx, orphanSeedTask, &orphanSeedArgs{}, clientSession)
			server.RaisePgResult(tx.Exec(
				ctx,
				`UPDATE pending_task SET function_name = $2 WHERE task_id = $1`,
				taskId,
				removedTarget,
			))
		})

		countForFunction := func(functionName string) (count int) {
			server.Db(ctx, func(conn server.PgConn) {
				result, err := conn.Query(
					ctx,
					`SELECT COUNT(*) FROM pending_task WHERE function_name = $1`,
					functionName,
				)
				server.WithPgResult(result, err, func() {
					connect.AssertEqual(t, result.Next(), true)
					server.Raise(result.Scan(&count))
				})
			})
			return
		}
		connect.AssertEqual(t, countForFunction(removedTarget), 1)

		InitTasks(ctx)

		// the orphan is reaped...
		connect.AssertEqual(t, countForFunction(removedTarget), 0)

		// ...while the normal seeding happened
		var pendingCount int
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(ctx, `SELECT COUNT(*) FROM pending_task`)
			server.WithPgResult(result, err, func() {
				connect.AssertEqual(t, result.Next(), true)
				server.Raise(result.Scan(&pendingCount))
			})
		})
		connect.AssertEqual(t, 0 < pendingCount, true)
	})
}

// TestRemovedTaskTargetsAreNotRegistered guards the reap list itself: a live
// function name on removedTaskTargets would delete a healthy recurring chain
// at every taskworker start. Every entry must be absent from the worker's
// registry, and the registry lookup is sanity-checked against a name that is
// registered.
func TestRemovedTaskTargetsAreNotRegistered(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		taskWorker := InitTaskWorker(ctx)

		// positive control: a literal alternate name registered in
		// InitTaskWorkerWithSettings
		connect.AssertEqual(t, taskWorker.HasTarget("main.TaskCleanup"), true)

		for _, functionName := range removedTaskTargets {
			if taskWorker.HasTarget(functionName) {
				t.Fatalf("removedTaskTargets lists a registered function; InitTasks would reap a live task chain: %s", functionName)
			}
		}
	})
}
