package taskworker

import (
	"testing"

	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/task"
	"github.com/urnetwork/server/taskworker/work"
)

// The spent pass, the service debit flush and the Team grant job are sidecar billing, so only the
// production registry claims them, as the sidecar engines' jobs: a subnet operator hosts no sidecar
// service, so it marks no network spent, journals no service debit and has no Team.
func TestQuotaTasksAreProductionOnly(t *testing.T) {
	names := []string{
		task.NewTaskTarget(work.QuotaSpentPass).TargetFunctionName(),
		task.NewTaskTarget(work.FlushServiceDebits).TargetFunctionName(),
		task.NewTaskTarget(controller.RefreshTeamTransferBalances).TargetFunctionName(),
	}
	for _, profile := range []WorkloadProfile{WorkloadProfileProduction, WorkloadProfileSubnetOperator} {
		worker, err := InitTaskWorkerForProfile(t.Context(), nil, profile)
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range names {
			if present := worker.HasTarget(name); present != (profile == WorkloadProfileProduction) {
				t.Errorf("%s registration profile=%q present=%t", name, profile, present)
			}
		}
		worker.Close()
	}
	for _, operatorTask := range subnetOperatorTasks() {
		for _, name := range names {
			if operatorTask.target.TargetFunctionName() == name {
				t.Errorf("the subnet operator workload schedules %s", name)
			}
		}
	}
}
