// Production placement is an owner-level opt-in, not a task-package default
// or a shared mutable budget between callers.
package taskworker

import (
	"reflect"
	"testing"

	"github.com/urnetwork/server/v2026/task"
	"github.com/urnetwork/server/v2026/taskworker/work"
)

// Only the probe owns the newly introduced production limit. Other settings
// and subnet-operator behavior retain their original values.
func TestProviderProbeClaimAdmissionIsProductionOnly(t *testing.T) {
	settings := task.DefaultTaskWorkerSettings()
	production := taskWorkerSettingsForProfile(settings, WorkloadProfileProduction)
	name := task.NewTaskTarget(work.ProviderEgressProbe).TargetFunctionName()
	if !reflect.DeepEqual(production.TargetClaimLimits, map[string]int{name: 1}) || settings.TargetClaimLimits != nil {
		t.Fatal("production did not opt in only the provider probe, or mutated its caller")
	}
	production.TargetClaimLimits = nil
	if !reflect.DeepEqual(production, settings) {
		t.Fatal("probe placement changed ordinary worker settings")
	}
	operator := taskWorkerSettingsForProfile(settings, WorkloadProfileSubnetOperator)
	if operator.TargetClaimLimits != nil || !operator.ClaimRegisteredTargetsOnly {
		t.Fatal("probe placement changed the independent workload profile")
	}
}

// Caller-supplied opt-ins survive without sharing their mutable map. A caller
// cannot accidentally override the production probe placement invariant.
func TestProviderProbeClaimAdmissionDoesNotBorrowCallerMap(t *testing.T) {
	settings := task.DefaultTaskWorkerSettings()
	name := task.NewTaskTarget(work.ProviderEgressProbe).TargetFunctionName()
	settings.TargetClaimLimits = map[string]int{name: 7, "fixture.example/tasks.Other": 2}
	owner := taskWorkerSettingsForProfile(settings, WorkloadProfileProduction)
	if owner.TargetClaimLimits[name] != 1 || owner.TargetClaimLimits["fixture.example/tasks.Other"] != 2 || settings.TargetClaimLimits[name] != 7 {
		t.Fatal("production opt-in lost caller policy or mutated the caller")
	}
	settings.TargetClaimLimits["fixture.example/tasks.Other"] = 9
	if owner.TargetClaimLimits["fixture.example/tasks.Other"] != 2 {
		t.Fatal("worker placement borrowed a mutable caller map")
	}
}
