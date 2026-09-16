package taskworker

// Workload profiles constrain a worker's scheduling and claims without changing
// task implementations or deleting rows owned by another workload.

import (
	"context"
	"fmt"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/stats"
	"github.com/urnetwork/server/task"
	"github.com/urnetwork/server/taskworker/work"
)

// WorkloadProfile selects an explicit task set. The zero value retains the
// complete production scheduler, registry, and unknown-target retry behavior.
type WorkloadProfile string

const (
	WorkloadProfileProduction WorkloadProfile = ""
	WorkloadProfileSubnetOperator WorkloadProfile = "subnet-operator"
)

// Validate rejects misspelled profiles before startup can access the environment.
func (self WorkloadProfile) Validate() error {
	switch self {
	case WorkloadProfileProduction, WorkloadProfileSubnetOperator:
		return nil
	default:
		return fmt.Errorf("unknown taskworker workload profile %q", self)
	}
}

// InitTasksForProfile arms only the chosen workload. A subnet operator leaves
// retained retail, external probing, and removed-target rows untouched; its
// claim filter also covers jobs independently enqueued by API/controller calls.
func InitTasksForProfile(ctx context.Context, profile WorkloadProfile) error {
	if err := profile.Validate(); err != nil {
		return err
	}
	if profile == WorkloadProfileProduction {
		InitTasks(ctx)
		return nil
	}
	server.Tx(ctx, func(tx server.PgTx) {
		clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer clientSession.Cancel()
		for _, definition := range subnetOperatorTasks() {
			if definition.schedule != nil {
				definition.schedule(clientSession, tx)
			}
		}
	})
	stats.ApplyStreamRetention(ctx)
	return nil
}

// InitTaskWorkerForProfile preserves the production dispatch and finalization
// machinery while selecting the registry and an opt-in queue claim boundary.
func InitTaskWorkerForProfile(ctx context.Context, settings *task.TaskWorkerSettings, profile WorkloadProfile) (*task.TaskWorker, error) {
	if err := profile.Validate(); err != nil {
		return nil, err
	}
	return initTaskWorkerWithSettings(ctx, settings, profile), nil
}

// addProfileTargets preserves canonical names, post hooks and legacy aliases
// from the production registry for every admitted target.
func addProfileTargets(worker *task.TaskWorker, profile WorkloadProfile, targets ...task.Target) {
	if profile == WorkloadProfileProduction {
		worker.AddTargets(targets...)
		return
	}
	allowedFunctionNames := map[string]bool{}
	for _, definition := range subnetOperatorTasks() {
		allowedFunctionNames[definition.target.TargetFunctionName()] = true
	}
	for _, target := range targets {
		if allowedFunctionNames[target.TargetFunctionName()] {
			worker.AddTargets(target)
		}
	}
}

// subnetOperatorTask couples startup scheduling to claim/dispatch admission.
// A nil schedule denotes API-enqueued or parent-enqueued work, not a disabled
// task. Post retries are additionally scoped to this registry by task's claim.
type subnetOperatorTask struct {
	target task.Target
	schedule func(*session.ClientSession, server.PgTx)
}

