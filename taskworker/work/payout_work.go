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
	task.ScheduleTaskInTx(
		tx,
		Payout,
		&SchedulePayoutArgs{},
		clientSession,
		task.RunOnce("payout"),
		task.RunAt(time.Now().Add(2*time.Hour)),
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
	warmEmail *SchedulePayoutArgs,
	warmEmailResult *SchedulePayoutResult,
	clientSession *session.ClientSession,
	tx bringyour.PgTx,
) error {
	SchedulePayout(clientSession, tx)
	return nil
}
