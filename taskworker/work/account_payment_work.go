package work

import (
	"context"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

type SchedulePayoutArgs struct {
	Retry bool `json:"retry"`
}

type SchedulePayoutResult struct {
	Success bool `json:"success"`
}

const payoutHourUtc = 10

func nextPayoutTime(now time.Time) time.Time {
	now = now.UTC()
	year, month, day := now.Date()

	// Always schedule the next Sunday, preserving the existing once-per-week
	// cadence. 10:00 UTC is deliberately outside the 00:00 full-backup window
	// and the 03:00/07:00 database-maintenance windows.
	day += 7 - int(now.Weekday())
	return time.Date(year, month, day, payoutHourUtc, 0, 0, 0, time.UTC)
}

func SchedulePayout(clientSession *session.ClientSession, tx server.PgTx) {
	runAt := nextPayoutTime(server.NowUtc())
	task.ScheduleTaskInTx(
		tx,
		Payout,
		&SchedulePayoutArgs{},
		clientSession,
		task.RunOnce("payout"),
		task.MaxTime(6*time.Hour),
		task.RunAt(runAt),
	)
}

// A run that committed planning progress retries promptly through its target
// (NewPayoutTaskTarget), however it stopped.
func Payout(
	schedulePayout *SchedulePayoutArgs,
	clientSession *session.ClientSession,
) (*SchedulePayoutResult, error) {
	// payouts were disabled while funds were being sent to the wrong wallets.
	// wallet ownership is now validated at every layer (set/plan/advance) and
	// submits are idempotent, so payouts are re-enabled.
	err := controller.SendPayments(clientSession)
	if err != nil {
		return nil, err
	}

	return &SchedulePayoutResult{
		Success: true,
	}, nil
}

// The payout task's target. The next run resumes a run's committed planning
// progress (a plan, a staged assignment batch or its completion, or a
// reliability chunk), so a run that committed progress retries promptly however
// it stopped: a returned error, a panic, a drain, or its own max time. The task
// runner joins its "Timeout" error around the body's failure after the body
// returns, and it shortens a retry only for a hint at the root of the failure,
// so the hint is attached here, outside the runner. A run that committed
// nothing keeps the ordinary error backoff.
type payoutTaskTarget struct {
	task.Target
}

// The registered payout target, under Payout's function name and its
// pre-2024.11 alternate name.
func NewPayoutTaskTarget() task.Target {
	return &payoutTaskTarget{
		Target: task.NewTaskTargetWithPost(
			Payout,
			PayoutPost,
			"bringyour.com/service/taskworker/work.Payout",
		),
	}
}

// Runs the ordinary target, including its max-time timer, with a progress
// observer in its context. A failure keeps its text and arguments and still
// counts as an error; only its retry delay changes.
func (self *payoutTaskTarget) Run(ctx context.Context, queued *task.Task) (any, func(server.PgTx) ([]server.PostFunction, error), error) {
	ctx, progress := model.WithPaymentPlanProgress(ctx)
	result, post, err := self.Target.Run(ctx, queued)
	if err != nil && progress.Committed() {
		err = task.WithCommittedProgressRetry(err)
	}
	return result, post, err
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
		runAt := server.NowUtc().Add(1 * time.Hour)
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
		task.MaxTime(1*time.Hour),
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

type CancelHungAccountPaymentsArgs struct {
}

type CancelHungAccountPaymentsResult struct {
}

func ScheduleCancelHungAccountPayments(clientSession *session.ClientSession, tx server.PgTx) {
	task.ScheduleTaskInTx(
		tx,
		CancelHungAccountPayments,
		&CancelHungAccountPaymentsArgs{},
		clientSession,
		task.RunOnce("cancel_hung_account_payments"),
		task.RunAt(server.NowUtc().Add(24*time.Hour)),
		task.MaxTime(1*time.Hour),
	)
}

func CancelHungAccountPayments(
	cancelHungAccountPayments *CancelHungAccountPaymentsArgs,
	clientSession *session.ClientSession,
) (*CancelHungAccountPaymentsResult, error) {
	canceledCount := model.CancelHungAccountPayments(clientSession.Ctx, server.NowUtc())
	if 0 < canceledCount {
		glog.Infof("[payw]canceled %d hung payments for re-planning.\n", canceledCount)
	}
	return &CancelHungAccountPaymentsResult{}, nil
}

func CancelHungAccountPaymentsPost(
	cancelHungAccountPayments *CancelHungAccountPaymentsArgs,
	cancelHungAccountPaymentsResult *CancelHungAccountPaymentsResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	ScheduleCancelHungAccountPayments(clientSession, tx)
	return nil
}
