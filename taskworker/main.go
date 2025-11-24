package main

import (
	"context"
	// "fmt"
	// "net/http"
	"net"
	"os"
	"strconv"
	"syscall"
	"time"

	"github.com/docopt/docopt-go"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	// "github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
	"github.com/urnetwork/server/taskworker/work"
)

func main() {
	usage := `BringYour task worker.

Usage:
  taskworker [--port=<port>] [--count=<count>] [--batch_size=<batch_size>]
  taskworker init-tasks
  taskworker -h | --help
  taskworker --version

Options:
  -h --help     Show this screen.
  --version     Show version.
  -p --port=<port>  Listen port [default: 80].
  -n --count=<count>  Number of worker processes [default: 8].
  -b --batch_size=<batch_size>  Batch size [default: 4].`

	opts, err := docopt.ParseArgs(usage, os.Args[1:], server.RequireVersion())
	if err != nil {
		panic(err)
	}

	quitEvent := server.NewEventWithContext(context.Background())
	closeFn := quitEvent.SetOnSignals(syscall.SIGQUIT, syscall.SIGTERM)
	defer closeFn()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if initTasks_, _ := opts.Bool("init-tasks"); initTasks_ {
		initTasks(ctx)
	} else {
		// note the total parallelism is count*batch_size
		count, _ := opts.Int("--count")
		batchSize, _ := opts.Int("--batch_size")
		port, _ := opts.Int("--port")

		glog.Infof(
			"[taskworker]starting %s %s %d task workers with batch size %d\n",
			server.RequireEnv(),
			server.RequireVersion(),
			count,
			batchSize,
		)

		initTasks(ctx)

		// one TaskWorker can be shared with many go routines calling EvalTasks
		settings := task.DefaultTaskWorkerSettings()
		settings.BatchSize = batchSize
		taskWorker := initTaskWorker(ctx)
		for i := 0; i < count; i += 1 {
			go taskWorker.Run()
		}

		// drain on sigterm
		go func() {
			defer cancel()
			select {
			case <-ctx.Done():
				return
			case <-quitEvent.Ctx.Done():
				taskWorker.Drain()
			}
		}()

		routes := []*router.Route{
			router.NewRoute("GET", "/status", router.WarpStatus),
		}

		glog.Infof(
			"[taskworker]serving %s %s on *:%d\n",
			server.RequireEnv(),
			server.RequireVersion(),
			port,
		)

		listenIpv4, _, listenPort := server.RequireListenIpPort(port)

		reusePort := false

		err := server.HttpListenAndServeWithReusePort(
			ctx,
			net.JoinHostPort(listenIpv4, strconv.Itoa(listenPort)),
			router.NewRouter(ctx, routes),
			reusePort,
		)
		if err != nil {
			panic(err)
		}
		glog.Infof("[taskworker]close\n")
	}
}

func initTasks(ctx context.Context) {
	server.Tx(ctx, func(tx server.PgTx) {
		clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer clientSession.Cancel()

		// **important** make sure the required functions are loaded in `initTaskWorker`
		// work.ScheduleWarmEmail(clientSession, tx)
		work.ScheduleExportStats(clientSession, tx)
		work.ScheduleRemoveExpiredAuthCodes(clientSession, tx)
		work.SchedulePayout(clientSession, tx)
		work.ScheduleProcessPendingPayouts(clientSession, tx)
		// work.SchedulePopulateAccountWallets(clientSession, tx)
		work.ScheduleCloseExpiredContracts(clientSession, tx)
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

func initTaskWorker(ctx context.Context) *task.TaskWorker {

	taskWorker := task.NewTaskWorkerWithDefaults(ctx)

	// 2024.11.15 migration from "bringyour.com" to new package

	taskWorker.AddTargets(
		task.NewTaskTargetWithPost(
			task.TaskCleanup,
			task.TaskCleanupPost,
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
			work.IndexSearchLocations,
			work.IndexSearchLocationsPost,
			"github.com/urnetwork/server/model.IndexSearchLocations",
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
