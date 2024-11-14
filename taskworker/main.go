package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"syscall"
	"time"

	"github.com/docopt/docopt-go"

	"github.com/golang/glog"

	"github.com/urnetwork/server/bringyour"
	"github.com/urnetwork/server/bringyour/controller"
	"github.com/urnetwork/server/bringyour/model"
	"github.com/urnetwork/server/bringyour/router"
	"github.com/urnetwork/server/bringyour/session"
	"github.com/urnetwork/server/bringyour/task"
	"github.com/urnetwork/server/taskworker/work"
)

const RemoveTaskTimeout = 90 * 24 * time.Hour
const RetryTimeoutAfterError = 30 * time.Second
const PollTimeout = 1 * time.Second

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
  -n --count=<count>  Number of worker processes [default: 16].
  -b --batch_size=<batch_size>  Batch size [default: 8].`

	opts, err := docopt.ParseArgs(usage, os.Args[1:], bringyour.RequireVersion())
	if err != nil {
		panic(err)
	}

	quitEvent := bringyour.NewEventWithContext(context.Background())
	closeFn := quitEvent.SetOnSignals(syscall.SIGQUIT, syscall.SIGTERM)
	defer closeFn()

	if initTasks_, _ := opts.Bool("init-tasks"); initTasks_ {
		initTasks(quitEvent.Ctx)
	} else {
		count, _ := opts.Int("--count")
		batchSize, _ := opts.Int("--batch_size")
		port, _ := opts.Int("--port")

		glog.Infof(
			"[taskworker]starting %s %s %d task workers with batch size %d\n",
			bringyour.RequireEnv(),
			bringyour.RequireVersion(),
			count,
			batchSize,
		)

		initTasks(quitEvent.Ctx)

		// one TaskWorker can be shared with many go routines calling EvalTasks
		taskWorker := initTaskWorker(quitEvent.Ctx)
		for i := 0; i < count; i += 1 {
			go evalTasks(quitEvent.Ctx, taskWorker, batchSize)
		}

		routes := []*router.Route{
			router.NewRoute("GET", "/status", router.WarpStatus),
		}

		glog.Infof(
			"[taskworker]serving %s %s on *:%d\n",
			bringyour.RequireEnv(),
			bringyour.RequireVersion(),
			port,
		)

		routerHandler := router.NewRouter(quitEvent.Ctx, routes)
		err = http.ListenAndServe(fmt.Sprintf(":%d", port), routerHandler)
		glog.Errorf("[taskworker]close = %s\n", err)
	}
}

func initTasks(ctx context.Context) {
	bringyour.Tx(ctx, func(tx bringyour.PgTx) {
		clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer clientSession.Cancel()

		// work.ScheduleWarmEmail(clientSession, tx)
		work.ScheduleExportStats(clientSession, tx)
		work.ScheduleRemoveExpiredAuthCodes(clientSession, tx)
		work.SchedulePayout(clientSession, tx)
		work.ScheduleProcessPendingPayouts(clientSession, tx)
		work.SchedulePopulateAccountWallets(clientSession, tx)
		work.ScheduleCloseExpiredContracts(clientSession, tx)
		work.ScheduleCloseExpiredNetworkClientHandlers(clientSession, tx)
		work.ScheduleDeleteDisconnectedNetworkClients(clientSession, tx)
		ScheduleTaskCleanup(clientSession, tx)
		work.ScheduleBackfillInitialTransferBalance(clientSession, tx)
		model.ScheduleIndexSearchLocations(clientSession, tx)
		controller.ScheduleRefreshTransferBalances(clientSession, tx)
	})
}

func initTaskWorker(ctx context.Context) *task.TaskWorker {

	taskWorker := task.NewTaskWorker(ctx)

	taskWorker.AddTargets(
		// task.NewTaskTargetWithPost(work.WarmEmail, work.WarmEmailPost),
		task.NewTaskTargetWithPost(work.ExportStats, work.ExportStatsPost),
		task.NewTaskTargetWithPost(work.RemoveExpiredAuthCodes, work.RemoveExpiredAuthCodesPost),
		task.NewTaskTargetWithPost(work.Payout, work.PayoutPost),
		task.NewTaskTargetWithPost(work.ProcessPendingPayouts, work.ProcessPendingPayoutsPost),
		task.NewTaskTargetWithPost(TaskCleanup, TaskCleanupPost),
		task.NewTaskTargetWithPost(controller.PlaySubscriptionRenewal, controller.PlaySubscriptionRenewalPost),
		task.NewTaskTargetWithPost(work.BackfillInitialTransferBalance, work.BackfillInitialTransferBalancePost),
		task.NewTaskTargetWithPost(controller.PopulateAccountWallets, work.PopulateAccountWalletsPost),
		task.NewTaskTargetWithPost(work.CloseExpiredContracts, work.CloseExpiredContractsPost),
		task.NewTaskTargetWithPost(work.CloseExpiredNetworkClientHandlers, work.CloseExpiredNetworkClientHandlersPost),
		task.NewTaskTargetWithPost(work.DeleteDisconnectedNetworkClients, work.DeleteDisconnectedNetworkClientsPost),
		task.NewTaskTargetWithPost(model.IndexSearchLocations, model.IndexSearchLocationsPost),
		task.NewTaskTargetWithPost(controller.RefreshTransferBalances, controller.RefreshTransferBalancesPost),
		task.NewTaskTargetWithPost(controller.AdvancePayment, controller.AdvancePaymentPost),
	)

	return taskWorker
}

func evalTasks(ctx context.Context, taskWorker *task.TaskWorker, batchSize int) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		finishedTaskIds, rescheduledTaskIds, postRescheduledTaskIds, err := taskWorker.EvalTasks(batchSize)
		if err != nil {
			glog.Infof("[taskworker]error running tasks: %s\n", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(RetryTimeoutAfterError):
			}
		} else if len(finishedTaskIds)+len(rescheduledTaskIds)+len(postRescheduledTaskIds) == 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(PollTimeout):
			}
		}
	}
}

// PERIODIC CLEANUP

type TaskCleanupArgs struct {
}

type TaskCleanupResult struct {
}

func ScheduleTaskCleanup(clientSession *session.ClientSession, tx bringyour.PgTx) {
	task.ScheduleTaskInTx(
		tx,
		TaskCleanup,
		&TaskCleanupArgs{},
		clientSession,
		task.RunOnce("task_cleanup"),
		task.RunAt(time.Now().Add(1*time.Hour)),
	)
}

func TaskCleanup(
	taskCleanup *TaskCleanupArgs,
	clientSession *session.ClientSession,
) (*TaskCleanupResult, error) {
	minTime := time.Now().Add(-RemoveTaskTimeout)
	task.RemoveFinishedTasks(clientSession.Ctx, minTime)
	return &TaskCleanupResult{}, nil
}

func TaskCleanupPost(
	taskCleanup *TaskCleanupArgs,
	taskCleanupResult *TaskCleanupResult,
	clientSession *session.ClientSession,
	tx bringyour.PgTx,
) error {
	ScheduleTaskCleanup(clientSession, tx)
	return nil
}
