package work

import (
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

type SchedulePayoutArgs struct {
	Retry bool `json:"retry"`
}

type SchedulePayoutResult struct {
	Success bool `json:"success"`
}

func SchedulePayout(clientSession *session.ClientSession, tx server.PgTx) {
	runAt := func() time.Time {
		now := server.NowUtc()
		year, month, day := now.Date()

		if day < 7 {
			// run on the 7th
			return time.Date(year, month, 7, 0, 0, 0, 0, time.UTC)
		} else if day < 15 {
			// run on the 15th
			return time.Date(year, month, 15, 0, 0, 0, 0, time.UTC)
		} else if day < 22 {
			// run on the 22nd
			return time.Date(year, month, 22, 0, 0, 0, 0, time.UTC)
		} else {
			// else run on the 1st of next month
			return time.Date(year, month+1, 1, 0, 0, 0, 0, time.UTC)
		}
	}()
	task.ScheduleTaskInTx(
		tx,
		Payout,
		&SchedulePayoutArgs{},
		clientSession,
		task.RunOnce("payout"),
		task.RunAt(runAt),
	)
}

func Payout(
	schedulePayout *SchedulePayoutArgs,
	clientSession *session.ClientSession,
) (*SchedulePayoutResult, error) {
	err := controller.SendPayments(clientSession)
	if err != nil {
		return nil, err
	}

	return &SchedulePayoutResult{
		Success: true,
	}, nil
}

func PayoutPost(
	schedulePayoutArgs *SchedulePayoutArgs,
	schedulePayoutResult *SchedulePayoutResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	if schedulePayoutResult.Success {
		SchedulePayout(clientSession, tx)
	} else {
		// retry faster than the normal payment schedule
		schedulePayout := &SchedulePayoutArgs{
			Retry: true,
		}
		runAt := time.Now().Add(1 * time.Hour)
		task.ScheduleTaskInTx(
			tx,
			Payout,
			schedulePayout,
			clientSession,
			task.RunOnce("payout_retry"),
			task.RunAt(runAt),
		)
	}
	return nil
}

// run at start
type ProcessPendingPayoutsArgs struct {
}

type ProcessPendingPayoutsResult struct {
}

func ScheduleProcessPendingPayouts(clientSession *session.ClientSession, tx server.PgTx) {
	task.ScheduleTaskInTx(
		tx,
		ProcessPendingPayouts,
		&ProcessPendingPayoutsArgs{},
		clientSession,
		task.RunOnce("process_pending_payouts"),
	)
}

func ProcessPendingPayouts(
	processPending *ProcessPendingPayoutsArgs,
	clientSession *session.ClientSession,
) (*ProcessPendingPayoutsResult, error) {
	controller.SchedulePendingPayments(clientSession)

	return &ProcessPendingPayoutsResult{}, nil
}

func ProcessPendingPayoutsPost(
	processPendingArgs *ProcessPendingPayoutsArgs,
	processPendingResult *ProcessPendingPayoutsResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	return nil
}
