// The hourly abandoned payment sheet sweep (owner, 2026-11-16). A payment
// sheet that is opened and left leaves its subscription behind: trialing (the
// yearly plan) or incomplete (the monthly), metadata payment_sheet pending,
// and no card. It never gets Pro, since its held $0 trial invoice is never
// credited, but it sat in Stripe as a trial until the trial ended 14 days
// later, when Stripe canceled it for the missing payment method. That
// inflated Stripe's trial count: 130 of 198 recently cancelled "trials" never
// had a card.
//
// Each run finds the pending sheets through Stripe's search (the server keeps
// no record of a sheet's subscription) and settles each:
//
//	not ours (no network_id metadata)                         -> untouched
//	its trial invoice is in the stripe_invoice ledger         -> credited: kept,
//	                                                             untouched
//	a yearly sheet with a card on its subscription            -> attached or
//	                                                             refused: the
//	                                                             finalization the
//	                                                             webhooks run
//	no card, under stripeAbandonedPaymentSheetAge old         -> left (in progress)
//	no card, older: read again, now attached                  -> kept
//	read again, a card on the subscription                    -> attached or refused
//	otherwise                                                 -> canceled
//
// A card on the customer and none on the subscription is no reason to keep a
// sheet (the owner, 2026-11-16: "Cancel after a day"): a completed sheet
// always saves its card on its subscription, so that sheet was abandoned, and
// left trialing Stripe would charge the customer's default card from an
// earlier purchase at the trial's end, with no trial before it. The cancel
// leaves the customer's card as it is.
//
// A completed sheet whose attach every path missed (the webhooks lost, or run
// before the card was on the subscription, and the app's report never came)
// is attached and credited within the hour, at any age: the search finds the
// sheet itself, where the hourly reconcile sees its trial invoice only while
// it is new, and the sweep runs the same finalization as the webhooks
// (stripeFinalizeTierForCustomer: the card check, the tier, the held credit
// and its records) with the taskworker's own connections.
//
// The ledger rule is the owner's (2026-11-16: "ok just let them run out"):
// the hourly reconcile once credited abandoned sheets' trials, and those
// windows run out as they are. Canceling their subscriptions would let the
// reconcile end them early, so the sweep never touches a sheet whose trial
// was credited. A sheet refused for its card (subscription_stripe_card_check.go)
// waits for a new card like any other and is canceled the same way. A
// customer who opens a new sheet has the old one replaced at once
// (stripeCancelPendingPaymentSheetSubscriptions); the sweep is for the ones
// never come back to. The trial-ending notice skips these subscriptions too
// (stripeTrialEndingNotice): a canceled one gets none.
//
// A run makes at most MaxCalls calls to Stripe (each search page, each read,
// each cancel, about stripeAbandonedSheetFinalizeCalls for each finalization)
// and stops after MaxErrors failures; the next run carries on. A search
// Stripe does not answer fails the run, which the task retries with backoff.
// An env without Stripe credentials (local, test) skips the run and keeps the
// schedule. What it does is counted in
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

// What the sweep counts a finalization as against its call budget: the
// card's country, the customer's subscriptions, the attach, and the held
// invoice's reads and credit.
const stripeAbandonedSheetFinalizeCalls = 6

// The sweep's tunables.
type stripeAbandonedSheetSettings struct {
	// how long after a run the next one runs
	Interval time.Duration
	// the most calls one run makes to Stripe: each search page (up to 100
	// subscriptions), each read before a cancel, each cancel, and
	// stripeAbandonedSheetFinalizeCalls for each finalization
	MaxCalls int
	// the failed reads, finalizations and cancels one run takes before it
	// stops
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

// What became of a sheet the sweep settled (the outcome label).
const (
	// abandoned: canceled at Stripe
	stripeAbandonedSheetOutcomeCanceled = "canceled"
	// a completed sheet whose attach was missed: attached, its trial credited
	stripeAbandonedSheetOutcomeAttached = "attached"
	// a sheet whose card failed its CVC check: refused
	stripeAbandonedSheetOutcomeRefused = "refused"
	// read again before a cancel, it had been attached in the meantime
	stripeAbandonedSheetOutcomeKept = "kept"
	// its trial invoice was credited before: it runs out as it is, found
	// again by every run until its trial ends
	stripeAbandonedSheetOutcomeCredited = "credited"
	// Stripe did not answer a read, a finalization or a cancel; the next run
	// tries again
	stripeAbandonedSheetOutcomeFailed = "failed"
)

// Payment sheets by what the sweep did.
var stripeAbandonedSheetsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "subscription",
	Name:      "stripe_abandoned_payment_sheets_total",
	Help:      "Pending payment sheet subscriptions the hourly sweep settled, by outcome: canceled (no card on the subscription for over a day, whatever card its customer holds); attached (a card on the subscription whose attach every path missed: attached and its trial credited); refused (that card failed its CVC check); kept (read again before a cancel, it had been attached); credited (its trial invoice was credited before, so it runs out as it is, found again every run until its trial ends); failed (Stripe did not answer; the next run tries again)",
}, []string{"outcome"})

