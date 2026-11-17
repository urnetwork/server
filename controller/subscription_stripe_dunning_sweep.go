// The hourly dunning sweep: the backfill's send (`bringyourctl payments
// stripe-dunning --send`, StripeDunningReport) as a recurring taskworker task,
// so a payment-failed notice that a lost, failed or mishandled webhook left
// unowed is owed within the hour (owner, 2026-11-13: "send now + schedule").
//
// Each run lists every subscription Stripe is retrying (past_due, unpaid) and
// puts its latest invoice through stripeDunningProcessInvoice, the processing
// of invoice.payment_failed and invoice.payment_action_required: the first and
// the final notice, including a renewal that waits on the customer to confirm
// it with their bank (3D Secure). The sweep, the webhooks and a hand send
// therefore decide on the same invoice ledger row, which each locks
// (model.RecordStripeInvoicePaymentFailureInTx) in the transaction that owes
// the notice. Each notice time is set once, and the notice's outbox key is
// unique, so a run that finds an invoice's notices owed owes nothing more, and
// concurrent runs, the task and a hand send included, decide one after the
// other. The account message delivery task sends what a run owes within a
// minute. Like a hand send, a run records each retrying invoice's failure as
// seen now, so the in-app past-due notice shows for as long as Stripe retries
// the subscription (PastDueNoticeMaxAge), and a run that finds the invoice
// paid, voided or uncollectible resolves its failure.
//
// A listing Stripe does not answer fails the run, which the task retries with
// backoff. A subscription whose invoice cannot be read is counted and logged,
// and the next run tries it again. An env without Stripe credentials (local,
// test) skips the run and keeps the schedule.
package controller

import (
	"fmt"
	"strings"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// One schedule fleet-wide.
const stripeDunningSweepTaskKey = "stripe_dunning_sweep"

// The sweep takes no arguments.
type SweepStripeDunningArgs struct {
}

// What one sweep run owed.
type SweepStripeDunningResult struct {
	// the subscriptions Stripe is retrying
	SubscriptionCount int `json:"subscription_count"`
	FirstNoticeCount  int `json:"first_notice_count"`
	FinalNoticeCount  int `json:"final_notice_count"`
	// notices owed to accounts without an email, which see the in-app
	// past-due notice only
	NoEmailCount int `json:"no_email_count"`
	// subscriptions whose invoice could not be read; the next run tries again
	ErrorCount int `json:"error_count"`
	// why the run read nothing
	Skip string `json:"skip,omitempty"`
}

// Schedules the sweep one interval from now, at startup. A pending run keeps
// its earlier time, so a fleet restart neither moves nor adds a run.
func ScheduleSweepStripeDunning(clientSession *session.ClientSession, tx server.PgTx) {
	scheduleSweepStripeDunningAt(clientSession, tx, server.NowUtc().Add(stripeDunningSettingsFunc().SweepInterval))
}

// Schedules the sweep at `runAt`, one run at a time.
func scheduleSweepStripeDunningAt(clientSession *session.ClientSession, tx server.PgTx, runAt time.Time) {
	task.ScheduleTaskInTx(
		tx,
		SweepStripeDunning,
		&SweepStripeDunningArgs{},
		clientSession,
		task.RunOnce(stripeDunningSweepTaskKey),
		task.RunAt(runAt),
		task.MaxTime(stripeDunningSettingsFunc().SweepMaxTime),
	)
}

// Owes every notice the retrying subscriptions are due now, with the
// backfill's send.
func SweepStripeDunning(
	_ *SweepStripeDunningArgs,
	clientSession *session.ClientSession,
) (*SweepStripeDunningResult, error) {
	if !stripeReconcileHasCredentials() {
		return &SweepStripeDunningResult{Skip: "no Stripe credentials in this env"}, nil
	}
	rows, err := StripeDunningReport(clientSession.Ctx, true)
	if err != nil {
		return nil, fmt.Errorf("stripe dunning sweep: %w", err)
	}
	result := &SweepStripeDunningResult{SubscriptionCount: len(rows)}
	for _, row := range rows {
		if strings.HasPrefix(row.Skip, "error: ") {
			result.ErrorCount += 1
			glog.Warningf("[stripe]dunning sweep: subscription %s invoice %s: %s\n", row.SubscriptionId, row.InvoiceId, row.Skip)
			continue
		}
		switch stripePaymentFailedNotice(row.Notice) {
		case stripePaymentFailedNoticeFirst:
			result.FirstNoticeCount += 1
		case stripePaymentFailedNoticeFinal:
			result.FinalNoticeCount += 1
		default:
			continue
		}
		if !row.HasEmail {
			result.NoEmailCount += 1
		}
	}
	glog.Infof(
		"[stripe]dunning sweep: %d subscriptions retrying; owed %d first and %d final notices (%d to accounts without an email); %d unreadable\n",
		result.SubscriptionCount,
		result.FirstNoticeCount,
		result.FinalNoticeCount,
		result.NoEmailCount,
		result.ErrorCount,
	)
	return result, nil
}

// Schedules the next run one interval after this one.
func SweepStripeDunningPost(
	_ *SweepStripeDunningArgs,
	_ *SweepStripeDunningResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	scheduleSweepStripeDunningAt(clientSession, tx, server.NowUtc().Add(stripeDunningSettingsFunc().SweepInterval))
	return nil
}
