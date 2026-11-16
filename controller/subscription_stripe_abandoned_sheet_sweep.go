// The hourly abandoned payment sheet sweep (owner, 2026-11-16). A payment
// sheet that is opened and left leaves its subscription behind: trialing (the
// yearly plan) or incomplete (the monthly), metadata payment_sheet pending,
// and no card. It never gets Pro, since its held $0 trial invoice is never
// credited, but it sat in Stripe as a trial until the trial ended 14 days
// later, when Stripe canceled it for the missing payment method. That
// inflated Stripe's trial count: 130 of 198 recently cancelled "trials" never
// had a card.
//
// Each run finds those subscriptions once they are older than
// stripeAbandonedPaymentSheetAge, through Stripe's search (the server keeps no
// record of a sheet's subscription), and cancels each one after reading it
// again, so a card attached at the last moment is never lost:
//
//	read again, the sheet is attached, or has a card on the
//	  subscription or as its customer's default               -> kept
//	its trial invoice is in the stripe_invoice ledger         -> kept (credited),
//	                                                             without a read
//	                                                             of Stripe
//	otherwise                                                 -> canceled
//
// The ledger rule is the owner's (2026-11-16: "ok just let them run out"):
// the hourly reconcile once credited abandoned sheets' trials, and those
// windows run out as they are. Canceling their subscriptions would let the
// reconcile end them early, so the sweep never touches a sheet whose trial
// was credited. A sheet refused for its card (subscription_stripe_card_check.go)
// waits for its card like any other and is canceled the same way. A customer
// who opens a new sheet has the old one replaced at once
// (stripeCancelPendingPaymentSheetSubscriptions); the sweep is for the ones
// never come back to. The trial-ending notice skips these subscriptions too
// (stripeTrialEndingNotice): a canceled one gets none.
//
// A run makes at most MaxCalls calls to Stripe (each search page, each read,
// each cancel) and stops after MaxErrors failed reads or cancels; the next run
// carries on. A search Stripe does not answer fails the run, which the task
// retries with backoff. An env without Stripe credentials (local, test) skips
// the run and keeps the schedule. Cancellations are counted in
// urnetwork_subscription_stripe_abandoned_payment_sheets_total and logged by
// subscription id, with no customer detail.
package controller

import (
	"context"
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stripe/stripe-go/v82"
	"github.com/stripe/stripe-go/v82/subscription"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// One schedule fleet-wide.
const stripeAbandonedSheetSweepTaskKey = "stripe_abandoned_payment_sheet_sweep"

// stripeAbandonedPaymentSheetAge is how long a payment sheet's subscription
// waits for its card before the sweep cancels it as abandoned. A sheet is
// completed within minutes; a day is far longer than any session, so a
// customer still completing one never has it canceled under them.
const stripeAbandonedPaymentSheetAge = 24 * time.Hour

// The page size of Stripe's search, its most.
const stripeAbandonedSheetSearchPageSize = 100

// The sweep's tunables.
type stripeAbandonedSheetSettings struct {
	// how long after a run the next one runs
	Interval time.Duration
	// the most calls one run makes to Stripe: each search page (up to 100
	// subscriptions), each read before a cancel, and each cancel
	MaxCalls int
	// the failed reads and cancels one run takes before it stops
	MaxErrors int
	// how long the task may run
	MaxTime time.Duration
}

// A new instance on every call.
func defaultStripeAbandonedSheetSettings() *stripeAbandonedSheetSettings {
	return &stripeAbandonedSheetSettings{
		Interval:  time.Hour,
		MaxCalls:  200,
		MaxErrors: 5,
		MaxTime:   15 * time.Minute,
	}
}

// Replaceable only by the tests; production never mutates it.
var stripeAbandonedSheetSettingsFunc = defaultStripeAbandonedSheetSettings

// What became of a sheet the sweep found (the outcome label).
const (
	// canceled at Stripe
	stripeAbandonedSheetOutcomeCanceled = "canceled"
	// it had a card or was attached, as found or read again
	stripeAbandonedSheetOutcomeKept = "kept"
	// its trial invoice was credited before: it runs out as it is, found
	// again by every run until its trial ends
	stripeAbandonedSheetOutcomeCredited = "credited"
	// Stripe did not answer the read or the cancel; the next run tries again
	stripeAbandonedSheetOutcomeFailed = "failed"
)

// Abandoned payment sheets by what the sweep did.
var stripeAbandonedSheetsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "subscription",
	Name:      "stripe_abandoned_payment_sheets_total",
	Help:      "Payment sheet subscriptions the hourly sweep found pending with no card for over a day, by outcome: canceled; kept (it had a card or was attached, as found or read again); credited (its trial invoice was credited before, so it runs out as it is, found again every run until its trial ends); failed (Stripe did not answer the read or the cancel; the next run tries again)",
}, []string{"outcome"})

