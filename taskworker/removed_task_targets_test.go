// Retired task chains stay reaped and never return to the registry.
package taskworker

import (
	"context"
	"slices"
	"testing"
)

// The legacy settlement queue retired with legacy_settlement_intent
// (PARTITION-MAINTENANCE-PLAN.md, Part B). Rows already queued for its 16
// flush_legacy_settlements shard dispatchers, or for the per-payer and
// per-source owner turns they published, are reaped at startup, and neither
// workload registers them again.
func TestRemovedTaskTargetsReapRetiredLegacySettlementQueue(t *testing.T) {
	for _, functionName := range []string{
		"github.com/urnetwork/server/taskworker/work.FlushLegacySettlements",
		"github.com/urnetwork/server/model.ApplyLegacyPayerSettlements",
		"github.com/urnetwork/server/model.ApplyLegacySourceSettlements",
	} {
		if !slices.Contains(removedTaskTargets, functionName) {
			t.Errorf("removedTaskTargets does not reap %s", functionName)
		}
	}
	for _, profile := range []WorkloadProfile{WorkloadProfileProduction, WorkloadProfileSubnetOperator} {
		worker, err := InitTaskWorkerForProfile(context.Background(), nil, profile)
		if err != nil {
			t.Fatal(err)
		}
		for _, functionName := range removedTaskTargets {
			if worker.HasTarget(functionName) {
				t.Errorf("profile %q registers removed target %s", profile, functionName)
			}
		}
		worker.Close()
	}
}
