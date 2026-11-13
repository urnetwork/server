// USDC provider payouts ended (owner decisions 2026-11-12 and 2026-11-13), and
// with them the payout planner that refreshed the country reliability
// multipliers at the end of each weekly plan. Their weekly refresh belongs to
// the production registry and startup now, and startup seeds its chain without
// moving a queued run. Before, nothing registered or seeded a refresh.
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

// The serving worker registers the refresh, where operator block points are
// awarded; the subnet operator's workload does not.
func TestRefreshCountryReliabilityMultipliersProductionRegistryOwnsTheChain(t *testing.T) {
	name := task.NewTaskTarget(work.RefreshCountryReliabilityMultipliers).TargetFunctionName()
	for _, profile := range []WorkloadProfile{WorkloadProfileProduction, WorkloadProfileSubnetOperator} {
		worker, err := InitTaskWorkerForProfile(t.Context(), nil, profile)
		if err != nil {
			t.Fatal(err)
		}
		present := worker.HasTarget(name)
		worker.Close()
		if present != (profile == WorkloadProfileProduction) {
			t.Fatalf("multiplier refresh registration profile=%q present=%t", profile, present)
		}
	}
}

// Startup seeds one refresh after its first-run delay; a later startup leaves
// a queued refresh where it is, so a deploy never pulls the weekly refresh
// forward.
func TestRefreshCountryReliabilityMultipliersStartupSeedsTheChainOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		controller.SetStConfig(&controller.StConfig{Enabled: false})
		defer controller.SetStConfig(nil)
		controller.SetVerifySettings(model.DefaultVerifySettings())
		defer controller.SetVerifySettings(nil)
		name := task.NewTaskTarget(work.RefreshCountryReliabilityMultipliers).TargetFunctionName()
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
		firstRunDelay := model.DefaultCountryReliabilityMultiplierSettings().FirstRunDelay
		if len(seeded) != 1 || seeded[0].RunAt.Before(before.Add(firstRunDelay)) || server.NowUtc().Add(firstRunDelay).Before(seeded[0].RunAt) {
			t.Fatalf("startup seeded %d refresh tasks: %+v", len(seeded), seeded)
		}

		// the predecessor queued the next week's refresh
		nextWeek := server.NowUtc().Add(6 * 24 * time.Hour).Truncate(time.Second)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET run_at = $2 WHERE task_id = $1`, seeded[0].TaskId, nextWeek))
		})
		if err := initTaskScheduleForProfile(ctx, WorkloadProfileProduction); err != nil {
			t.Fatal(err)
		}
		again := chain()
		if len(again) != 1 || again[0].TaskId != seeded[0].TaskId || !again[0].RunAt.Equal(nextWeek) {
			t.Fatalf("a later startup moved the queued refresh: %+v", again)
		}
	})
}
