package taskworker

import (
	"context"
	"testing"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/task"
	"github.com/urnetwork/server/v2026/taskworker/work"
)

// The churn tables' vacuum (PARTITION-MAINTENANCE-PLAN.md A1) is registered
// and armed in both workloads, since DbMaintenance no longer vacuums
// pending_task in either. A chain armed with no registered target is reaped as
// a removed target, and a registered target never armed never runs.

// Pure: both workload profiles register the target, and the subnet operator
// schedules it.
func TestVacuumHotTablesRegisteredInBothProfiles(t *testing.T) {
	name := task.NewTaskTarget(work.VacuumHotTables).TargetFunctionName()
	for _, profile := range []WorkloadProfile{WorkloadProfileProduction, WorkloadProfileSubnetOperator} {
		worker, err := InitTaskWorkerForProfile(t.Context(), nil, profile)
		if err != nil {
			t.Fatal(err)
		}
		present := worker.HasTarget(name)
		worker.Close()
		if !present {
			t.Fatalf("profile %q does not register %s", profile, name)
		}
	}

	scheduled := false
	for _, operatorTask := range subnetOperatorTasks() {
		if operatorTask.target.TargetFunctionName() == name {
			scheduled = operatorTask.schedule != nil
		}
	}
	if !scheduled {
		t.Fatal("the subnet operator workload does not schedule the hot table vacuum")
	}
}

// DB-backed: InitTasks arms one run-once row for the chain however often a
// worker starts.
func TestInitTasksArmsTheHotVacuumChain(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		for range 2 {
			InitTasks(ctx)
			var count int
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(
					ctx,
					`SELECT COUNT(*) FROM pending_task WHERE run_once_key = '["vacuum_hot_tables"]'`,
				).Scan(&count))
			})
			connect.AssertEqual(t, count, 1)
		}
	})
}
