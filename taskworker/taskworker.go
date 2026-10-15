package taskworker

import (
	"context"
	"strconv"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/onboarding"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
	"github.com/urnetwork/server/taskworker/work"
)

// removedTaskTargets lists scheduled task functions that have been deleted
// or renamed in the codebase. Every start reaps their pending rows (see
// reapRetiredTasks): at claim time deploy version skew is indistinguishable
// from permanent removal, so the runner never gives up on them and cleanup
// must be explicit. Add the full function name whenever a scheduled function
// is deleted or renamed; TestRemovedTaskTargetsAreNotRegistered keeps live
// names off this list.
var removedTaskTargets = []string{
	// split into ScheduleRefreshFree/Pro/ReferralTransferBalances
	"github.com/urnetwork/server/controller.RefreshTransferBalances",
}

// Every statement of a startup scheduling or reap transaction ends on the
// server within this bound. Stopping a candidate, as warp does when its
// readiness poll times out, only closes its socket, and PostgreSQL notices
// that only after the running statement ends. An unbounded statement thus
// outlives its process while it holds, or queues for, the run-once rows that
// later candidates and live workers share. A timed-out scheduling attempt
// fails with a retryable startup read error, which the startup owner retries.
var taskworkerStartupStatementTimeout = 10 * time.Second

// Applies the bound to the caller's transaction only.
func boundStartupStatementsInTx(ctx context.Context, tx server.PgTx) {
	server.RaisePgResult(tx.Exec(
		ctx,
		`SELECT set_config('statement_timeout', $1, true)`,
		strconv.FormatInt(taskworkerStartupStatementTimeout.Milliseconds(), 10),
	))
}

// InitTasks schedules the recurring tasks, then reaps retired rows. It backs
// the `init-tasks` subcommand. A serving worker schedules before readiness and
// reaps after admission instead (see startTaskworkerRuntime).
func InitTasks(ctx context.Context) {
	initTaskSchedule(ctx)
	reapRetiredTasks(ctx)
	controller.ApplyBlobRetention(ctx)
}

