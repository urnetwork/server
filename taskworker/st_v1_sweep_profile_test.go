package taskworker

import (
	"context"
	"testing"

	"github.com/urnetwork/server/task"
	"github.com/urnetwork/server/taskworker/work"
)

// Both the production and the subnet operator workloads run the phase C v1
// carry sweep chain, which StSyncChain starts while a sweep is configured.
func TestWorkloadsRunTheV1Sweep(t *testing.T) {
	target := task.NewTaskTarget(work.StV1Sweep).TargetFunctionName()
	for _, profile := range []WorkloadProfile{WorkloadProfileProduction, WorkloadProfileSubnetOperator} {
		worker, err := InitTaskWorkerForProfile(context.Background(), task.DefaultTaskWorkerSettings(), profile)
		if err != nil {
			t.Fatal(err)
		}
		if !worker.HasTarget(target) {
			t.Errorf("%s workload lacks %s", profile, target)
		}
		worker.Close()
	}
}