// Registers the metric, every series from the start so the first
// cancellation is an increase.
func init() {
	prometheus.MustRegister(stripeAbandonedSheetsTotal)
	for _, outcome := range []string{
		stripeAbandonedSheetOutcomeCanceled,
		stripeAbandonedSheetOutcomeAttached,
		stripeAbandonedSheetOutcomeRefused,
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
	// completed sheets whose missed attach the sweep made
	AttachedCount int `json:"attached_count"`
	// sheets whose card failed its CVC check
	RefusedCount int `json:"refused_count"`
	// attached in the meantime, when read again before a cancel
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

// Cancels the payment sheets abandoned for more than a day, and attaches the
// completed ones whose attach was missed.
func SweepStripeAbandonedSheets(
	_ *SweepStripeAbandonedSheetsArgs,
	clientSession *session.ClientSession,
) (*SweepStripeAbandonedSheetsResult, error) {
	if !stripeReconcileHasCredentials() {
		return &SweepStripeAbandonedSheetsResult{Skip: "no Stripe credentials in this env"}, nil
	}
	stripe.Key = stripeApiToken()
	return stripeSweepAbandonedSheets(clientSession, server.NowUtc(), stripeAbandonedSheetSettingsFunc())
}

// The finalization the sweep runs for a completed sheet. Replaceable only by
// the hermetic tests (it reads the database); production never mutates it.
var stripeAbandonedSheetFinalizeFunc = stripeFinalizeTierForCustomer

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

// stripePaymentSheetAbandoned reports whether a subscription is one of our
// payment sheets left without a card for longer than
// stripeAbandonedPaymentSheetAge before now: still pending, trialing or
// incomplete, created before the cutoff, and no payment method on the
// subscription itself. A card its customer holds from an earlier purchase
// does not count (the owner, 2026-11-16: "Cancel after a day"): a completed
// sheet always saves its card on its subscription.
func stripePaymentSheetAbandoned(sub *stripe.Subscription, now time.Time) bool {
	if sub == nil || sub.Metadata[stripeMetadataPaymentSheet] != stripePaymentSheetPending || sub.Metadata[stripeMetadataNetworkId] == "" {
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
	return sub.DefaultPaymentMethod == nil && sub.DefaultSource == nil
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

// stripePaymentSheetCompleted reports whether a pending yearly sheet has a
// card on its own subscription: completed, waiting only on its attach.
func stripePaymentSheetCompleted(sub *stripe.Subscription) bool {
	return sub.Metadata[stripeMetadataPaymentSheet] == stripePaymentSheetPending &&
		sub.Status == stripe.SubscriptionStatusTrialing &&
		sub.DefaultPaymentMethod != nil && sub.DefaultPaymentMethod.ID != ""
}

// One run of the sweep: every pending trialing sheet (found at any age, to
// attach the completed ones), then the incomplete ones over a day old, each
// found by Stripe's search; an abandoned one is read again and canceled.
// stripe.Key must be set.
func stripeSweepAbandonedSheets(
	clientSession *session.ClientSession,
	now time.Time,
	settings *stripeAbandonedSheetSettings,
) (*SweepStripeAbandonedSheetsResult, error) {
	ctx := clientSession.Ctx
	result := &SweepStripeAbandonedSheetsResult{}
	calls := 0
	errorCount := 0
	// spend reports whether the run may make n more calls to Stripe
	spend := func(n int) bool {
		if settings.MaxCalls < calls+n {
			result.Stopped = "call budget"
			return false
		}
		calls += n
		return true
	}
	// failed counts a step Stripe did not answer, and reports whether the run
	// may go on
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
	count := func(counter *int, outcome string) {
		*counter += 1
		stripeAbandonedSheetsTotal.WithLabelValues(outcome).Inc()
	}
	// finalized customers this run: a customer's finalization settles all of
	// its sheets
	finalized := map[string]*stripeFinalizeResult{}
	// finalize runs the webhooks' finalization for a completed sheet, and
	// reports whether the run may go on
	finalize := func(sub *stripe.Subscription) bool {
		customerId := ""
		if sub.Customer != nil {
			customerId = sub.Customer.ID
		}
		finalizeResult, ok := finalized[customerId]
		if !ok {
			if !spend(stripeAbandonedSheetFinalizeCalls) {
				return false
			}
			country := stripePaymentMethodCountry(sub.DefaultPaymentMethod.ID)
			finalizeResult = stripeAbandonedSheetFinalizeFunc(clientSession, customerId, country)
			finalized[customerId] = finalizeResult
		}
		switch {
		case finalizeResult.Refused[sub.ID]:
			count(&result.RefusedCount, stripeAbandonedSheetOutcomeRefused)
		case finalizeResult.Attached[sub.ID]:
			count(&result.AttachedCount, stripeAbandonedSheetOutcomeAttached)
			glog.Infof("[stripe]abandoned sheet sweep: attached completed payment sheet subscription %s, whose attach was missed\n", sub.ID)
		case finalizeResult.Err != nil:
			glog.Warningf("[stripe]abandoned sheet sweep: could not finalize subscription %s: %s\n", sub.ID, finalizeResult.Err)
			return failed()
		default:
			// attached in the meantime
			count(&result.KeptCount, stripeAbandonedSheetOutcomeKept)
		}
		return true
	}

	cutoff := now.Add(-stripeAbandonedPaymentSheetAge)
	searches := []struct {
		status stripe.SubscriptionStatus
		query  string
	}{
		// every age: a completed sheet is attached however new
		{
			status: stripe.SubscriptionStatusTrialing,
			query: fmt.Sprintf(
				"metadata['%s']:'%s' AND status:'%s'",
				stripeMetadataPaymentSheet, stripePaymentSheetPending, stripe.SubscriptionStatusTrialing,
			),
		},
		// the monthly sheet's, only to cancel the abandoned ones
		{
			status: stripe.SubscriptionStatusIncomplete,
			query: fmt.Sprintf(
				"metadata['%s']:'%s' AND status:'%s' AND created<%d",
				stripeMetadataPaymentSheet, stripePaymentSheetPending, stripe.SubscriptionStatusIncomplete, cutoff.Unix(),
			),
		},
	}
	for _, search := range searches {
		params := &stripe.SubscriptionSearchParams{
			SearchParams: stripe.SearchParams{
				Context: ctx,
				Query:   search.query,
				Limit:   stripe.Int64(stripeAbandonedSheetSearchPageSize),
			},
		}
		iter := subscription.Search(params)
		read := 0
		for result.Stopped == "" {
			// the next search page is a call to Stripe
			if read%stripeAbandonedSheetSearchPageSize == 0 && !spend(1) {
				break
			}
			if !iter.Next() {
				break
			}
			read += 1
			found := iter.Subscription()
			if found.Metadata[stripeMetadataNetworkId] == "" {
				// another product's subscription: never touched
				continue
			}
			result.FoundCount += 1
			if stripePaymentSheetTrialCredited(ctx, found) {
				// its window runs out as it is (the owner, 2026-11-16)
				count(&result.CreditedCount, stripeAbandonedSheetOutcomeCredited)
				continue
			}
			if stripePaymentSheetCompleted(found) {
				if !finalize(found) {
					break
				}
				continue
			}
			// the search can lag behind Stripe: what it found is read again
			// before anything is canceled
			if found.Created <= 0 || cutoff.Before(time.Unix(found.Created, 0)) {
				// under a day old: a customer may still be completing it
				continue
			}
			if !stripePaymentSheetAbandoned(found, now) {
				count(&result.KeptCount, stripeAbandonedSheetOutcomeKept)
				continue
			}

			if !spend(1) {
				break
			}
			// read again just before the cancel: a card that landed on the
			// subscription meanwhile goes the attach path instead
			getParams := &stripe.SubscriptionParams{}
			getParams.Context = ctx
			sub, err := subscription.Get(found.ID, getParams)
			if err != nil {
				glog.Warningf("[stripe]abandoned sheet sweep: could not read subscription %s: %s\n", found.ID, err)
				if !failed() {
					break
				}
				continue
			}
			if stripePaymentSheetTrialCredited(ctx, sub) {
				count(&result.CreditedCount, stripeAbandonedSheetOutcomeCredited)
				continue
			}
			if stripePaymentSheetCompleted(sub) {
				// its card landed in the meantime
				if !finalize(sub) {
					break
				}
				continue
			}
			if !stripePaymentSheetAbandoned(sub, now) {
				// attached in the meantime
				count(&result.KeptCount, stripeAbandonedSheetOutcomeKept)
				continue
			}

			if !spend(1) {
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
			count(&result.CanceledCount, stripeAbandonedSheetOutcomeCanceled)
			glog.Infof(
				"[stripe]abandoned sheet sweep: canceled abandoned payment sheet subscription %s (%s, no card after %s)\n",
				sub.ID, sub.Status, now.Sub(time.Unix(sub.Created, 0)).Round(time.Minute),
			)
		}
		if err := iter.Err(); err != nil {
			return result, fmt.Errorf("stripe abandoned sheet sweep: search %s sheets: %w", search.status, err)
		}
		if result.Stopped != "" {
			break
		}
	}
	glog.Infof(
		"[stripe]abandoned sheet sweep: found %d pending payment sheets; canceled %d abandoned over %s, attached %d completed, refused %d, kept %d, kept %d credited before, failed %d%s\n",
		result.FoundCount,
		result.CanceledCount,
		stripeAbandonedPaymentSheetAge,
		result.AttachedCount,
		result.RefusedCount,
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
