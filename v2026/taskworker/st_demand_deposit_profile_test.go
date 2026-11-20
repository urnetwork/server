package taskworker

import (
	"testing"

	"github.com/urnetwork/server/v2026/task"
	"github.com/urnetwork/server/v2026/taskworker/work"
)

// Pure: both workload profiles run the usage statement's steps and the late price
// check, since a sync turn on either can start them, and the deposit task keeps the
// entry point persisted tasks name.
func TestStDemandDepositTasksRegisteredInBothProfiles(t *testing.T) {
	names := []string{
		task.NewTaskTarget(work.StDeposit).TargetFunctionName(),
		task.NewTaskTarget(work.StLedgerV2UsageStep).TargetFunctionName(),
		task.NewTaskTarget(work.StDepositLateEntry).TargetFunctionName(),
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
}
