package taskworker

import (
	"testing"

	"github.com/urnetwork/server/task"
	"github.com/urnetwork/server/taskworker/work"
)

// Pure: both workload profiles register the session state retention, and the
// subnet operator schedules it, since its clients connect and are journaled
// too.
func TestSessionStateRetentionRegisteredInBothProfiles(t *testing.T) {
	name := task.NewTaskTarget(work.RemoveExpiredSessionState).TargetFunctionName()
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
		if operatorTask.schedule != nil && operatorTask.target.TargetFunctionName() == name {
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
