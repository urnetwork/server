package taskworker

import (
	"context"
	"testing"
	"time"

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
		controller.SetVerifySettings(model.DefaultVerifySettings())
		defer controller.SetVerifySettings(model.DefaultVerifySettings())
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

// TestInitTasksReapsProviderEgressChainsWhileDisabled is the deployment-race
// regression: merely stopping new seeds leaves old RunOnce rows alive, and an
// already-claimed row can otherwise run and post a successor. Startup removes
// unclaimed, stale-geometry, and actively leased rows by canonical function
// name while leaving unrelated work intact.
func TestInitTasksReapsProviderEgressChainsWhileDisabled(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		t.Setenv("WARP_DOMAIN", "bringyour.com")
		t.Cleanup(server.Config.PushSimpleResource(
			"provider_egress_probe.yml",
			[]byte("enabled: false\n"),
		))
		ctx := context.Background()
		clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer clientSession.Cancel()

		var survivorTaskId server.Id
		server.Tx(ctx, func(tx server.PgTx) {
			for shardIndex, shardCount := range []int{3, 99, 3} {
				task.ScheduleTaskInTx(
					tx,
					work.ProviderEgressProbe,
					&work.ProviderEgressProbeArgs{ShardIndex: shardIndex, ShardCount: shardCount},
					clientSession,
					task.RunOnce("provider_egress_probe", shardIndex),
				)
			}
			survivorTaskId = task.ScheduleTaskInTx(tx, orphanSeedTask, &orphanSeedArgs{}, clientSession)
			server.RaisePgResult(tx.Exec(
				ctx,
				`UPDATE pending_task
				 SET claim_time = $2, release_time = $3
				 WHERE run_once_key = $1`,
				`["provider_egress_probe",2]`,
				server.NowUtc(),
				server.NowUtc().Add(time.Hour),
			))
		})

		countProbeTasks := func() (count int) {
			server.Db(ctx, func(conn server.PgConn) {
				result, err := conn.Query(
					ctx,
					`SELECT count(*) FROM pending_task WHERE function_name = ANY($1)`,
					work.ProviderEgressProbeTaskFunctionNames(),
				)
				server.WithPgResult(result, err, func() {
					connect.AssertEqual(t, result.Next(), true)
					server.Raise(result.Scan(&count))
				})
			})
			return
		}
		connect.AssertEqual(t, countProbeTasks(), 3)

		InitTasks(ctx)

		connect.AssertEqual(t, countProbeTasks(), 0)
		var survivorCount int
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(ctx, `SELECT count(*) FROM pending_task WHERE task_id = $1`, survivorTaskId)
			server.WithPgResult(result, err, func() {
				connect.AssertEqual(t, result.Next(), true)
				server.Raise(result.Scan(&survivorCount))
			})
		})
		if survivorCount != 1 {
			t.Fatalf("disabled probe cleanup removed unrelated task %s (remaining=%d)", survivorTaskId, survivorCount)
		}
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
