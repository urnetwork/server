package taskworker

import (
	"testing"

	"github.com/urnetwork/server/v2026/task"
	"github.com/urnetwork/server/v2026/taskworker/work"
)

// Pure: both workload profiles register the unique-exit sweep with its post
// (the post reschedules the next pass), and the subnet operator schedules it,
// since it hosts proxies too.
func TestSweepProxyExitsRegisteredInBothProfiles(t *testing.T) {
	name := task.NewTaskTarget(work.SweepProxyExits).TargetFunctionName()
	for _, profile := range []WorkloadProfile{WorkloadProfileProduction, WorkloadProfileSubnetOperator} {
		worker, err := InitTaskWorkerForProfile(t.Context(), nil, profile)
		if err != nil {
			t.Fatal(err)
		}
		if !worker.HasTarget(name) {
			t.Errorf("profile %q does not register %s", profile, name)
		}
		worker.Close()
	}
	scheduled := false
	for _, operatorTask := range subnetOperatorTasks() {
		if operatorTask.target.TargetFunctionName() == name && operatorTask.schedule != nil {
			scheduled = true
		}
	}
	if !scheduled {
		t.Errorf("the subnet operator workload does not schedule %s", name)
	}
	for _, removed := range removedTaskTargets {
		if removed == name {
			t.Errorf("%s is listed as a removed target", name)
		}
	}
}
