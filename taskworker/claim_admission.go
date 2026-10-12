// Placement policy is explicit at the production worker owner. The generic
// task scheduler remains unlimited unless its constructor is given a limit.
package taskworker

import (
	"maps"

	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/task"
	"github.com/urnetwork/server/taskworker/work"
)

// Ordinary queue turns per provider totals lane turn: a quarter of all claims
// while totals are due.
const legacyProviderTotalsClaimSharePeriod = 2

// One provider-probe shard per instance prevents several polling loops from
// co-locating long-lived tunnel pools. One legacy mirror repair bounds cold
// history work moved off the financial page. Each worker has independent
// capacity, and caller-owned settings and maps remain unchanged. A saturated
// limit makes every claim filter that target's rows out of the oldest-first
// candidate scan, so only targets that never dominate the queue are limited.
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
	// Every paid close queues one provider total, and the older close backlog
	// fills every ordinary claim. The totals lane alone, one turn in about 200
	// claims on Main, applied a few rows a second while hundreds a second were
	// queued. It also takes one of every legacyProviderTotalsClaimSharePeriod
	// ordinary turns while totals are due.
	ownerSettings.TargetClaimShares = maps.Clone(settings.TargetClaimShares)
	if ownerSettings.TargetClaimShares == nil {
		ownerSettings.TargetClaimShares = map[string]int{}
	}
	ownerSettings.TargetClaimShares[task.NewTaskTarget(model.ApplyLegacyProviderTotals).TargetFunctionName()] = legacyProviderTotalsClaimSharePeriod
	if profile == WorkloadProfileProduction {
		name := task.NewTaskTarget(work.ProviderEgressProbe).TargetFunctionName()
		ownerSettings.TargetClaimLimits[name] = 1
	} else if profile == WorkloadProfileSubnetOperator {
		ownerSettings.ClaimRegisteredTargetsOnly = true
	}
	return &ownerSettings
}
