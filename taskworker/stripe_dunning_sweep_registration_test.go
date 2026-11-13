package taskworker

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/task"
)

// The production worker registers the hourly Stripe dunning sweep, and its
// startup schedules the first run an hour out (owner, 2026-11-13: "send now +
// schedule"). A restart keeps the pending run's time, so a fleet roll neither
// moves nor adds a run. The subnet operator's workload has no retail billing.
func TestStripeDunningSweepIsScheduledAtStartup(t *testing.T) {
	t.Setenv("WARP_DOMAIN", "stripe-dunning-sweep.example")
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		controller.SetStConfig(&controller.StConfig{Enabled: false})
		defer controller.SetStConfig(nil)
		worker := InitTaskWorker(ctx)
		defer worker.Close()
		functionName := task.NewTaskTarget(controller.SweepStripeDunning).TargetFunctionName()
		if !worker.HasTarget(functionName) {
			t.Fatalf("the production worker does not register %s", functionName)
		}

		readSchedule := func() (count int, runAt *time.Time) {
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(
					ctx,
					`SELECT count(*), max(run_at) FROM pending_task WHERE function_name = $1`,
					functionName,
				).Scan(&count, &runAt))
			})
			return
		}

		beforeInit := server.NowUtc()
		if err := InitTasksForProfile(ctx, WorkloadProfileProduction); err != nil {
			t.Fatal(err)
		}
		afterInit := server.NowUtc()
		count, runAt := readSchedule()
		if count != 1 || runAt == nil || runAt.Before(beforeInit.Add(time.Hour)) || afterInit.Add(time.Hour).Before(*runAt) {
			t.Fatalf("startup scheduled %d sweeps at %v, want one an hour after %s", count, runAt, beforeInit)
		}

		if err := InitTasksForProfile(ctx, WorkloadProfileProduction); err != nil {
			t.Fatal(err)
		}
		if restartCount, restartRunAt := readSchedule(); restartCount != 1 || restartRunAt == nil || !restartRunAt.Equal(*runAt) {
			t.Fatalf("a restart left %d sweeps at %v, want the one at %s", restartCount, restartRunAt, *runAt)
		}

		subnetWorker, err := InitTaskWorkerForProfile(ctx, nil, WorkloadProfileSubnetOperator)
		if err != nil {
			t.Fatal(err)
		}
		defer subnetWorker.Close()
		if subnetWorker.HasTarget(functionName) {
			t.Fatalf("the subnet operator's workload registers %s", functionName)
		}
	})
}
