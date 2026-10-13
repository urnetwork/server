// Production placement is an owner-level opt-in, not a task-package default
// or a shared mutable budget between callers.
package taskworker

import (
	"reflect"
	"testing"

	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/task"
	"github.com/urnetwork/server/v2026/taskworker/work"
)

// Probe placement is production-only; mirror repair is bounded in both
// profiles that can produce it. Other worker settings retain their values.
func TestProviderProbeClaimAdmissionIsProductionOnly(t *testing.T) {
	settings := task.DefaultTaskWorkerSettings()
	production := taskWorkerSettingsForProfile(settings, WorkloadProfileProduction)
	name := task.NewTaskTarget(work.ProviderEgressProbe).TargetFunctionName()
	mirror := task.NewTaskTarget(model.ApplyLegacyNetEscrowMirror).TargetFunctionName()
	if !reflect.DeepEqual(production.TargetClaimLimits, map[string]int{name: 1, mirror: 1}) || settings.TargetClaimLimits != nil {
		t.Fatal("production target placement differs or mutated its caller")
	}
	production.TargetClaimLimits = nil
	if !production.FairClaimFunctions || settings.FairClaimFunctions {
		t.Fatal("production fairness is missing or mutated caller settings")
	}
	production.FairClaimFunctions = settings.FairClaimFunctions
	if !reflect.DeepEqual(production, settings) {
		t.Fatal("probe placement changed ordinary worker settings")
	}
	operator := taskWorkerSettingsForProfile(settings, WorkloadProfileSubnetOperator)
	if !reflect.DeepEqual(operator.TargetClaimLimits, map[string]int{mirror: 1}) || !operator.ClaimRegisteredTargetsOnly {
		t.Fatal("independent profile did not retain mirror-only placement")
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