// Registers the metric, every series from the start so the first
// cancellation is an increase.
func init() {
	prometheus.MustRegister(stripeAbandonedSheetsTotal)
	for _, outcome := range []string{
		stripeAbandonedSheetOutcomeCanceled,
		stripeAbandonedSheetOutcomeKept,
		stripeAbandonedSheetOutcomeCredited,
		stripeAbandonedSheetOutcomeFailed,
	} {
		stripeAbandonedSheetsTotal.WithLabelValues(outcome)
	}
}

// The sweep takes no arguments.
type SweepStripeAbandonedSheetsArgs struct {
}

// What one sweep run did.
type SweepStripeAbandonedSheetsResult struct {
	// the subscriptions Stripe's search returned
	FoundCount    int `json:"found_count"`
	CanceledCount int `json:"canceled_count"`
	// had a card or was attached, as found or read again
	KeptCount int `json:"kept_count"`
	// its trial was credited before, so it runs out as it is
	CreditedCount int `json:"credited_count"`
	FailedCount   int `json:"failed_count"`
	// the run stopped at its call budget or its error budget; the next run
	// carries on
	Stopped string `json:"stopped,omitempty"`
	// why the run read nothing
	Skip string `json:"skip,omitempty"`
}

// Schedules the sweep one interval from now, at startup. A pending run keeps
// its earlier time, so a fleet restart neither moves nor adds a run.
func ScheduleSweepStripeAbandonedSheets(clientSession *session.ClientSession, tx server.PgTx) {
	scheduleSweepStripeAbandonedSheetsAt(clientSession, tx, server.NowUtc().Add(stripeAbandonedSheetSettingsFunc().Interval))
}

// Schedules the sweep at `runAt`, one run at a time.
func scheduleSweepStripeAbandonedSheetsAt(clientSession *session.ClientSession, tx server.PgTx, runAt time.Time) {
	task.ScheduleTaskInTx(
		tx,
		SweepStripeAbandonedSheets,
		&SweepStripeAbandonedSheetsArgs{},
		clientSession,
		task.RunOnce(stripeAbandonedSheetSweepTaskKey),
		task.RunAt(runAt),
		task.MaxTime(stripeAbandonedSheetSettingsFunc().MaxTime),
	)
}

// Cancels the payment sheets abandoned for more than a day.
func SweepStripeAbandonedSheets(
	_ *SweepStripeAbandonedSheetsArgs,
	clientSession *session.ClientSession,
) (*SweepStripeAbandonedSheetsResult, error) {
	if !stripeReconcileHasCredentials() {
		return &SweepStripeAbandonedSheetsResult{Skip: "no Stripe credentials in this env"}, nil
	}
	stripe.Key = stripeApiToken()
	return stripeSweepAbandonedSheets(clientSession.Ctx, server.NowUtc(), stripeAbandonedSheetSettingsFunc())
}

// Schedules the next run one interval after this one.
func SweepStripeAbandonedSheetsPost(
	_ *SweepStripeAbandonedSheetsArgs,
	_ *SweepStripeAbandonedSheetsResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	scheduleSweepStripeAbandonedSheetsAt(clientSession, tx, server.NowUtc().Add(stripeAbandonedSheetSettingsFunc().Interval))
	return nil
}

// stripePaymentSheetAbandoned reports whether a subscription is a payment
// sheet left without a card for longer than stripeAbandonedPaymentSheetAge
// before now: still pending, trialing or incomplete, created before the
// cutoff, and no payment method on the subscription or as its customer's
// default. The customer counts only when it was read with the subscription
// (expanded); the sweep reads it so before every cancel.
func stripePaymentSheetAbandoned(sub *stripe.Subscription, now time.Time) bool {
	if sub == nil || sub.Metadata[stripeMetadataPaymentSheet] != stripePaymentSheetPending {
		return false
	}
	switch sub.Status {
	case stripe.SubscriptionStatusTrialing, stripe.SubscriptionStatusIncomplete:
	default:
		return false
	}
	if sub.Created <= 0 || now.Add(-stripeAbandonedPaymentSheetAge).Before(time.Unix(sub.Created, 0)) {
		// a customer may still be completing it
		return false
	}
	if sub.DefaultPaymentMethod != nil || sub.DefaultSource != nil {
		return false
	}
	if customer := sub.Customer; customer != nil {
		if customer.DefaultSource != nil {
			return false
		}
		if customer.InvoiceSettings != nil && customer.InvoiceSettings.DefaultPaymentMethod != nil {
			return false
		}
	}
	return true
}

// stripePaymentSheetTrialCredited reports whether the sheet's trial invoice
// (its latest; a pending sheet has no other) is in the stripe_invoice ledger.
// A sheet with no invoice named is treated as credited: the sweep never
// cancels what it cannot clear.
func stripePaymentSheetTrialCredited(ctx context.Context, sub *stripe.Subscription) bool {
	if sub.LatestInvoice == nil || sub.LatestInvoice.ID == "" {
		return true
	}
	_, credited := stripeInvoiceLedgerNetworkIdFunc(ctx, sub.LatestInvoice.ID)
	return credited
}

