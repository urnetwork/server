// Placement policy is explicit at the production worker owner. The generic
// task scheduler remains unlimited unless its constructor is given a limit.
package taskworker

import (
	"maps"

	"github.com/urnetwork/server/v2026/task"
	"github.com/urnetwork/server/v2026/taskworker/work"
)

// One provider-probe shard per instance prevents several polling loops from
// co-locating long-lived tunnel pools. No other production target is limited,
// and a separate worker receives independent capacity. Caller settings stay
// unchanged, including their map if they also opted other targets in.
func taskWorkerSettingsForProfile(settings *task.TaskWorkerSettings, profile WorkloadProfile) *task.TaskWorkerSettings {
	if settings == nil {
		settings = task.DefaultTaskWorkerSettings()
	}
	ownerSettings := *settings
	ownerSettings.TargetClaimLimits = maps.Clone(settings.TargetClaimLimits)
	if profile == WorkloadProfileProduction {
		if ownerSettings.TargetClaimLimits == nil {
			ownerSettings.TargetClaimLimits = map[string]int{}
		}
		name := task.NewTaskTarget(work.ProviderEgressProbe).TargetFunctionName()
		ownerSettings.TargetClaimLimits[name] = 1
	} else if profile == WorkloadProfileSubnetOperator {
		ownerSettings.ClaimRegisteredTargetsOnly = true
	}
	return &ownerSettings
}
