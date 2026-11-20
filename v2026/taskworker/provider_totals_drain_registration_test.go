package taskworker

import (
	"testing"

	"github.com/urnetwork/server/v2026/model"
)

// Both workload profiles register the provider totals drain beside the
// per-task reader; work.ScheduleSettlementAccountingTasks arms it in both.
func TestProviderTotalsDrainRegisteredInBothProfiles(t *testing.T) {
	names := []string{
		model.NewLegacyProviderTotalsDrainTaskTarget().TargetFunctionName(),
		model.NewLegacyProviderTotalsTaskTarget().TargetFunctionName(),
	}
	for _, profile := range []WorkloadProfile{WorkloadProfileProduction, WorkloadProfileSubnetOperator} {
		worker, err := InitTaskWorkerForProfile(t.Context(), nil, profile)
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range names {
			if !worker.HasTarget(name) {
				worker.Close()
				t.Fatalf("profile %q does not register %s", profile, name)
			}
		}
		worker.Close()
	}
}
