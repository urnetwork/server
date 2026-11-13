package work

import (
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// The weekly USDC payout run ended with USDC payouts (owner decision
// 2026-11-12: USDC payouts ended), and nothing schedules it. A "payout" or
// "payout_retry" row queued before then still names this target, under its
// function name or its pre-2024.11 alternate name, so the target stays
// registered: such a row runs once, plans and sends nothing, and schedules
// nothing after it. A retry row's `retry` field is ignored.
type SchedulePayoutArgs struct {
}

type SchedulePayoutResult struct {
}

// Plans and sends nothing.
func Payout(
	schedulePayout *SchedulePayoutArgs,
	clientSession *session.ClientSession,
) (*SchedulePayoutResult, error) {
	return &SchedulePayoutResult{}, nil
}

// Schedules no next run.
func PayoutPost(
	schedulePayoutArgs *SchedulePayoutArgs,
	schedulePayoutResult *SchedulePayoutResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
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
