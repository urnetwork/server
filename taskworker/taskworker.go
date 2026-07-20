package taskworker

import (
	"context"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/stats"
	"github.com/urnetwork/server/task"
	"github.com/urnetwork/server/taskworker/work"
)

// InitTasks schedules the recurring tasks. It is invoked at startup by the
// taskworkercli command (and by the `init-tasks` subcommand).
func InitTasks(ctx context.Context) {
	server.Tx(ctx, func(tx server.PgTx) {
		clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer clientSession.Cancel()

		// **important** make sure the required functions are loaded in `InitTaskWorker`
		// work.ScheduleWarmEmail(clientSession, tx)
		work.ScheduleExportStats(clientSession, tx)
		work.ScheduleExportProvidersMap(clientSession, tx)
		work.ScheduleRemoveExpiredAuthCodes(clientSession, tx)
		work.SchedulePayout(clientSession, tx)
		work.ScheduleProcessPendingPayouts(clientSession, tx)
		work.ScheduleCancelHungAccountPayments(clientSession, tx)
		// work.SchedulePopulateAccountWallets(clientSession, tx)
		for i := range work.DefaultCloseExpiredContractsBlockSize {
			work.ScheduleCloseExpiredContracts(clientSession, tx, i, false)
		}
		work.ScheduleCloseExpiredNetworkClientHandlers(clientSession, tx)
		work.ScheduleRemoveDisconnectedNetworkClients(clientSession, tx)
		work.ScheduleSweepOrphanNetworkClientData(clientSession, tx)
		work.ScheduleSweepOrphanContractData(clientSession, tx)
		task.ScheduleTaskCleanup(clientSession, tx)
		work.ScheduleBackfillInitialTransferBalance(clientSession, tx)
		work.ScheduleIndexSearchLocations(clientSession, tx)
		// the three data grants run on three different schedules:
		// free daily, pro monthly, referral every referral period
		controller.ScheduleRefreshFreeTransferBalances(clientSession, tx)
		controller.ScheduleRefreshProTransferBalances(clientSession, tx)
		controller.ScheduleRefreshReferralTransferBalances(clientSession, tx)
		work.ScheduleSetMissingConnectionLocations(clientSession, tx)
		work.ScheduleRemoveLocationLookupResults(clientSession, tx)
		work.ScheduleRemoveCompletedContracts(clientSession, tx)
		work.ScheduleReconcileNetEscrow(clientSession, tx)
		work.ScheduleDbMaintenance(clientSession, tx, 0)
		work.ScheduleWarmNetworkGetProviderLocations(clientSession, tx)
		work.ScheduleRemoveExpiredAuthAttempts(clientSession, tx)
		work.ScheduleRemoveExpiredWalletAuthChallenges(clientSession, tx)
		work.ScheduleRemoveExpiredWalletNonces(clientSession, tx)
		work.ScheduleRemoveOldAuditNetworkEvents(clientSession, tx)
		work.ScheduleRemoveOldAuditEvents(clientSession, tx)
		work.ScheduleRemoveOldClientReliabilityStats(clientSession, tx)
		work.ScheduleRollupClientReliabilityStats(clientSession, tx)
		work.ScheduleUpdateClientReliabilityScores(clientSession, tx)
		work.ScheduleRemoveOldProvideKeyChanges(clientSession, tx)
		work.ScheduleUpdateNetworkReliabilityWindow(clientSession, tx)
		work.ScheduleRemoveOldClientLocationReliabilities(clientSession, tx)
		work.ScheduleUpdateClientScores(clientSession, tx)
		work.ScheduleRemoveOldNetworkReliabilityWindow(clientSession, tx)
		work.ScheduleSyncInitialProductUpdates(clientSession, tx)
		work.ScheduleUpdateClientLocations(clientSession, tx)
		work.ScheduleUpdateReliabilities(clientSession, tx, server.NowUtc().Add(-1*time.Hour))
		work.ScheduleCleanupExpiredPaymentIntents(clientSession, tx)
		work.ScheduleSweepVerifyTrails(clientSession, tx)
		work.ScheduleRollupVerifyProviderStats(clientSession, tx)
		work.ScheduleRemoveOldVerifyProviderStats(clientSession, tx)
		work.ScheduleRollupSearchProviderStats(clientSession, tx)
		work.ScheduleRemoveOldSearchProviderStats(clientSession, tx)
		work.ScheduleRefreshVerifyProxyEgress(clientSession, tx)
		work.ScheduleStSyncChain(clientSession, tx)
	})

	// apply per-stream stats retention (MinIO ILM, or the local reaper) once at
	// init, from the central defaults in the stats package
	stats.ApplyStreamRetention(ctx)
}

// InitTaskWorker preserves the historical exact Go signature, including
// assignability to func(context.Context) *task.TaskWorker. New callers that
// need non-default settings use InitTaskWorkerWithSettings.
func InitTaskWorker(ctx context.Context) *task.TaskWorker {
	return InitTaskWorkerWithSettings(ctx, nil)
}

