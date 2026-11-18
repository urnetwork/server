package taskworker

import (
	"testing"

	"github.com/urnetwork/server/task"
	"github.com/urnetwork/server/taskworker/work"
)

// Pure: the production workload registers the session level sample, whose
// passes urnetwork_session_live_per_network is observed by.
func TestSampleSessionLevelsRegisteredInProduction(t *testing.T) {
	name := task.NewTaskTarget(work.SampleSessionLevels).TargetFunctionName()
	worker, err := InitTaskWorkerForProfile(t.Context(), nil, WorkloadProfileProduction)
	if err != nil {
		t.Fatal(err)
	}
	defer worker.Close()
	if !worker.HasTarget(name) {
		t.Fatalf("the production profile does not register %s", name)
	}
	for _, removed := range removedTaskTargets {
		if removed == name {
			t.Errorf("%s is listed as a removed target", name)
		}
	}
}
