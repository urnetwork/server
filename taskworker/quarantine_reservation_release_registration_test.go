package taskworker

import (
	"testing"

	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/task"
)

// A contract quarantine commits a durable Redis release task. Both workload
// profiles quarantine contracts, so both must run it; an unregistered target
// would retry as not found and leave the payer's reservation held.
func TestQuarantineReservationReleaseRegisteredInBothProfiles(t *testing.T) {
	name := model.NewQuarantineReservationReleaseTaskTarget().TargetFunctionName()
	if name != task.NewTaskTarget(model.ReleaseQuarantinedContractReservation).TargetFunctionName() {
		t.Fatal("the release target has another function", name)
	}
	for _, profile := range []WorkloadProfile{WorkloadProfileProduction, WorkloadProfileSubnetOperator} {
		worker, err := InitTaskWorkerForProfile(t.Context(), nil, profile)
		if err != nil {
			t.Fatal(err)
		}
		present := worker.HasTarget(name)
		worker.Close()
		if !present {
			t.Fatalf("profile %q does not register %s", profile, name)
		}
	}
}