// Scheduling is the bounded startup work that readiness waits on. Its
// transaction holds every shared run-once row it upserts until commit, so it
// runs nothing else: retired-row cleanup is reapRetiredTasks, and long-lived
// retention observers start after the process acquires its runtime owner.
// After the commit the expiry sweep starts on a fresh pass; a row held by a
// run in flight is retried in the background (startup_close_expired.go).
func initTaskSchedule(ctx context.Context) {
	// Run-once upserts merge independently with live claim updates. A fixed
	// snapshot needlessly retries the entire initializer whenever a claim wins.
	server.Tx(ctx, func(tx server.PgTx) {
		boundStartupStatementsInTx(ctx, tx)
		clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer clientSession.Cancel()

		// **important** make sure the required functions are loaded in `InitTaskWorker`
		// work.ScheduleWarmEmail(clientSession, tx)
		work.ScheduleExportStats(clientSession, tx)
		work.ScheduleExportProvidersMap(clientSession, tx)
		work.ScheduleBackfillClock(clientSession, tx, server.NowUtc())
		work.ScheduleWebSearchAnalytics(clientSession, tx)
		work.ScheduleRemoveExpiredAuthCodes(clientSession, tx)
		work.ScheduleMaintainNetworkSessions(clientSession, tx)
		controller.ScheduleAppleOfferCodeTopUp(clientSession, tx, server.NowUtc().Add(1*time.Hour))
		controller.ScheduleOnboardingResultsRollup(clientSession, tx, onboarding.NextRollupAt(server.NowUtc()))
		controller.ScheduleOnboardingEmailTrackerSync(clientSession, tx, server.NowUtc())
		controller.ScheduleSubscriptionMetricsSync(clientSession, tx, server.NowUtc())
		controller.ScheduleDeliverAccountMessages(clientSession, tx)
		work.SchedulePayout(clientSession, tx)
		work.ScheduleProcessPendingPayouts(clientSession, tx)
		work.ScheduleCancelHungAccountPayments(clientSession, tx)
		// work.SchedulePopulateAccountWallets(clientSession, tx)
		work.ScheduleOpenContractClosuresOnStartup(clientSession, tx)
		work.ScheduleCloseContractDeadlines(clientSession, tx)
		// idle until contract_close.yml enables it
		work.ScheduleReapScheduledContractCloses(clientSession, tx)
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
		controller.ScheduleRebuildPointsLeaderboard(clientSession, tx)
		work.ScheduleSetMissingConnectionLocations(clientSession, tx)
		work.ScheduleRemoveLocationLookupResults(clientSession, tx)
		work.ScheduleRemoveCompletedContracts(clientSession, tx)
		work.ScheduleReconcileNetEscrow(clientSession, tx)
		work.ScheduleDbMaintenance(clientSession, tx, 0)
		work.ScheduleWarmNetworkGetProviderLocations(clientSession, tx)
		work.ScheduleRemoveExpiredAuthAttempts(clientSession, tx)
		work.ScheduleRemoveExpiredWalletAuthChallenges(clientSession, tx)
		work.ScheduleRemoveExpiredWalletNonces(clientSession, tx)
		work.ScheduleRemoveExpiredProviderEgressLocations(clientSession, tx)
		work.ScheduleRemoveExpiredProviderUrlProbeRuns(clientSession, tx)
		work.ScheduleProberBootstrap(clientSession, tx)
		// no geolocation-source pin refresh: its sources are gone
		// (connect/GEOMAP.md D24), and a pending row drains through the
		// registered task below without scheduling a successor
		work.ScheduleProviderEgressProbeTasks(clientSession, tx)
		// the modern provider gate's evidence (server/capprobe), apart from
		// the URL probe's shards, quota and budget
		work.ScheduleCapabilityProbe(clientSession, tx)
		work.ScheduleRefreshEgressDestinations(clientSession, tx)
		work.ScheduleRollupProviderEgressTallies(clientSession, tx)
		work.ScheduleExtenderProbe(clientSession, tx)
		work.ScheduleExtenderPublish(clientSession, tx)
		// lag.dev measurement certificates and names (lag.dev/PLAN.md C5-C9);
		// both idle until extender.yml enables the measurement block
		work.ScheduleExtenderMeasurementIssue(clientSession, tx)
		work.ScheduleExtenderMeasurementDns(clientSession, tx)
		work.ScheduleRemoveOldExtenderLatencies(clientSession, tx)
		work.ScheduleRemoveExpiredPings(clientSession, tx)
		work.ScheduleDeriveLocations(clientSession, tx)
		work.ScheduleRemoveExpiredBulkClientRemovalQuota(clientSession, tx)
		work.ScheduleRemoveOldAuditNetworkEvents(clientSession, tx)
		work.ScheduleRemoveOldAuditEvents(clientSession, tx)
		work.ScheduleSweepProviderAuditEvents(clientSession, tx)
		work.ScheduleRollupTransferAuditEvents(clientSession, tx)
		work.ScheduleRemoveOldClientReliabilityStats(clientSession, tx)
		work.ScheduleRollupClientReliabilityStats(clientSession, tx)
		work.ScheduleRollupClientDataUsage(clientSession, tx)
		// seeded only while degraded.yml enables the contract degradation valve
		work.ScheduleCheckContractDegradation(clientSession, tx)
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
		work.SchedulePaymentReconcile(clientSession, tx)
		// lag.dev guest expiry and result retention (lag.dev/PLAN.md A2, A5)
		work.ScheduleSweepLag(clientSession, tx)
	}, server.TxReadCommitted)
	work.ScheduleSettlementAccountingTasks(ctx)
	// After the commit, in its own bounded transactions.
	startCloseExpiredContractsFreshPass(ctx)
}

