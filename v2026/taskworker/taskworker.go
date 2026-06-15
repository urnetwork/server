package taskworker

import (
	"context"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/session"
	"github.com/urnetwork/server/v2026/task"
	"github.com/urnetwork/server/v2026/taskworker/work"
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
		work.ScheduleRemoveExpiredAuthCodes(clientSession, tx)
		work.SchedulePayout(clientSession, tx)
		work.ScheduleProcessPendingPayouts(clientSession, tx)
		// work.SchedulePopulateAccountWallets(clientSession, tx)
		for i := range work.DefaultCloseExpiredContractsBlockSize {
			work.ScheduleCloseExpiredContracts(clientSession, tx, i, false)
		}
		work.ScheduleCloseExpiredNetworkClientHandlers(clientSession, tx)
		work.ScheduleRemoveDisconnectedNetworkClients(clientSession, tx)
		task.ScheduleTaskCleanup(clientSession, tx)
		work.ScheduleBackfillInitialTransferBalance(clientSession, tx)
		work.ScheduleIndexSearchLocations(clientSession, tx)
		controller.ScheduleRefreshTransferBalances(clientSession, tx)
		work.ScheduleSetMissingConnectionLocations(clientSession, tx)
		work.ScheduleRemoveLocationLookupResults(clientSession, tx)
		work.ScheduleRemoveCompletedContracts(clientSession, tx)
		work.ScheduleDbMaintenance(clientSession, tx, 0)
		work.ScheduleWarmNetworkGetProviderLocations(clientSession, tx)
		work.ScheduleRemoveExpiredAuthAttempts(clientSession, tx)
		work.ScheduleRemoveOldClientReliabilityStats(clientSession, tx)
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
	})
}

// InitTaskWorker creates a TaskWorker with all task targets registered.
// One TaskWorker can be shared with many goroutines calling EvalTasks.
func InitTaskWorker(ctx context.Context) *task.TaskWorker {

	taskWorker := task.NewTaskWorkerWithDefaults(ctx)

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
			controller.PlaySubscriptionRenewal,
			controller.PlaySubscriptionRenewalPost,
			"bringyour.com/bringyour/controller.PlaySubscriptionRenewal",
			"github.com/urnetwork/server/v2026/bringyour/controller.PlaySubscriptionRenewal",
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
			"github.com/urnetwork/server/v2026/taskworker/work.DeleteDisconnectedNetworkClients",
		),
		task.NewTaskTargetWithPost(
			work.IndexSearchLocations,
			work.IndexSearchLocationsPost,
			"github.com/urnetwork/server/v2026/model.IndexSearchLocations",
		),
		task.NewTaskTargetWithPost(
			controller.RefreshTransferBalances,
			controller.RefreshTransferBalancesPost,
			"bringyour.com/bringyour/controller.RefreshTransferBalances",
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
			work.RemoveExpiredAuthAttempts,
			work.RemoveExpiredAuthAttemptsPost,
		),
		task.NewTaskTargetWithPost(
			work.RemoveOldClientReliabilityStats,
			work.RemoveOldClientReliabilityStatsPost,
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
	)

	return taskWorker
}
