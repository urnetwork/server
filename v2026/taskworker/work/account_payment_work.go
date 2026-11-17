// The USDC provider payout tasks, which ended with USDC provider payouts
// (owner decisions 2026-11-12 and 2026-11-13: "settle closed rows, let's close
// out usdc entirely"). Nothing schedules them. A row queued before then, or by
// a taskworker of an older release during a deploy, still names one of these
// targets, under its function name or its alternate name, so each stays
// registered: such a row runs once, does nothing, and schedules nothing after
// it. A retry row's `retry` field is ignored.
package work

import (
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
)

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

type ProcessPendingPayoutsArgs struct {
}

type ProcessPendingPayoutsResult struct {
}

// Schedules no payment.
func ProcessPendingPayouts(
	processPending *ProcessPendingPayoutsArgs,
	clientSession *session.ClientSession,
) (*ProcessPendingPayoutsResult, error) {
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

// Cancels nothing: no payment is pending, and none is planned again.
func CancelHungAccountPayments(
	cancelHungAccountPayments *CancelHungAccountPaymentsArgs,
	clientSession *session.ClientSession,
) (*CancelHungAccountPaymentsResult, error) {
	return &CancelHungAccountPaymentsResult{}, nil
}

// Schedules no next run.
func CancelHungAccountPaymentsPost(
	cancelHungAccountPayments *CancelHungAccountPaymentsArgs,
	cancelHungAccountPaymentsResult *CancelHungAccountPaymentsResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	return nil
}
