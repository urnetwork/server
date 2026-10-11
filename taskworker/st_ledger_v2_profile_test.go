package taskworker

import (
	"context"
	"testing"

	"github.com/urnetwork/server/task"
	"github.com/urnetwork/server/taskworker/work"
)

// SV-1 (wiring): both the production and the subnet operator workloads run
// the settlement v2 driver's step task.
func TestWorkloadsRunLedgerV2Steps(t *testing.T) {
	target := task.NewTaskTarget(work.StLedgerV2Step).TargetFunctionName()
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
