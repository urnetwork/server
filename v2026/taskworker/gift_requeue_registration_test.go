package taskworker

import (
	"testing"
	"time"

	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/task"
)

// The production worker runs the gift year queue's hourly reconciliation, and
// it is no removed target.
func TestRebaseQueuedGiftYearsRegisteredInProduction(t *testing.T) {
	worker, err := InitTaskWorkerForProfile(t.Context(), nil, WorkloadProfileProduction)
	if err != nil {
		t.Fatal(err)
	}
	defer worker.Close()
	name := task.NewTaskTargetWithPost(controller.RebaseQueuedGiftYears, controller.RebaseQueuedGiftYearsPost).TargetFunctionName()
	if !worker.HasTarget(name) {
		t.Fatalf("the production worker does not register %s", name)
	}
	for _, removed := range removedTaskTargets {
		if removed == name {
			t.Fatalf("%s is listed as a removed target", name)
		}
	}
	if controller.RebaseQueuedGiftYearsInterval != time.Hour {
		t.Fatalf("the reconciliation runs every %s, want hourly", controller.RebaseQueuedGiftYearsInterval)
	}
}
