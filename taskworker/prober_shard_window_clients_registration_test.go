package taskworker

import (
	"testing"

	"github.com/urnetwork/server/task"
	"github.com/urnetwork/server/taskworker/work"
)

// Pure: production registers the prober retention's window step beside its
// generation drop, and the subnet operator, which runs no prober, schedules
// neither.
func TestProberShardWindowStepRegisteredInProduction(t *testing.T) {
	windows := task.NewTaskTarget(work.RemoveProberShardWindows).TargetFunctionName()
	generations := task.NewTaskTarget(work.RemoveProberShardGenerations).TargetFunctionName()
	worker, err := InitTaskWorkerForProfile(t.Context(), nil, WorkloadProfileProduction)
	if err != nil {
		t.Fatal(err)
	}
	defer worker.Close()
	for _, name := range []string{windows, generations} {
		if !worker.HasTarget(name) {
			t.Errorf("production does not register %s", name)
		}
	}
	for _, operatorTask := range subnetOperatorTasks() {
		if name := operatorTask.target.TargetFunctionName(); name == windows || name == generations {
			t.Errorf("the subnet operator workload runs %s", name)
		}
	}
	for _, removed := range removedTaskTargets {
		if removed == windows {
			t.Errorf("%s is listed as a removed target", windows)
		}
	}
}
