package taskworker

import (
	"testing"

	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/task"
)

// The production worker runs the hourly Pro cycle sweep, and keeps the retired
// calendar-month grant registered so that its pending row is consumed without
// a grant (server/PRO-GRANT-CYCLE.md).
func TestProCycleGrantTasksRegisteredInProduction(t *testing.T) {
	worker, err := InitTaskWorkerForProfile(t.Context(), nil, WorkloadProfileProduction)
	if err != nil {
		t.Fatal(err)
	}
	defer worker.Close()
	for _, name := range []string{
		task.NewTaskTargetWithPost(controller.RefreshProCycleTransferBalances, controller.RefreshProCycleTransferBalancesPost).TargetFunctionName(),
		task.NewTaskTargetWithPost(controller.RefreshProTransferBalances, controller.RefreshProTransferBalancesPost).TargetFunctionName(),
	} {
		if !worker.HasTarget(name) {
			t.Fatalf("the production worker does not register %s", name)
		}
	}
}
