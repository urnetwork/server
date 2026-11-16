package work

// StDepositLateEntry(epoch): a check of the sheet of an epoch whose deposit chain
// concluded with a zero-price skip (controller.StDepositLateEntryDue). The sync turn
// keeps one scheduled about every 15 minutes while the epoch's deposit rows leave a
// late price to pay (scheduleStDepositLateEntry). Once a verified sheet prices the
// epoch, the check seeds its deposit chain (scheduleStDeposit), which then pays and
// retries as any other. While that chain has not landed, each check seeds it again if
// it is no longer pending. The check itself sends nothing.

import (
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

type StDepositLateEntryArgs struct {
	DeploymentKey string `json:"deployment_key"`
	Epoch         uint64 `json:"epoch"`
}

type StDepositLateEntryResult struct {
	Seeded bool `json:"seeded"`
}

func stDepositLateEntryRunOnce(deploymentKey string, epoch uint64) *task.RunOnceOption {
	return task.RunOnce("st_deposit_late_entry", deploymentKey, epoch)
}

// scheduleStDepositLateEntry schedules the next check of the epoch's sheet, unless one
// is pending, while the epoch's deposit rows leave a late price to pay. A panic here,
// as from a database read, is logged and cannot stop the rest of the sync turn.
func scheduleStDepositLateEntry(clientSession *session.ClientSession, deploymentKey model.StDeploymentKey, epoch uint64) {
	defer func() {
		if r := recover(); r != nil {
			glog.Errorf("[st]epoch %d late price check not scheduled: %v\n", epoch, r)
		}
	}()
	if !controller.StDepositLateEntryWatched(clientSession.Ctx, epoch) {
		return
	}
	task.ScheduleTaskIfAbsent(
		StDepositLateEntry,
		&StDepositLateEntryArgs{DeploymentKey: string(deploymentKey), Epoch: epoch},
		clientSession,
		stDepositLateEntryRunOnce(string(deploymentKey), epoch),
		task.RunAt(server.NowUtc().Add(controller.StDepositLateEntryInterval)),
		task.MaxTime(5*time.Minute),
	)
}

// StDepositLateEntry checks the epoch's sheet once and seeds its deposit chain when a
// verified sheet prices it. The sync turn schedules the next check.
func StDepositLateEntry(
	stDepositLateEntry *StDepositLateEntryArgs,
	clientSession *session.ClientSession,
) (*StDepositLateEntryResult, error) {
	if _, current := stTaskDeploymentCurrent(stDepositLateEntry.DeploymentKey); !current {
		return &StDepositLateEntryResult{}, nil
	}
	due, err := controller.StDepositLateEntryDue(clientSession.Ctx, stDepositLateEntry.Epoch)
	if err != nil {
		glog.Infof("[st]epoch %d late price check: %s\n", stDepositLateEntry.Epoch, err)
		return &StDepositLateEntryResult{}, nil
	}
	if !due {
		return &StDepositLateEntryResult{}, nil
	}
	glog.Infof("[st]epoch %d is priced by an entry published after its zero-price skip: its deposit is seeded\n", stDepositLateEntry.Epoch)
	scheduleStDeposit(clientSession, model.StDeploymentKey(stDepositLateEntry.DeploymentKey), stDepositLateEntry.Epoch)
	return &StDepositLateEntryResult{Seeded: true}, nil
}

func StDepositLateEntryPost(
	stDepositLateEntry *StDepositLateEntryArgs,
	stDepositLateEntryResult *StDepositLateEntryResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	return nil
}