// Removes pending rows that can never run again: removed targets, which
// otherwise reschedule on the skew backoff forever, and the chains of
// subsystems that configuration disables. Each family deletes in its own
// bounded transaction after scheduling has committed, so a slow or blocked
// reap holds no run-once row, and a serving worker runs it after admission.
// The consistency tradeoff: a retired row can be claimed before its reap. A
// removed target then fails with target-not-found and a disabled family's
// post does not reseed; the delete ignores claims either way. A family that
// fails or times out keeps its rows until the next start reaps them again.
func reapRetiredTasks(ctx context.Context) {
	reap := func(description string, remove func(server.PgTx) int64) {
		server.HandleError(func() {
			var removedCount int64
			server.Tx(ctx, func(tx server.PgTx) {
				boundStartupStatementsInTx(ctx, tx)
				removedCount = remove(tx)
			}, server.TxReadCommitted)
			if 0 < removedCount {
				glog.Infof("[taskworker]reaped %d pending %s\n", removedCount, description)
			}
		})
	}
	for _, functionName := range removedTaskTargets {
		reap("tasks for removed target "+functionName, func(tx server.PgTx) int64 {
			return task.RemovePendingTasksForFunctionInTx(ctx, tx, functionName)
		})
	}
	reap("verification tasks while the subnet is disabled", func(tx server.PgTx) int64 {
		return work.RemoveDisabledVerifyTasks(ctx, tx)
	})
	reap("provider egress probe tasks while probing is disabled", func(tx server.PgTx) int64 {
		return work.RemoveDisabledProviderEgressProbeTasks(ctx, tx)
	})
	reap("contract degradation checks while degraded.yml disables them", func(tx server.PgTx) int64 {
		return work.RemoveDisabledContractDegradationTasks(ctx, tx)
	})
}

// How often a production taskworker checks the capability probe's chain;
// replaced by tests.
var capabilityProbeEnsureInterval = 10 * time.Minute

// Every capabilityProbeEnsureInterval until ctx ends, schedules the
// capability probe's chain again when it has no task row while this
// taskworker's capability_probe.yml switches the probe on
// (work.EnsureCapabilityProbe). The startup schedule covers the start; this
// brings back a chain that a pass on a taskworker with an older config, or
// maintenance, ended since.
func ensureCapabilityProbeChain(ctx context.Context) {
	interval := capabilityProbeEnsureInterval
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
		server.HandleError(func() { work.EnsureCapabilityProbe(ctx) })
	}
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
	return initTaskWorkerWithSettings(ctx, settings, WorkloadProfileProduction)
}

