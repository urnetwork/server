package taskworker

import (
	"testing"

	"github.com/urnetwork/server/task"
	"github.com/urnetwork/server/taskworker/work"
)

// The spent pass is sidecar billing, so only the production registry claims it, as the sidecar
// engines' jobs: a subnet operator hosts no sidecar service and marks no network spent.
func TestQuotaSpentPassIsProductionOnly(t *testing.T) {
	name := task.NewTaskTarget(work.QuotaSpentPass).TargetFunctionName()
	for _, profile := range []WorkloadProfile{WorkloadProfileProduction, WorkloadProfileSubnetOperator} {
		worker, err := InitTaskWorkerForProfile(t.Context(), nil, profile)
		if err != nil {
			t.Fatal(err)
		}
		present := worker.HasTarget(name)
		worker.Close()
		if present != (profile == WorkloadProfileProduction) {
			t.Fatalf("spent pass registration profile=%q present=%t", profile, present)
		}
	}
	for _, operatorTask := range subnetOperatorTasks() {
		if operatorTask.target.TargetFunctionName() == name {
			t.Fatal("the subnet operator workload schedules the spent pass")
		}
	}
}
