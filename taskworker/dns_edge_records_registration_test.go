package taskworker

import (
	"testing"

	"github.com/urnetwork/server/task"
	"github.com/urnetwork/server/taskworker/work"
)

// §12.7's hourly pass over the edge grants is the sidecar's, so only the production registry claims it: a subnet
// operator serves no zone.
func TestDnsEdgeRecordsPassIsProductionOnly(t *testing.T) {
	name := task.NewTaskTarget(work.DnsEdgeRecords).TargetFunctionName()
	for _, profile := range []WorkloadProfile{WorkloadProfileProduction, WorkloadProfileSubnetOperator} {
		worker, err := InitTaskWorkerForProfile(t.Context(), nil, profile)
		if err != nil {
			t.Fatal(err)
		}
		if present := worker.HasTarget(name); present != (profile == WorkloadProfileProduction) {
			t.Errorf("%s registration profile=%q present=%t", name, profile, present)
		}
		worker.Close()
	}
	for _, operatorTask := range subnetOperatorTasks() {
		if operatorTask.target.TargetFunctionName() == name {
			t.Errorf("the subnet operator workload schedules %s", name)
		}
	}
}
