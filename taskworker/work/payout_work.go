package work

import (
	"time"

	"bringyour.com/bringyour"
	"bringyour.com/bringyour/controller"
	"bringyour.com/bringyour/session"
	"bringyour.com/bringyour/task"
)

type SchedulePayoutArgs struct {
}

type SchedulePayoutResult struct{}

func SchedulePayout(clientSession *session.ClientSession, tx bringyour.PgTx) {

	twoWeeks := 14 * 24 * time.Hour

	task.ScheduleTaskInTx(
		tx,
		Payout,
		&SchedulePayoutArgs{},
		clientSession,
		task.RunOnce("payout"),
		task.RunAt(time.Now().Add(twoWeeks)),
	)
}

func Payout(
	schedulePayout *SchedulePayoutArgs,
	clientSession *session.ClientSession,
) (*SchedulePayoutResult, error) {
	// send a continuous verification code message to a bunch of popular email providers

	controller.SendPayments(clientSession)

	return &SchedulePayoutResult{}, nil
}

func PayoutPost(
	schedulePayoutArgs *SchedulePayoutArgs,
	schedulePayoutResult *SchedulePayoutResult,
	clientSession *session.ClientSession,
	tx bringyour.PgTx,
) error {
	SchedulePayout(clientSession, tx)
	return nil
}

type ProcessPendingPayoutsArgs struct {
}

type ProcessPendingPayoutsResult struct{}

func ScheduleProcessPendingPayouts(clientSession *session.ClientSession, tx bringyour.PgTx) {
	task.ScheduleTaskInTx(
		tx,
		ProcessPendingPayouts,
		&ProcessPendingPayoutsArgs{},
		clientSession,
		task.RunOnce("process_pending_payouts"),
		task.RunAt(time.Now().Add(1*time.Hour)),
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
	tx bringyour.PgTx,
) error {
	ScheduleProcessPendingPayouts(clientSession, tx)
	return nil
}
