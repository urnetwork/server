// Placement policy is explicit at the production worker owner. The generic
// task scheduler remains unlimited unless its constructor is given a limit.
package taskworker

import (
	"maps"

	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/task"
	"github.com/urnetwork/server/v2026/taskworker/work"
)

// Ordinary queue turns per provider totals lane turn: a sixteenth of all claims
// while totals are due. The drain below applies them in bulk; the per-task lane
// takes the rows it leaves.
const legacyProviderTotalsClaimSharePeriod = 8

// The provider totals drain (model/subscription_provider_totals_drain.go) is
// one run-once chain. Behind the totals backlog its next run waited for its
// lane turn, one claim in about two hundred: 85 seconds in
// TestProductionProviderTotalsDrainOutpacesHotProviderProduction. It takes one
// of every eight ordinary turns while that run is due, a sixteenth of all claims.
const legacyProviderTotalsDrainClaimSharePeriod = 8

// The debit flush owns one run-once chain per shard, and behind the totals
// backlog it waited over a minute per claim, one rotation turn in about two
// hundred. It now takes one of every eight ordinary turns, a sixteenth of all
// claims, while it is due. The three shares spend three eighths of the
// ordinary turns, under the half the scheduler allows; the rest stays oldest
// first. The USDC payment sender held the fourth until USDC provider payouts
// ended (owner decisions 2026-11-12 and 2026-11-13).
const financialOwnerClaimSharePeriod = 8

// Provider-probe shard passes per instance follow the configured URL geometry,
// so co-located tunnel pools stay within one instance's URL concurrency while
// every configured shard can run on fewer instances than shards. One legacy
// mirror repair bounds cold history work moved off the financial page, and one
// payer drain bounds the closes payers' requests add. Each worker has
// independent capacity, and caller-owned settings and maps remain unchanged. A
// saturated limit makes every claim filter that target's rows out of the
// oldest-first candidate scan, so only targets that never dominate the queue
// are limited.
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
	// A payer drain writes in owner turns like the deadline pass. One per
	// worker bounds the closes payers' requests add to the primary, whatever
	// the number of payers waiting.
	ownerSettings.TargetClaimLimits[model.NewClosePayerDueContractsTaskTarget().TargetFunctionName()] = 1
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
	ownerSettings.TargetClaimShares[model.NewLegacyProviderTotalsDrainTaskTarget().TargetFunctionName()] = legacyProviderTotalsDrainClaimSharePeriod
	ownerSettings.TargetClaimShares[task.NewTaskTarget(work.FlushTransferDebits).TargetFunctionName()] = financialOwnerClaimSharePeriod
	if profile == WorkloadProfileProduction {
		name := task.NewTaskTarget(work.ProviderEgressProbe).TargetFunctionName()
		ownerSettings.TargetClaimLimits[name] = work.ProviderEgressProbeInstancePassLimit()
	} else if profile == WorkloadProfileSubnetOperator {
		ownerSettings.ClaimRegisteredTargetsOnly = true
	}
	return &ownerSettings
}
