package taskworker

import (
	"testing"

	"github.com/urnetwork/server/task"
	"github.com/urnetwork/server/taskworker/work"
)

// Pure: both workload profiles register the close evidence row retention, its
// own chain since 2026-11-23, beside the contract partition task that ran it
// before, and the subnet operator schedules it, as it schedules that task.
func TestCloseEvidenceRetentionRegisteredInBothProfiles(t *testing.T) {
	name := task.NewTaskTarget(work.RemoveExpiredCloseEvidence).TargetFunctionName()
	contracts := task.NewTaskTarget(work.MaintainContractPartitions).TargetFunctionName()
	for _, profile := range []WorkloadProfile{WorkloadProfileProduction, WorkloadProfileSubnetOperator} {
		worker, err := InitTaskWorkerForProfile(t.Context(), nil, profile)
		if err != nil {
			t.Fatal(err)
		}
		if !worker.HasTarget(name) || !worker.HasTarget(contracts) {
			t.Errorf("profile %q does not register %s and %s", profile, name, contracts)
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