// One run of the sweep: the trialing sheets, then the incomplete ones, each
// found by Stripe's search, read again and canceled when still abandoned.
// stripe.Key must be set.
func stripeSweepAbandonedSheets(
	ctx context.Context,
	now time.Time,
	settings *stripeAbandonedSheetSettings,
) (*SweepStripeAbandonedSheetsResult, error) {
	result := &SweepStripeAbandonedSheetsResult{}
	calls := 0
	errorCount := 0
	// spend reports whether the run may make one more call to Stripe
	spend := func() bool {
		if settings.MaxCalls <= calls {
			result.Stopped = "call budget"
			return false
		}
		calls += 1
		return true
	}
	// failed counts a read or cancel Stripe did not answer, and reports
	// whether the run may go on
	failed := func() bool {
		result.FailedCount += 1
		stripeAbandonedSheetsTotal.WithLabelValues(stripeAbandonedSheetOutcomeFailed).Inc()
		errorCount += 1
		if settings.MaxErrors <= errorCount {
			result.Stopped = "error budget"
			return false
		}
		return true
	}
	kept := func() {
		result.KeptCount += 1
		stripeAbandonedSheetsTotal.WithLabelValues(stripeAbandonedSheetOutcomeKept).Inc()
	}
	credited := func() {
		result.CreditedCount += 1
		stripeAbandonedSheetsTotal.WithLabelValues(stripeAbandonedSheetOutcomeCredited).Inc()
	}

	cutoff := now.Add(-stripeAbandonedPaymentSheetAge)
	statuses := []stripe.SubscriptionStatus{
		stripe.SubscriptionStatusTrialing,
		stripe.SubscriptionStatusIncomplete,
	}
	for _, status := range statuses {
		params := &stripe.SubscriptionSearchParams{
			SearchParams: stripe.SearchParams{
				Context: ctx,
				Query: fmt.Sprintf(
					"metadata['%s']:'%s' AND status:'%s' AND created<%d",
					stripeMetadataPaymentSheet,
					stripePaymentSheetPending,
					status,
					cutoff.Unix(),
				),
				Limit: stripe.Int64(stripeAbandonedSheetSearchPageSize),
			},
		}
		iter := subscription.Search(params)
		read := 0
		for result.Stopped == "" {
			// the next search page is a call to Stripe
			if read%stripeAbandonedSheetSearchPageSize == 0 && !spend() {
				break
			}
			if !iter.Next() {
				break
			}
			read += 1
			result.FoundCount += 1
			found := iter.Subscription()
			// the search can lag behind Stripe: what it found is read again
			// before anything is canceled
			if !stripePaymentSheetAbandoned(found, now) {
				kept()
				continue
			}
			if stripePaymentSheetTrialCredited(ctx, found) {
				// its window runs out as it is (the owner, 2026-11-16)
				credited()
				continue
			}

			if !spend() {
				break
			}
			getParams := &stripe.SubscriptionParams{}
			getParams.Context = ctx
			getParams.AddExpand("customer")
			sub, err := subscription.Get(found.ID, getParams)
			if err != nil {
				glog.Warningf("[stripe]abandoned sheet sweep: could not read subscription %s: %s\n", found.ID, err)
				if !failed() {
					break
				}
				continue
			}
			if !stripePaymentSheetAbandoned(sub, now) {
				// a card or an attach in the meantime
				kept()
				continue
			}
			if stripePaymentSheetTrialCredited(ctx, sub) {
				credited()
				continue
			}

			if !spend() {
				break
			}
			cancelParams := &stripe.SubscriptionCancelParams{}
			cancelParams.Context = ctx
			if _, err := subscription.Cancel(sub.ID, cancelParams); err != nil {
				glog.Warningf("[stripe]abandoned sheet sweep: could not cancel subscription %s: %s\n", sub.ID, err)
				if !failed() {
					break
				}
				continue
			}
			result.CanceledCount += 1
			stripeAbandonedSheetsTotal.WithLabelValues(stripeAbandonedSheetOutcomeCanceled).Inc()
			glog.Infof(
				"[stripe]abandoned sheet sweep: canceled abandoned payment sheet subscription %s (%s, no card after %s)\n",
				sub.ID, sub.Status, now.Sub(time.Unix(sub.Created, 0)).Round(time.Minute),
			)
		}
		if err := iter.Err(); err != nil {
			return result, fmt.Errorf("stripe abandoned sheet sweep: search %s sheets: %w", status, err)
		}
		if result.Stopped != "" {
			break
		}
	}
	glog.Infof(
		"[stripe]abandoned sheet sweep: found %d payment sheets pending for over %s; canceled %d, kept %d with a card, kept %d credited before, failed %d%s\n",
		result.FoundCount,
		stripeAbandonedPaymentSheetAge,
		result.CanceledCount,
		result.KeptCount,
		result.CreditedCount,
		result.FailedCount,
		stripeAbandonedSheetStoppedSuffix(result.Stopped),
	)
	return result, nil
}

// The summary line's note of a run that stopped early.
func stripeAbandonedSheetStoppedSuffix(stopped string) string {
	if stopped == "" {
		return ""
	}
	return fmt.Sprintf("; stopped at its %s, the next run carries on", stopped)
}