// initTaskWorkerWithSettings selects targets before the worker can claim rows.
func initTaskWorkerWithSettings(ctx context.Context, settings *task.TaskWorkerSettings, profile WorkloadProfile) *task.TaskWorker {
	taskWorker := task.NewTaskWorker(ctx, taskWorkerSettingsForProfile(settings, profile))

	// 2024.11.15 migration from "bringyour.com" to new package

	addProfileTargets(taskWorker, profile,
		task.NewTaskCleanupTaskTarget(),
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
			work.BackfillClock,
			work.BackfillClockPost,
		),
		work.NewMaintainNetworkSessionsTaskTarget(),
		task.NewTaskTargetWithPost(
			work.WebSearchAnalytics,
			work.WebSearchAnalyticsPost,
		),
		task.NewTaskTargetWithPost(
			work.RemoveExpiredAuthCodes,
			work.RemoveExpiredAuthCodesPost,
			"bringyour.com/service/taskworker/work.RemoveExpiredAuthCodes",
		),
		work.NewPayoutTaskTarget(),
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
			controller.DeliverAccountMessages,
			controller.DeliverAccountMessagesPost,
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
		work.NewStartupContractClosureTaskTarget(),
		work.NewScheduledContractClosureTaskTarget(),
		work.NewContractDeadlineCloseTaskTarget(),
		work.NewScheduledContractCloseReapTaskTarget(),
		work.NewCloseExpiredContractsTaskTarget(),
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
		model.NewLegacyProviderTotalsTaskTarget(),
		model.NewLegacyNetEscrowMirrorTaskTarget(),
		model.NewQuarantineReservationReleaseTaskTarget(),
		task.NewTaskTargetWithPost(
			model.RemoveNetworkClientsTask,
			model.RemoveNetworkClientsTaskPost,
		),
		task.NewTaskTargetWithPost(
			work.SweepOrphanContractData,
			work.SweepOrphanContractDataPost,
		),
		task.NewTaskTargetWithPost(work.RefreshContractHoles, work.RefreshContractHolesPost),
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
			controller.RebuildPointsLeaderboard,
			controller.RebuildPointsLeaderboardPost,
			"bringyour.com/bringyour/controller.RebuildPointsLeaderboard",
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
			controller.ProviderIntentCheck,
			controller.ProviderIntentCheckPost,
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
		work.NewTransferDebitTaskTarget(),
		work.NewLegacySettlementDispatcherTaskTarget(),
		model.NewLegacyPayerSettlementTaskTarget(),
		model.NewLegacySourceSettlementTaskTarget(),
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
			work.RemoveExpiredProviderEgressLocations,
			work.RemoveExpiredProviderEgressLocationsPost,
		),
		task.NewTaskTargetWithPost(
			work.RemoveExpiredProviderUrlProbeRuns,
			work.RemoveExpiredProviderUrlProbeRunsPost,
		),
		task.WithErrorRetryCap(task.NewTaskTargetWithPost(
			work.ProberBootstrap,
			work.ProberBootstrapPost,
		), work.ProberBootstrapTimeout),
		task.NewTaskTargetWithPost(
			work.RefreshGeolocationSourcePins,
			work.RefreshGeolocationSourcePinsPost,
		),
		work.NewProviderEgressProbeTaskTarget(),
		work.NewProviderEgressProbeDisabledTaskTarget(),
		work.NewCapabilityProbeTaskTarget(),
		task.NewTaskTargetWithPost(work.RollupProviderEgressTallies, work.RollupProviderEgressTalliesPost),
		task.NewTaskTargetWithPost(
			work.RefreshEgressDestinations,
			work.RefreshEgressDestinationsPost,
		),
		task.NewTaskTargetWithPost(
			work.ExtenderProbe,
			work.ExtenderProbePost,
		),
		task.NewTaskTargetWithPost(
			work.ExtenderPublish,
			work.ExtenderPublishPost,
		),
		// lag.dev/PLAN.md C5-C9
		task.NewTaskTargetWithPost(
			work.ExtenderMeasurementIssue,
			work.ExtenderMeasurementIssuePost,
		),
		task.NewTaskTargetWithPost(
			work.ExtenderMeasurementDns,
			work.ExtenderMeasurementDnsPost,
		),
		task.NewTaskTargetWithPost(
			work.RemoveOldExtenderLatencies,
			work.RemoveOldExtenderLatenciesPost,
		),
		task.NewTaskTargetWithPost(
			work.RemoveExpiredPings,
			work.RemoveExpiredPingsPost,
		),
		task.NewTaskTargetWithPost(
			work.DeriveLocations,
			work.DeriveLocationsPost,
		),
		task.NewTaskTargetWithPost(
			work.RemoveExpiredBulkClientRemovalQuota,
			work.RemoveExpiredBulkClientRemovalQuotaPost,
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
			work.SweepProviderAuditEvents,
			work.SweepProviderAuditEventsPost,
		),
		task.NewTaskTargetWithPost(
			work.RollupTransferAuditEvents,
			work.RollupTransferAuditEventsPost,
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
			work.RollupClientDataUsage,
			work.RollupClientDataUsagePost,
		),
		task.NewTaskTargetWithPost(
			work.CheckContractDegradation,
			work.CheckContractDegradationPost,
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
		// the onboarding email campaign: one task per network and step, and the
		// daily App Store one-time offer code top-up
		task.NewTaskTargetWithPost(
			controller.OnboardingCampaignStep,
			controller.OnboardingCampaignStepPost,
		),
		task.NewTaskTargetWithPost(
			controller.AppleOfferCodeTopUp,
			controller.AppleOfferCodeTopUpPost,
		),
		// the nightly onboarding results rollup (02:00 UTC): outcome events,
		// the trial backstop, onboarding_results_daily, guardrails
		task.NewTaskTargetWithPost(
			controller.OnboardingResultsRollup,
			controller.OnboardingResultsRollupPost,
		),
		task.NewTaskTargetWithPost(
			controller.OnboardingEmailTrackerSync,
			controller.OnboardingEmailTrackerSyncPost,
		),
		task.NewTaskTargetWithPost(
			controller.SubscriptionMetricsSync,
			controller.SubscriptionMetricsSyncPost,
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
		work.NewRefreshVerifyProxyEgressTaskTarget(),
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
		task.NewTaskTargetWithPost(
			work.StLedgerV2Step,
			work.StLedgerV2StepPost,
		),
		task.NewTaskTargetWithPost(
			work.PaymentReconcile,
			work.PaymentReconcilePost,
		),
		// lag.dev/PLAN.md A2, A5
		task.NewTaskTargetWithPost(
			work.SweepLag,
			work.SweepLagPost,
		),
	)

	return taskWorker
}
