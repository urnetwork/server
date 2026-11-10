package taskworker

import (
	"testing"

	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/task"
	"github.com/urnetwork/server/taskworker/work"
)

// §7's taskworker jobs are the sidecar's, so only the production registry claims them: a subnet
// operator hosts no sidecar, so it mints no ephemeral client and holds no authorization log.
func TestTeamTasksAreProductionOnly(t *testing.T) {
	names := []string{
		task.NewTaskTarget(work.ReapEphemeralClients).TargetFunctionName(),
		task.NewTaskTarget(model.AppendConfigLogTickTask).TargetFunctionName(),
		task.NewTaskTarget(work.ConfigLogRetention).TargetFunctionName(),
		task.NewTaskTarget(work.NetworkDomainRecheck).TargetFunctionName(),
		task.NewTaskTarget(work.NetworkAuditWebhookDelivery).TargetFunctionName(),
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
