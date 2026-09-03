package taskworker

import (
	"context"
	"testing"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
	"github.com/urnetwork/server/taskworker/work"
)

type orphanSeedArgs struct{}
type orphanSeedResult struct{}

func orphanSeedTask(args *orphanSeedArgs, clientSession *session.ClientSession) (*orphanSeedResult, error) {
	return &orphanSeedResult{}, nil
}

// TestInitTasksReapsVerificationChainsWhileDisabled is the regression for
// main's RefreshVerifyProxyEgress row retrying "Interrupted: Done" / context
// canceled hundreds of times although st.yml deliberately had enabled=false.
// Old pending RunOnce chains are removed, and the normal InitTasks scheduling
// pass cannot recreate any verification task while disabled.
func TestInitTasksReapsVerificationChainsWhileDisabled(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer clientSession.Cancel()

		// Enabling the subsystem makes RefreshVerifyProxyEgress's schedule use
		// its configured interval. This test owns that enabled fixture and must
		// not fall through to the deployment-only verify.yml vault resource.
		controller.SetVerifySettings(model.DefaultVerifySettings())
		defer controller.SetVerifySettings(nil)
		controller.SetStConfig(&controller.StConfig{Enabled: true})
		defer controller.SetStConfig(nil)
		server.Tx(ctx, func(tx server.PgTx) {
			work.ScheduleSweepVerifyTrails(clientSession, tx)
			work.ScheduleRollupVerifyProviderStats(clientSession, tx)
			work.ScheduleRemoveOldVerifyProviderStats(clientSession, tx)
			work.ScheduleRefreshVerifyProxyEgress(clientSession, tx)
		})

		countVerifyTasks := func() (count int) {
			server.Db(ctx, func(conn server.PgConn) {
				result, err := conn.Query(
					ctx,
					`SELECT count(*) FROM pending_task WHERE function_name = ANY($1)`,
					work.VerifyTaskFunctionNames(),
				)
				server.WithPgResult(result, err, func() {
					connect.AssertEqual(t, result.Next(), true)
					server.Raise(result.Scan(&count))
				})
			})
			return
		}

		connect.AssertEqual(t, countVerifyTasks(), len(work.VerifyTaskFunctionNames()))
		controller.SetStConfig(&controller.StConfig{Enabled: false})

		InitTasks(ctx)

		connect.AssertEqual(t, countVerifyTasks(), 0)
	})
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
		connect.AssertEqual(
			t,
			taskWorker.HasTarget("github.com/urnetwork/server/taskworker/work.ProviderEgressProbe"),
			true,
		)

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
