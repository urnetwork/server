// Placement policy is explicit at the production worker owner. The generic
// task scheduler remains unlimited unless its constructor is given a limit.
package taskworker

import (
	"maps"

	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/task"
	"github.com/urnetwork/server/taskworker/work"
)

// Ordinary queue turns per provider totals lane turn: an eighth of all claims
// while totals are due.
const legacyProviderTotalsClaimSharePeriod = 4

// The debit flush and the payment sender each own one run-once chain per shard
// or payment, and behind the totals backlog each waited over a minute per
// claim, one rotation turn in about two hundred. Each now takes one of every
// eight ordinary turns, a sixteenth of all claims, while it is due. The three
// shares spend exactly the half of the ordinary turns the scheduler allows;
// the other half stays oldest first.
const financialOwnerClaimSharePeriod = 8

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
	ownerSettings.TargetClaimShares[task.NewTaskTarget(work.FlushTransferDebits).TargetFunctionName()] = financialOwnerClaimSharePeriod
	ownerSettings.TargetClaimShares[task.NewTaskTarget(controller.AdvancePayment).TargetFunctionName()] = financialOwnerClaimSharePeriod
	if profile == WorkloadProfileProduction {
		name := task.NewTaskTarget(work.ProviderEgressProbe).TargetFunctionName()
		ownerSettings.TargetClaimLimits[name] = work.ProviderEgressProbeInstancePassLimit()
	} else if profile == WorkloadProfileSubnetOperator {
		ownerSettings.ClaimRegisteredTargetsOnly = true
	}
	return &ownerSettings
}
