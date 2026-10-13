// Placement policy is explicit at the production worker owner. The generic
// task scheduler remains unlimited unless its constructor is given a limit.
package taskworker

import (
	"maps"

	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/task"
	"github.com/urnetwork/server/taskworker/work"
)

// Provider-probe shard passes per instance follow the configured URL geometry,
// so co-located tunnel pools stay within one instance's URL concurrency while
// every configured shard can run on fewer instances than shards. One legacy
// mirror repair bounds cold history work moved off the financial page. Each
// worker has independent capacity, and caller-owned settings and maps remain
// unchanged. A saturated limit makes every claim filter that target's rows out
// of the oldest-first candidate scan, so only targets that never dominate the
// queue are limited.
func taskWorkerSettingsForProfile(settings *task.TaskWorkerSettings, profile WorkloadProfile) *task.TaskWorkerSettings {
	if settings == nil {
		settings = task.DefaultTaskWorkerSettings()
	}
	ownerSettings := *settings
	// All entry points, including InitTaskWorker/EvalTasks, need the same
	// indexed admission lanes as Run. A close backlog must not hide its
	// downstream financial owners or recurring maintenance.
	ownerSettings.FairClaimFunctions = true
	ownerSettings.TargetClaimLimits = maps.Clone(settings.TargetClaimLimits)
	if ownerSettings.TargetClaimLimits == nil {
		ownerSettings.TargetClaimLimits = map[string]int{}
	}
	ownerSettings.TargetClaimLimits[task.NewTaskTarget(model.ApplyLegacyNetEscrowMirror).TargetFunctionName()] = 1
	if profile == WorkloadProfileProduction {
		name := task.NewTaskTarget(work.ProviderEgressProbe).TargetFunctionName()
		ownerSettings.TargetClaimLimits[name] = work.ProviderEgressProbeInstancePassLimit()
	} else if profile == WorkloadProfileSubnetOperator {
		ownerSettings.ClaimRegisteredTargetsOnly = true
	}
	return &ownerSettings
}
