package taskworker

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
	"github.com/urnetwork/server/v2026/task"
	"github.com/urnetwork/server/v2026/taskworker/work"
)

// The per-client data usage rollup (model/network_client_data_cap_model.go)
// turns settled bytes into monthly usage and the cap markers that escrow
// admission enforces. Without a registered, scheduled owner a cap never acts.

// Pure: both workload profiles register the target, since a subnet operator
// settles contracts too.
func TestRollupClientDataUsageRegisteredInBothProfiles(t *testing.T) {
	name := task.NewTaskTarget(work.RollupClientDataUsage).TargetFunctionName()
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
		t.Fatal("the subnet operator workload does not schedule the data usage rollup")
	}
}

// DB-backed: under the owner rule this runs only after the branch, with its
// migrations, is merged to main.
//
// Startup arms one run-once owner that runs within one rollup interval, and
// its post re-arms the same owner instead of adding another.
func TestRollupClientDataUsageScheduleArmsOneOwner(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
		defer cancel()
		controller.SetStConfig(&controller.StConfig{Enabled: false})
		defer controller.SetStConfig(nil)
		controller.SetVerifySettings(model.DefaultVerifySettings())
		defer controller.SetVerifySettings(nil)

		name := task.NewTaskTarget(work.RollupClientDataUsage).TargetFunctionName()
		owners := func() []*task.Task {
			values := []*task.Task{}
			for _, pending := range task.GetTasks(ctx, task.ListPendingTasks(ctx)...) {
				if pending.FunctionName == name {
					values = append(values, pending)
				}
			}
			return values
		}

		start := server.NowUtc()
		for _, profile := range []WorkloadProfile{WorkloadProfileProduction, WorkloadProfileSubnetOperator} {
			if err := initTaskScheduleForProfile(ctx, profile); err != nil {
				t.Fatal(err)
			}
		}
		armed := owners()
		connect.AssertEqual(t, len(armed), 1)
		owner := armed[0]
		connect.AssertEqual(t, owner.RunOnceKey, task.RunOnce("rollup_client_data_usage").String())
		connect.AssertEqual(t, owner.RunMaxTimeSeconds, int((15 * time.Minute).Seconds()))
		if owner.RunAt.After(start.Add(model.ClientDataUsageRollupInterval + 5*time.Second)) {
			t.Fatalf("owner runs at %s, more than one rollup interval after startup", owner.RunAt)
		}

		// the post re-arms the run-once owner
		clientSession := session.Testing_CreateClientSession(ctx, nil)
		server.Tx(ctx, func(tx server.PgTx) {
			err := work.RollupClientDataUsagePost(&work.RollupClientDataUsageArgs{}, &work.RollupClientDataUsageResult{}, clientSession, tx)
			connect.AssertEqual(t, err, nil)
		})
		connect.AssertEqual(t, len(owners()), 1)

		// the work itself drains with no usage recorded and no error
		result, err := work.RollupClientDataUsage(&work.RollupClientDataUsageArgs{}, clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertNotEqual(t, result, (*work.RollupClientDataUsageResult)(nil))
	})
}
