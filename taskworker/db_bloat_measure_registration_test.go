// The weekly bloat measurement (PARTITION-MAINTENANCE-PLAN.md A4) belongs to
// the production registry and startup, and startup seeds its chain without
// moving a scheduled run.
package taskworker

import (
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/task"
	"github.com/urnetwork/server/taskworker/work"
)

// The serving worker must register the chain; the subnet operator's workload
// does not measure the database.
func TestMeasureDbBloatProductionRegistryOwnsTheChain(t *testing.T) {
	name := task.NewTaskTarget(work.MeasureDbBloat).TargetFunctionName()
	for _, profile := range []WorkloadProfile{WorkloadProfileProduction, WorkloadProfileSubnetOperator} {
		worker, err := InitTaskWorkerForProfile(t.Context(), nil, profile)
		if err != nil {
			t.Fatal(err)
		}
		present := worker.HasTarget(name)
		worker.Close()
		if present != (profile == WorkloadProfileProduction) {
			t.Fatalf("bloat measurement registration profile=%q present=%t", profile, present)
		}
	}
}

// Startup seeds one chain to run after its first-run delay. A later startup
// leaves a scheduled run where it is: a deploy must not pull the next week's
// run forward or reset the cursor its predecessor wrote.
func TestMeasureDbBloatStartupSeedsTheChainOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		controller.SetStConfig(&controller.StConfig{Enabled: false})
		defer controller.SetStConfig(nil)
		controller.SetVerifySettings(model.DefaultVerifySettings())
		defer controller.SetVerifySettings(nil)
		name := task.NewTaskTarget(work.MeasureDbBloat).TargetFunctionName()
		chain := func() []*task.Task {
			tasks := []*task.Task{}
			for _, pending := range task.GetTasks(ctx, task.ListPendingTasks(ctx)...) {
				if pending.FunctionName == name {
					tasks = append(tasks, pending)
				}
			}
			return tasks
		}

		// a timestamp keeps microseconds
		before := server.NowUtc().Truncate(time.Microsecond)
		if err := initTaskScheduleForProfile(ctx, WorkloadProfileProduction); err != nil {
			t.Fatal(err)
		}
		seeded := chain()
		firstRunDelay := server.DefaultDbBloatMeasureSettings().FirstRunDelay
		if len(seeded) != 1 || seeded[0].RunAt.Before(before.Add(firstRunDelay)) || server.NowUtc().Add(firstRunDelay).Before(seeded[0].RunAt) {
			t.Fatalf("startup seeded %d chain tasks: %+v", len(seeded), seeded)
		}

		// the predecessor scheduled the next week's slot with its cursor
		nextWeek := server.NowUtc().Add(6 * 24 * time.Hour).Truncate(time.Second)
		cursorJson := `{"cursor":{"table":"synthetic_bloat_c","catalog_done":true}}`
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`UPDATE pending_task SET run_at = $2, args_json = $3 WHERE task_id = $1`,
				seeded[0].TaskId,
				nextWeek,
				cursorJson,
			))
		})
		if err := initTaskScheduleForProfile(ctx, WorkloadProfileProduction); err != nil {
			t.Fatal(err)
		}
		after := chain()
		if len(after) != 1 || after[0].TaskId != seeded[0].TaskId || !after[0].RunAt.Equal(nextWeek) || after[0].ArgsJson != cursorJson {
			t.Fatalf("a later startup changed the chain: %+v", after)
		}
	})
}