// InitTaskWorkerWithSettings creates a TaskWorker with all task targets
// registered. One TaskWorker can be shared with many goroutines calling
// EvalTasks. Passing nil uses the defaults.
func InitTaskWorkerWithSettings(ctx context.Context, settings *task.TaskWorkerSettings) *task.TaskWorker {
	if settings == nil {
		settings = task.DefaultTaskWorkerSettings()
	}
	taskWorker := task.NewTaskWorker(ctx, settings)

	// 2024.11.15 migration from "bringyour.com" to new package

	taskWorker.AddTargets(
		task.NewTaskTargetWithPost(
			task.TaskCleanup,
			task.TaskCleanupPost,
			"main.TaskCleanup",
		),
		// task.NewTaskTargetWithPost(work.WarmEmail, work.WarmEmailPost),
		task.NewTaskTargetWithPost(
			work.ExportStats,
			work.ExportStatsPost,
			"bringyour.com/service/taskworker/work.ExportStats",
		),
		task.NewTaskTargetWithPost(
			work.ExportProvidersMap,
			work.ExportProvidersMapPost,
		),
		task.NewTaskTargetWithPost(
			work.RemoveExpiredAuthCodes,
			work.RemoveExpiredAuthCodesPost,
			"bringyour.com/service/taskworker/work.RemoveExpiredAuthCodes",
		),
		task.NewTaskTargetWithPost(
			work.Payout,
			work.PayoutPost,
			"bringyour.com/service/taskworker/work.Payout",
		),
		task.NewTaskTargetWithPost(
			work.ProcessPendingPayouts,
			work.ProcessPendingPayoutsPost,
			"bringyour.com/service/taskworker/work.ProcessPendingPayouts",
		),
		task.NewTaskTargetWithPost(
			work.CancelHungAccountPayments,
			work.CancelHungAccountPaymentsPost,
		),
		task.NewTaskTargetWithPost(
			controller.PlaySubscriptionRenewal,
			controller.PlaySubscriptionRenewalPost,
			"bringyour.com/bringyour/controller.PlaySubscriptionRenewal",
			"github.com/urnetwork/server/bringyour/controller.PlaySubscriptionRenewal",
		),
		task.NewTaskTargetWithPost(
			work.BackfillInitialTransferBalance,
			work.BackfillInitialTransferBalancePost,
			"bringyour.com/bringyour/controller.BackfillInitialTransferBalance",
		),
		task.NewTaskTargetWithPost(
			controller.PopulateAccountWallets,
			work.PopulateAccountWalletsPost,
			"bringyour.com/bringyour/controller.PopulateAccountWallets",
		),
		task.NewTaskTargetWithPost(
			work.CloseExpiredContracts,
			work.CloseExpiredContractsPost,
			"bringyour.com/service/taskworker/work.CloseExpiredContracts",
		),
		task.NewTaskTargetWithPost(
			work.CloseExpiredNetworkClientHandlers,
			work.CloseExpiredNetworkClientHandlersPost,
			"bringyour.com/service/taskworker/work.CloseExpiredNetworkClientHandlers",
		),
		task.NewTaskTargetWithPost(
			work.RemoveDisconnectedNetworkClients,
			work.RemoveDisconnectedNetworkClientsPost,
			"bringyour.com/service/taskworker/work.DeleteDisconnectedNetworkClients",
			"github.com/urnetwork/server/taskworker/work.DeleteDisconnectedNetworkClients",
		),
		task.NewTaskTargetWithPost(
			work.SweepOrphanNetworkClientData,
			work.SweepOrphanNetworkClientDataPost,
		),
		task.NewTaskTargetWithPost(
			work.SweepOrphanContractData,
			work.SweepOrphanContractDataPost,
		),
		task.NewTaskTargetWithPost(
			work.IndexSearchLocations,
			work.IndexSearchLocationsPost,
			"github.com/urnetwork/server/model.IndexSearchLocations",
		),
		task.NewTaskTargetWithPost(
			controller.RefreshFreeTransferBalances,
			controller.RefreshFreeTransferBalancesPost,
			"bringyour.com/bringyour/controller.RefreshFreeTransferBalances",
		),
		task.NewTaskTargetWithPost(
			controller.RefreshProTransferBalances,
			controller.RefreshProTransferBalancesPost,
			"bringyour.com/bringyour/controller.RefreshProTransferBalances",
		),
		task.NewTaskTargetWithPost(
			controller.RefreshReferralTransferBalances,
			controller.RefreshReferralTransferBalancesPost,
			"bringyour.com/bringyour/controller.RefreshReferralTransferBalances",
		),
		task.NewTaskTargetWithPost(
			controller.AdvancePayment,
			controller.AdvancePaymentPost,
			"bringyour.com/bringyour/controller.AdvancePayment",
		),
		task.NewTaskTargetWithPost(
			work.SetMissingConnectionLocations,
			work.SetMissingConnectionLocationsPost,
		),
		task.NewTaskTargetWithPost(
			work.RemoveLocationLookupResults,
			work.RemoveLocationLookupResultsPost,
		),
		task.NewTaskTargetWithPost(
			work.RemoveCompletedContracts,
			work.RemoveCompletedContractsPost,
		),
		task.NewTaskTargetWithPost(
			work.ReconcileNetEscrow,
			work.ReconcileNetEscrowPost,
		),
		task.NewTaskTargetWithPost(
			work.DbMaintenance,
			work.DbMaintenancePost,
		),
		task.NewTaskTargetWithPost(
			work.WarmNetworkGetProviderLocations,
			work.WarmNetworkGetProviderLocationsPost,
		),
		task.NewTaskTargetWithPost(
			work.RemoveExpiredAuthAttempts,
			work.RemoveExpiredAuthAttemptsPost,
		),
		task.NewTaskTargetWithPost(
			work.RemoveExpiredWalletAuthChallenges,
			work.RemoveExpiredWalletAuthChallengesPost,
			"github.com/urnetwork/server/taskworker/work.RemoveExpiredWalletAuthChallenges",
		),
		task.NewTaskTargetWithPost(
			work.RemoveExpiredWalletNonces,
			work.RemoveExpiredWalletNoncesPost,
			"github.com/urnetwork/server/taskworker/work.RemoveExpiredWalletNonces",
		),
		task.NewTaskTargetWithPost(
			work.RemoveOldAuditNetworkEvents,
			work.RemoveOldAuditNetworkEventsPost,
		),
		task.NewTaskTargetWithPost(
			work.RemoveOldAuditEvents,
			work.RemoveOldAuditEventsPost,
		),
		task.NewTaskTargetWithPost(
			work.RemoveOldClientReliabilityStats,
			work.RemoveOldClientReliabilityStatsPost,
		),
		task.NewTaskTargetWithPost(
			work.RollupClientReliabilityStats,
			work.RollupClientReliabilityStatsPost,
		),
		task.NewTaskTargetWithPost(
			work.UpdateClientReliabilityScores,
			work.UpdateClientReliabilityScoresPost,
		),
		task.NewTaskTargetWithPost(
			work.RemoveOldProvideKeyChanges,
			work.RemoveOldProvideKeyChangesPost,
		),
		task.NewTaskTargetWithPost(
			work.UpdateNetworkReliabilityWindow,
			work.UpdateNetworkReliabilityWindowPost,
		),
		task.NewTaskTargetWithPost(
			work.RemoveOldClientLocationReliabilities,
			work.RemoveOldClientLocationReliabilitiesPost,
		),
		task.NewTaskTargetWithPost(
			work.UpdateClientScores,
			work.UpdateClientScoresPost,
		),
		task.NewTaskTargetWithPost(
			work.RemoveOldNetworkReliabilityWindow,
			work.RemoveOldNetworkReliabilityWindowPost,
		),
		task.NewTaskTargetWithPost(
			work.SyncInitialProductUpdates,
			work.SyncInitialProductUpdatesPost,
		),
		task.NewTaskTargetWithPost(
			controller.SyncProductUpdatesForUser,
			controller.SyncProductUpdatesForUserPost,
		),
		task.NewTaskTargetWithPost(
			controller.RemoveProductUpdates,
			controller.RemoveProductUpdatesPost,
		),
		task.NewTaskTargetWithPost(
			work.UpdateClientLocations,
			work.UpdateClientLocationsPost,
		),
		task.NewTaskTargetWithPost(
			work.UpdateReliabilities,
			work.UpdateReliabilitiesPost,
		),
		task.NewTaskTargetWithPost(
			work.CleanupExpiredPaymentIntents,
			work.CleanupExpiredPaymentIntentsPost,
		),
		task.NewTaskTargetWithPost(
			work.SweepVerifyTrails,
			work.SweepVerifyTrailsPost,
		),
		task.NewTaskTargetWithPost(
			work.RollupVerifyProviderStats,
			work.RollupVerifyProviderStatsPost,
		),
		task.NewTaskTargetWithPost(
			work.RollupSearchProviderStats,
			work.RollupSearchProviderStatsPost,
		),
		task.NewTaskTargetWithPost(
			work.RemoveOldSearchProviderStats,
			work.RemoveOldSearchProviderStatsPost,
		),
		task.NewTaskTargetWithPost(
			work.RemoveOldVerifyProviderStats,
			work.RemoveOldVerifyProviderStatsPost,
		),
		task.NewTaskTargetWithPost(
			work.RefreshVerifyProxyEgress,
			work.RefreshVerifyProxyEgressPost,
		),
		task.NewTaskTargetWithPost(
			work.StSyncChain,
			work.StSyncChainPost,
		),
		task.NewTaskTargetWithPost(
			work.StEpochClose,
			work.StEpochClosePost,
		),
		task.NewTaskTargetWithPost(
			work.StCommitRoot,
			work.StCommitRootPost,
		),
		task.NewTaskTargetWithPost(
			work.StDeposit,
			work.StDepositPost,
		),
		task.NewTaskTargetWithPost(
			work.StFinalizePoke,
			work.StFinalizePokePost,
		),
	)

	return taskWorker
}