// subnetOperatorTasks covers the operator's transfer, provider, verification,
// settlement and evidence lifecycle. Retail billing/subscriptions/onboarding,
// public geolocation/prober/extender discovery, and web analytics are separate
// workloads. Existing simulator fixtures provide provider locations and proxy
// identities; verification's proxy-egress refresh remains active below.
func subnetOperatorTasks() []subnetOperatorTask {
	return []subnetOperatorTask{
		{target: task.NewTaskTarget(work.ExportStats), schedule: work.ScheduleExportStats},
		{target: task.NewTaskTarget(work.ExportProvidersMap), schedule: work.ScheduleExportProvidersMap},
		{target: task.NewTaskTarget(work.BackfillClock), schedule: func(clientSession *session.ClientSession, tx server.PgTx) {
			work.ScheduleBackfillClock(clientSession, tx, server.NowUtc())
		}},
		{target: task.NewTaskTarget(work.RemoveExpiredAuthCodes), schedule: work.ScheduleRemoveExpiredAuthCodes},
		{target: task.NewTaskTarget(work.CloseExpiredContracts), schedule: func(clientSession *session.ClientSession, tx server.PgTx) {
			for i := range work.DefaultCloseExpiredContractsBlockSize {
				work.ScheduleCloseExpiredContracts(clientSession, tx, i, false)
			}
		}},
		{target: task.NewTaskTarget(work.CloseExpiredNetworkClientHandlers), schedule: work.ScheduleCloseExpiredNetworkClientHandlers},
		{target: task.NewTaskTarget(work.RemoveDisconnectedNetworkClients), schedule: work.ScheduleRemoveDisconnectedNetworkClients},
		{target: task.NewTaskTarget(work.SweepOrphanNetworkClientData), schedule: work.ScheduleSweepOrphanNetworkClientData},
		{target: task.NewTaskTarget(model.RemoveNetworkClientsTask)},
		{target: task.NewTaskTarget(work.SweepOrphanContractData), schedule: work.ScheduleSweepOrphanContractData},
		{target: task.NewTaskTarget(task.TaskCleanup), schedule: task.ScheduleTaskCleanup},
		{target: task.NewTaskTarget(work.BackfillInitialTransferBalance), schedule: work.ScheduleBackfillInitialTransferBalance},
		{target: task.NewTaskTarget(work.IndexSearchLocations), schedule: work.ScheduleIndexSearchLocations},
		{target: task.NewTaskTarget(controller.RefreshFreeTransferBalances), schedule: controller.ScheduleRefreshFreeTransferBalances},
		{target: task.NewTaskTarget(controller.RebuildPointsLeaderboard), schedule: controller.ScheduleRebuildPointsLeaderboard},
		{target: task.NewTaskTarget(work.RemoveCompletedContracts), schedule: work.ScheduleRemoveCompletedContracts},
		{target: task.NewTaskTarget(work.ReconcileNetEscrow), schedule: work.ScheduleReconcileNetEscrow},
		{target: task.NewTaskTarget(work.DbMaintenance), schedule: func(clientSession *session.ClientSession, tx server.PgTx) {
			work.ScheduleDbMaintenance(clientSession, tx, 0)
		}},
		{target: task.NewTaskTarget(work.WarmNetworkGetProviderLocations), schedule: work.ScheduleWarmNetworkGetProviderLocations},
		{target: task.NewTaskTarget(work.RemoveExpiredAuthAttempts), schedule: work.ScheduleRemoveExpiredAuthAttempts},
		{target: task.NewTaskTarget(work.RemoveExpiredWalletAuthChallenges), schedule: work.ScheduleRemoveExpiredWalletAuthChallenges},
		{target: task.NewTaskTarget(work.RemoveExpiredWalletNonces), schedule: work.ScheduleRemoveExpiredWalletNonces},
		{target: task.NewTaskTarget(work.RemoveExpiredProviderEgressLocations), schedule: work.ScheduleRemoveExpiredProviderEgressLocations},
		{target: task.NewTaskTarget(work.RemoveExpiredBulkClientRemovalQuota), schedule: work.ScheduleRemoveExpiredBulkClientRemovalQuota},
		{target: task.NewTaskTarget(work.RemoveOldAuditNetworkEvents), schedule: work.ScheduleRemoveOldAuditNetworkEvents},
		{target: task.NewTaskTarget(work.RemoveOldAuditEvents), schedule: work.ScheduleRemoveOldAuditEvents},
		{target: task.NewTaskTarget(work.SweepProviderAuditEvents), schedule: work.ScheduleSweepProviderAuditEvents},
		{target: task.NewTaskTarget(work.RollupTransferAuditEvents), schedule: work.ScheduleRollupTransferAuditEvents},
		{target: task.NewTaskTarget(work.RemoveOldClientReliabilityStats), schedule: work.ScheduleRemoveOldClientReliabilityStats},
		{target: task.NewTaskTarget(work.RollupClientReliabilityStats), schedule: work.ScheduleRollupClientReliabilityStats},
		{target: task.NewTaskTarget(work.UpdateClientReliabilityScores), schedule: work.ScheduleUpdateClientReliabilityScores},
		{target: task.NewTaskTarget(work.RemoveOldProvideKeyChanges), schedule: work.ScheduleRemoveOldProvideKeyChanges},
		{target: task.NewTaskTarget(work.UpdateNetworkReliabilityWindow), schedule: work.ScheduleUpdateNetworkReliabilityWindow},
		{target: task.NewTaskTarget(work.RemoveOldClientLocationReliabilities), schedule: work.ScheduleRemoveOldClientLocationReliabilities},
		{target: task.NewTaskTarget(work.UpdateClientScores), schedule: work.ScheduleUpdateClientScores},
		{target: task.NewTaskTarget(work.RemoveOldNetworkReliabilityWindow), schedule: work.ScheduleRemoveOldNetworkReliabilityWindow},
		{target: task.NewTaskTarget(work.UpdateClientLocations), schedule: work.ScheduleUpdateClientLocations},
		{target: task.NewTaskTarget(work.UpdateReliabilities), schedule: func(clientSession *session.ClientSession, tx server.PgTx) {
			work.ScheduleUpdateReliabilities(clientSession, tx, server.NowUtc().Add(-1*time.Hour))
		}},
		{target: task.NewTaskTarget(work.SweepVerifyTrails), schedule: work.ScheduleSweepVerifyTrails},
		{target: task.NewTaskTarget(work.RollupVerifyProviderStats), schedule: work.ScheduleRollupVerifyProviderStats},
		{target: task.NewTaskTarget(work.RemoveOldVerifyProviderStats), schedule: work.ScheduleRemoveOldVerifyProviderStats},
		{target: task.NewTaskTarget(work.RefreshVerifyProxyEgress), schedule: work.ScheduleRefreshVerifyProxyEgress},
		{target: task.NewTaskTarget(work.RollupSearchProviderStats), schedule: work.ScheduleRollupSearchProviderStats},
		{target: task.NewTaskTarget(work.RemoveOldSearchProviderStats), schedule: work.ScheduleRemoveOldSearchProviderStats},
		{target: task.NewTaskTarget(work.StSyncChain), schedule: work.ScheduleStSyncChain},
		{target: task.NewTaskTarget(work.StEpochClose)},
		{target: task.NewTaskTarget(work.StCommitRoot)},
		{target: task.NewTaskTarget(work.StDeposit)},
		{target: task.NewTaskTarget(work.StFinalizePoke)},
	}
}
