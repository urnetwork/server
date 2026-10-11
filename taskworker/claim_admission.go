// Placement policy is explicit at the production worker owner. The generic
// task scheduler remains unlimited unless its constructor is given a limit.
package taskworker

import (
	"maps"

	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/task"
	"github.com/urnetwork/server/taskworker/work"
)

// Legacy payer settlement turns per instance. On Main a running turn keeps
// about one PostgreSQL backend busy, mostly in financial transactions, and the
// fleet averaged 3.8 concurrent turns; two per instance caps payer work at
// about ten backends across five instances.
const legacyPayerSettlementClaimLimit = 2

// One provider-probe shard per instance prevents several polling loops from
// co-locating long-lived tunnel pools. One legacy mirror repair bounds cold
// history work moved off the financial page. Legacy payer turns are bounded
// because lanes no longer starve them. Each worker has independent capacity,
// and caller-owned settings and maps remain unchanged. A saturated limit makes
// every claim filter that target's due rows out of the oldest-first candidate
// scan, so only targets that never dominate the queue are limited. A payer has
// one queued turn, so its filter reads at most one row per due payer.
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
	ownerSettings.TargetClaimLimits[model.NewLegacyPayerSettlementTaskTarget().TargetFunctionName()] = legacyPayerSettlementClaimLimit
	if profile == WorkloadProfileProduction {
		name := task.NewTaskTarget(work.ProviderEgressProbe).TargetFunctionName()
		ownerSettings.TargetClaimLimits[name] = 1
	} else if profile == WorkloadProfileSubnetOperator {
		ownerSettings.ClaimRegisteredTargetsOnly = true
	}
	return &ownerSettings
}
