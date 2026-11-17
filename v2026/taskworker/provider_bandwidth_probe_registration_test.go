// The serving taskworker registers the bandwidth probe; a subnet operator
// leaves it alone, as it does every external probe.
package taskworker

import (
	"testing"

	"github.com/urnetwork/server/v2026/taskworker/work"
)

// Only the production registry holds the bandwidth probe's target.
func TestProviderBandwidthProbeIsProductionOnly(t *testing.T) {
	name := work.NewProviderBandwidthProbeTaskTarget().TargetFunctionName()
	for _, profile := range []WorkloadProfile{WorkloadProfileProduction, WorkloadProfileSubnetOperator} {
		worker, err := InitTaskWorkerForProfile(t.Context(), nil, profile)
		if err != nil {
			t.Fatal(err)
		}
		present := worker.HasTarget(name)
		worker.Close()
		if present != (profile == WorkloadProfileProduction) {
			t.Fatalf("bandwidth probe registration profile=%q present=%t", profile, present)
		}
	}
	for _, definition := range subnetOperatorTasks() {
		if definition.target.TargetFunctionName() == name {
			t.Fatal("the subnet operator schedules the bandwidth probe")
		}
	}
}
