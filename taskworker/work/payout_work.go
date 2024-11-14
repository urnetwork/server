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
		now := time.Now().UTC()
		year, month, day := now.Year(), now.Month(), now.Day()

		if day < 15 {
			// run on the 15th
			return time.Date(year, month, 15, 0, 0, 0, 0, time.UTC)
		}
		// else run on the 1st
		if month == time.December {
			return time.Date(year+1, time.January, 1, 0, 0, 0, 0, time.UTC)
		} else {
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
	success := err == nil

	return &SchedulePayoutResult{
		Success: success,
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
	// send a continuous verification code message to a bunch of popular email providers

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
