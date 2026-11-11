package taskworker

import (
	"testing"

	"github.com/urnetwork/server/task"
	"github.com/urnetwork/server/taskworker/work"
)

// Pure: both workload profiles run capture the flag's samples and census, since
// a sync turn on either can start them.
func TestStCtfStepRegisteredInBothProfiles(t *testing.T) {
	name := task.NewTaskTarget(work.StCtfStep).TargetFunctionName()
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
}
