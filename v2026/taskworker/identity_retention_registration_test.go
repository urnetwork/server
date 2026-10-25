package taskworker

import (
	"testing"

	"github.com/urnetwork/server/v2026/task"
	"github.com/urnetwork/server/v2026/taskworker/work"
)

// Pure: both workload profiles register the retired hosted identity reap and
// the key history retention, and the subnet operator schedules both, since it
// mints, retires and reaps clients too.
func TestIdentityRetentionRegisteredInBothProfiles(t *testing.T) {
	names := []string{
		task.NewTaskTarget(work.RemoveRetiredHostedNetworkClients).TargetFunctionName(),
		task.NewTaskTarget(work.RemoveGoneStClientKeyHistory).TargetFunctionName(),
	}
	for _, profile := range []WorkloadProfile{WorkloadProfileProduction, WorkloadProfileSubnetOperator} {
		worker, err := InitTaskWorkerForProfile(t.Context(), nil, profile)
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range names {
			if !worker.HasTarget(name) {
				t.Errorf("profile %q does not register %s", profile, name)
			}
		}
		worker.Close()
	}
	scheduled := map[string]bool{}
	for _, operatorTask := range subnetOperatorTasks() {
		if operatorTask.schedule != nil {
			scheduled[operatorTask.target.TargetFunctionName()] = true
		}
	}
	for _, name := range names {
		if !scheduled[name] {
			t.Errorf("the subnet operator workload does not schedule %s", name)
		}
	}
	for _, name := range removedTaskTargets {
		for _, retained := range names {
			if name == retained {
				t.Errorf("%s is listed as a removed target", name)
			}
		}
	}
}
