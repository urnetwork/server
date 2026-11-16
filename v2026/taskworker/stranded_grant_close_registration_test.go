package taskworker

import (
	"slices"
	"testing"

	"github.com/urnetwork/server/v2026/model"
)

// Both profiles run the stranded-grant pass and schedule it at startup, and the
// name the schedule test keeps is the pass's function.
func TestCloseStrandedGrantContractsRegisteredAndScheduledInBothProfiles(t *testing.T) {
	name := model.NewCloseStrandedGrantContractsTaskTarget().TargetFunctionName()
	if !slices.Contains(payerScopedContractClosers, name) {
		t.Fatal("the schedule test does not keep the stranded-grant pass", name, payerScopedContractClosers)
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
	scheduled := false
	for _, definition := range subnetOperatorTasks() {
		if definition.target.TargetFunctionName() == name && definition.schedule != nil {
			scheduled = true
		}
	}
	if !scheduled {
		t.Fatal("the subnet operator profile does not schedule the stranded-grant pass")
	}
}
