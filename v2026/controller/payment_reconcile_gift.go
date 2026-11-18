package controller

// The Stripe store's gift leg of the hourly payment reconciliation
// (payment_reconcile_controller.go): card gift orders that Stripe says are
// paid and that are still pending here, because the webhook that fulfils them
// (checkout.session.completed, or checkout.session.async_payment_succeeded for
// a delayed payment, gift_checkout_controller.go) was missed, failed or
// disabled. Before it, nothing re-checked a pending card order against
// Stripe: a lost webhook left a paid buyer with no cards.
//
// Each run lists the card orders still pending that were made inside the
// window and whose Checkout session no settled record names
// (model.ListPendingStripeGiftOrdersInConn), and reads each session from
// Stripe with its payment intent's charge
// (expand[]=payment_intent.latest_charge):
//
//	open                                  -> nothing: the buyer may still pay
//	expired                               -> checkout_expired, recorded once; the
//	    session can never be paid, so the order is not read again (it stays
//	    pending, as an abandoned order of every rail does)
//	complete, payment not cleared (unpaid) -> nothing: a delayed method's money
//	    is on its way; the window ends the checks
//	complete and paid, its charge refunded or disputed (not kept whole, as the
//	    client report of a Pro invoice judges it: keptWholePayment)
//	                                      -> refunded or disputed, recorded
//	    once (store stripe, leg gift_checkout); never fulfilled, and not read
//	    again. A dispute of an order with no codes voids nothing
//	    (model.DisputeGiftOrderCodesInTx), so a fulfilment now would make its
//	    codes live mid-dispute; a refund the webhook missed would be given
//	    away. A lost dispute still refunds the order (its webhook)
//	complete and paid, the charge kept whole, the order's total in usd
//	                                      -> fulfilled through the webhook's own
//	    path (stripeFulfillGiftCheckoutInConn): the order, its codes, its
//	    delivery and its sales notice happen once, whichever of the webhook and
//	    this leg reports the session first. Recorded as credited (store stripe,
//	    leg gift_checkout): the webhook was missed
//	complete and paid, another amount or currency
//	                                      -> recorded through the webhook's
//	    path (stripeRecordGiftPaymentMismatch: store gift, credit_unfulfillable,
//	    reason gift_payment_mismatch), never fulfilled, and not read again: an
//	    operator settles it (SIGNALS.md §2.22 payment-gift-mismatch)
//	refunded since it was listed          -> nothing, as the webhook does
//	the session names another order, or a read, the charge or the fulfilment
//	    fails                             -> the order's error, and it is read
//	    again next run
//	Stripe does not know the session      -> the order's error, until it has
//	    persisted through giftCheckoutUnknownToStoreRetryRuns runs; then
//	    unknown_to_store, recorded once and no longer an error (the App Store
//	    leg's bounded retry); the order is still read every run
//
// The window. A Checkout session expires 24 hours after it was made unless it
// completes (Stripe's default and maximum expires_at; the gift checkout sets
// none). A session completed with a delayed payment method is paid when the
// money clears, which Stripe documents as up to 4 business days for an ACH
// Direct Debit (the delayed method a usd session offers), after up to 10 days
// of microdeposit verification when the bank is not linked instantly, and up
// to 14 business days for a SEPA Direct Debit (the longest of Checkout's
// delayed-notification methods; a usd session does not offer it). 21 days
// covers the 24-hour session and 14 business days, about 20 calendar days,
// and ends well before the gift retention removes a pending order's buyer and
// friend addresses at 30 days (gift_delivery.go), after which its cards could
// not be delivered. Orders made in the last 15 minutes (the grace) are left
// to the webhook, which normally arrives within seconds of the payment:
// reading them would only race it and count a webhook that was on time as
// missed.
//
// Bounds. At most giftCheckoutReconcileMaxCalls sessions are read per run,
// each one also a call of the Stripe store's budget; the leg runs before the
// invoice legs, so they cannot spend what it needs. The newest pending orders
// are read first, so a recent payment is reached at the next run even in a
// burst of open sessions (a card-testing wave); when more are pending than the
// cap, giftCheckoutReconcileOldestCalls of it read the oldest, so the oldest
// are reached too. Three reads failing in a row stop the leg until the next
// run instead of reading every order into the same outage. One order's
// failure (Stripe does not know its session, its charge cannot be read, its
// fulfilment fails or raises) is recorded and the next order is read.
//
// Visibility. A fulfilment here is a credited event of store stripe, which
// the payment monitor reports as payment-reconciliation-repair (SIGNALS.md
// §2.21) like a missed invoice.paid, and the run's heartbeat folds the leg's
// counts into the stripe store's details (gift_checkout_*). An order's failure
// is a stripe error row with leg gift_checkout, which the monitor's
// payment-reconciliation-store-error reads, counted in the run's errors but
// never as a store error (recordGiftCheckoutError): an order that keeps
// failing does not hold the invoice listing's watermark, whose overlap would
// otherwise grow until the store's budget ran out before the renewal legs.
// Every read counts in urnetwork_gift_checkout_reconcile_total by result.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// How long after its order was made a pending card order is read from
// Stripe (see the header).
const giftCheckoutReconcileWindow = 21 * 24 * time.Hour

// Orders made this recently are left to the webhook (see the header).
const giftCheckoutReconcileGrace = 15 * time.Minute

// The details' leg of every record the gift leg writes.
const stripeGiftCheckoutLegName = "gift_checkout"

// The most sessions the gift leg reads per run. Replaceable by tests.
var giftCheckoutReconcileMaxCalls = 100

// The reads of giftCheckoutReconcileMaxCalls that go to the oldest pending
// orders when more are pending than the cap; the rest read the newest. The
// orders between wait for a later run. Replaceable by tests.
var giftCheckoutReconcileOldestCalls = 20

// The failed reads in a row that stop the gift leg until the next run.
// Replaceable by tests.
var giftCheckoutReconcileMaxConsecutiveErrors = 3

// The bounded retry of a session Stripe says it does not know, as the App
// Store leg's (appleUnknownToStoreRetryRuns): it is an error, which holds the
// stripe watermark, until it has persisted through this many runs within
// giftCheckoutUnknownToStoreRetryLookback; then unknown_to_store is recorded
// once and the answer is no longer an error. The session is still read every
// run, so a later answer is reconciled as usual. Replaceable by tests.
var giftCheckoutUnknownToStoreRetryRuns = 24

const giftCheckoutUnknownToStoreRetryLookback = 7 * 24 * time.Hour

// The reason of the error events that spend that retry.
const giftCheckoutSessionNotFoundReason = "session_not_found"

// The results of a read of a pending card order's session.
const (
	// paid and still pending: the webhook was missed, and the leg fulfilled it
	giftCheckoutReconcileResultFulfilled = "fulfilled"
	// paid, and the webhook fulfilled it between the listing and the fulfilment
	giftCheckoutReconcileResultAlreadyFulfilled = "already_fulfilled"
	giftCheckoutReconcileResultOpen             = "open"
	// complete, its delayed payment not cleared yet
	giftCheckoutReconcileResultUnpaid = "unpaid"
	// expired unpaid: recorded once, and no longer read
	giftCheckoutReconcileResultExpired = "expired"
	// paid another amount or currency: recorded for an operator, not fulfilled
	giftCheckoutReconcileResultMismatch = "mismatch"
	// refunded since it was listed, or paid with a charge since refunded:
	// nothing fulfilled
	giftCheckoutReconcileResultRefunded = "refunded"
	// paid with a charge since disputed: recorded once, nothing fulfilled
	giftCheckoutReconcileResultDisputed = "disputed"
	// Stripe does not know the session, past the bounded retry: recorded once
	// as unknown_to_store, read again next run, and no longer an error
	giftCheckoutReconcileResultUnknownToStore = "unknown_to_store"
	// Stripe or the fulfilment failed: read again next run
	giftCheckoutReconcileResultError = "error"
)

var giftCheckoutReconcileResultValues = []string{
	giftCheckoutReconcileResultFulfilled,
	giftCheckoutReconcileResultAlreadyFulfilled,
	giftCheckoutReconcileResultOpen,
	giftCheckoutReconcileResultUnpaid,
	giftCheckoutReconcileResultExpired,
	giftCheckoutReconcileResultMismatch,
	giftCheckoutReconcileResultRefunded,
	giftCheckoutReconcileResultDisputed,
	giftCheckoutReconcileResultUnknownToStore,
	giftCheckoutReconcileResultError,
}

// The gift leg's reads by result: a closed label set, so no order, session
// or amount leaves the process. Counted by real runs only, not dry runs.
var giftCheckoutReconcileResults = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "gift",
	Name:      "checkout_reconcile_total",
	Help:      "Pending card gift orders the hourly payment reconciler read from Stripe, by result: fulfilled (paid and still pending, so the webhook was missed; fulfilled through the webhook's path), already_fulfilled, open, unpaid (a delayed payment not cleared yet), expired (recorded; no longer read), mismatch (another amount or currency; recorded for an operator, not fulfilled), refunded (the order, or its charge before fulfilment), disputed (its charge before fulfilment; recorded, not fulfilled), unknown_to_store (Stripe does not know the session, past the bounded retry), error (read again next run)",
}, []string{"result"})

func init() {
	prometheus.MustRegister(giftCheckoutReconcileResults)
	for _, result := range giftCheckoutReconcileResultValues {
		giftCheckoutReconcileResults.WithLabelValues(result)
	}
}

// stripeReconcileFetchCheckoutSession reads one Checkout session with its
// payment intent's charge, GET
// /v1/checkout/sessions/{id}?expand[]=payment_intent.latest_charge, through the
// raw-HTTP seams of every Stripe read of the reconciler (stripeApiBaseUrl,
// stripeAuthHeader): one call, whatever the session's state. found = false
// when Stripe answers that it does not know the session (400, 404); any other
// failure is an error.
func stripeReconcileFetchCheckoutSession(ctx context.Context, sessionId string) (*stripeVerifyCheckoutSession, bool, error) {
	checkoutSession, err := server.HttpGetRequireStatusOk[*stripeVerifyCheckoutSession](
		ctx,
		fmt.Sprintf("%s/v1/checkout/sessions/%s?expand[]=payment_intent.latest_charge", stripeApiBaseUrl, url.PathEscape(sessionId)),
		stripeAuthHeader,
		server.ResponseJsonObject[*stripeVerifyCheckoutSession],
	)
	if err != nil {
		var statusErr *server.HttpStatusError
		if errors.As(err, &statusErr) && (statusErr.StatusCode == http.StatusNotFound || statusErr.StatusCode == http.StatusBadRequest) {
			return nil, false, nil
		}
		return nil, false, err
	}
	if checkoutSession == nil || checkoutSession.Id != sessionId {
		return nil, false, fmt.Errorf("checkout session %s: Stripe returned another or a malformed session", sessionId)
	}
	return checkoutSession, true, nil
}

// stripeCheckoutSessionCharge is the charge that paid a session, from the
// payment intent stripeReconcileFetchCheckoutSession expanded. An error when
// the session names no expanded charge: the payment cannot be judged, so
// nothing is fulfilled from it.
func stripeCheckoutSessionCharge(checkoutSession *stripeVerifyCheckoutSession) (*stripeReconcileCharge, error) {
	intentJson := bytes.TrimSpace(checkoutSession.PaymentIntent)
	if len(intentJson) == 0 || intentJson[0] != '{' {
		return nil, errors.New("the checkout session's payment intent is not expanded")
	}
	var intent struct {
		LatestCharge json.RawMessage `json:"latest_charge"`
	}
	if err := json.Unmarshal(intentJson, &intent); err != nil {
		return nil, fmt.Errorf("the checkout session's payment intent is malformed: %w", err)
	}
	chargeJson := bytes.TrimSpace(intent.LatestCharge)
	if len(chargeJson) == 0 || chargeJson[0] != '{' {
		return nil, errors.New("the checkout session's payment intent names no expanded charge")
	}
	var charge stripeReconcileCharge
	if err := json.Unmarshal(chargeJson, &charge); err != nil || charge.Id == "" {
		return nil, errors.New("the checkout session's charge is malformed")
	}
	return &charge, nil
}

// What a paid session's charge says of its gift order: kept whole, so it may
// be fulfilled (keptWholePayment); refunded or disputed, so it never is; or
// neither yet (a charge that has not succeeded), to read again.
const (
	giftCheckoutChargeKept      = "kept"
	giftCheckoutChargeRefunded  = "refunded"
	giftCheckoutChargeDisputed  = "disputed"
	giftCheckoutChargeUnsettled = "unsettled"
)

func giftCheckoutChargeState(charge *stripeReconcileCharge) string {
	switch {
	case charge.Disputed:
		return giftCheckoutChargeDisputed
	case charge.Refunded || 0 < charge.AmountRefunded:
		return giftCheckoutChargeRefunded
	case charge.keptWholePayment():
		return giftCheckoutChargeKept
	default:
		return giftCheckoutChargeUnsettled
	}
}

// reconcileStripeGiftCheckouts is the gift leg (see the header). complete =
// false only when the Stripe store's budget ran out; the leg's own cap, its
// stop on failing reads and its orders' failures leave the rest for the next
// run without holding the store's watermark, which belongs to the invoice
// listing.
func reconcileStripeGiftCheckouts(run *paymentReconcileRun) (complete bool) {
	ctx := run.clientSession.Ctx
	store := model.SubscriptionMarketStripe
	maxCalls := max(giftCheckoutReconcileMaxCalls, 0)
	createdFrom := run.now.Add(-giftCheckoutReconcileWindow)
	createdBefore := run.now.Add(-giftCheckoutReconcileGrace)
	// newest first, one more than the cap, to know whether orders are left over
	orders := model.ListPendingStripeGiftOrdersInConn(run.connOwner, ctx, createdFrom, createdBefore, true, maxCalls+1)
	if len(orders) == 0 {
		return true
	}

	detail := run.storeDetail(store)
	if maxCalls < len(orders) {
		// more pending than the cap: the newest, then the oldest in their
		// reserve; the orders between wait for a later run
		detail["gift_checkout_calls_exhausted"] = true
		oldestCalls := min(max(giftCheckoutReconcileOldestCalls, 0), maxCalls)
		orders = orders[:maxCalls-oldestCalls]
		listed := map[server.Id]bool{}
		for _, order := range orders {
			listed[order.GiftOrderId] = true
		}
		for _, order := range model.ListPendingStripeGiftOrdersInConn(run.connOwner, ctx, createdFrom, createdBefore, false, oldestCalls) {
			if !listed[order.GiftOrderId] {
				orders = append(orders, order)
			}
		}
	}

	results := map[string]int{}
	calls := 0
	defer func() {
		detail["gift_checkout_calls"] = calls
		for result, count := range results {
			detail["gift_checkout_"+result] = count
		}
	}()

	failedReads := 0
	for _, order := range orders {
		if maxCalls <= calls {
			return true
		}
		if !run.spend(store) {
			return false
		}
		calls += 1
		run.examine(store)
		var result string
		var readFailed bool
		server.HandleError(func() {
			result, readFailed = run.reconcileStripeGiftCheckout(order)
		}, func(err error) {
			// a raise (the database) is this order's failure, never the end
			// of the leg's other orders or of the store's other legs
			run.recordGiftCheckoutError(order.StripeSessionId, map[string]any{
				"leg":        stripeGiftCheckoutLegName,
				"gift_order": order.GiftOrderId.String(),
				"reason":     "raised",
				"error":      err.Error(),
			})
			result, readFailed = giftCheckoutReconcileResultError, false
		})
		if ctx.Err() != nil {
			// the run is over (its task's time ran out): the rest wait
			return true
		}
		results[result] += 1
		if !run.dryRun {
			giftCheckoutReconcileResults.WithLabelValues(result).Inc()
		}
		if !readFailed {
			failedReads = 0
			continue
		}
		failedReads += 1
		if giftCheckoutReconcileMaxConsecutiveErrors <= failedReads {
			// Stripe is failing: wait for the next run rather than read every
			// order into the same failure
			detail["gift_checkout_stopped_on_errors"] = true
			glog.Errorf("[reconcile]stripe: %d checkout session reads failed in a row; the gift leg stops until the next run\n", failedReads)
			return true
		}
	}
	return true
}

// reconcileStripeGiftCheckout reads one pending card order's session and acts
// on it (the header's table), and says what it found. readFailed is a read of
// Stripe that failed, which counts toward the leg's stop; the other failures
// are the order's own. Every failure is the order's error
// (recordGiftCheckoutError), never a store error.
func (self *paymentReconcileRun) reconcileStripeGiftCheckout(
	order *model.PendingStripeGiftOrder,
) (result string, readFailed bool) {
	ctx := self.clientSession.Ctx
	store := model.SubscriptionMarketStripe
	sessionId := order.StripeSessionId
	details := func(more map[string]any) map[string]any {
		values := map[string]any{
			"leg":        stripeGiftCheckoutLegName,
			"gift_order": order.GiftOrderId.String(),
		}
		maps.Copy(values, more)
		return values
	}
	fail := func(reason string, err error) {
		self.recordGiftCheckoutError(sessionId, details(map[string]any{
			"reason": reason,
			"error":  err.Error(),
		}))
	}

	checkoutSession, found, err := stripeReconcileFetchCheckoutSession(ctx, sessionId)
	if err != nil {
		fail("read", err)
		return giftCheckoutReconcileResultError, true
	}
	if !found {
		// the order's own session, which this server made: a key of another
		// account or mode, or a session Stripe lost; for an operator
		if self.giftCheckoutUnknownToStore(order, details) {
			return giftCheckoutReconcileResultUnknownToStore, false
		}
		fail(giftCheckoutSessionNotFoundReason, errors.New("Stripe does not know the checkout session"))
		return giftCheckoutReconcileResultError, false
	}
	if named := checkoutSession.Metadata[giftStripeMetadataOrderId]; named != order.GiftOrderId.String() {
		fail("other_order", fmt.Errorf("the checkout session names gift order %q", named))
		return giftCheckoutReconcileResultError, false
	}

	switch checkoutSession.Status {
	case "open":
		return giftCheckoutReconcileResultOpen, false
	case "expired":
		// never paid, and never payable: not read again
		self.recordGiftCheckoutOnce(store, model.PaymentReconcileActionCheckoutExpired, sessionId, details(nil))
		return giftCheckoutReconcileResultExpired, false
	case "complete":
	default:
		fail("unknown_status", fmt.Errorf("the checkout session's status is %q", checkoutSession.Status))
		return giftCheckoutReconcileResultError, false
	}
	checkoutComplete := checkoutSession.completedObject()
	if !stripeCheckoutSessionPaid(checkoutComplete) {
		// completed with a delayed payment method: paid when its money
		// clears, or never; the window ends the checks
		return giftCheckoutReconcileResultUnpaid, false
	}

	paidDetails := details(map[string]any{
		"amount_total": checkoutComplete.AmountTotal,
		"currency":     checkoutComplete.Currency,
	})
	// paid is not enough: the charge must still hold the money, as the client
	// report of a Pro invoice requires (keptWholePayment). A refund the
	// webhook missed, or a dispute (which voids nothing of an order with no
	// codes), is recorded once and never fulfilled.
	charge, err := stripeCheckoutSessionCharge(checkoutSession)
	if err != nil {
		fail("charge", err)
		return giftCheckoutReconcileResultError, false
	}
	chargeDetails := details(map[string]any{
		"amount_total":           checkoutComplete.AmountTotal,
		"currency":               checkoutComplete.Currency,
		"charge":                 charge.Id,
		"charge_status":          charge.Status,
		"charge_amount_refunded": charge.AmountRefunded,
		"charge_disputed":        charge.Disputed,
	})
	switch giftCheckoutChargeState(charge) {
	case giftCheckoutChargeDisputed:
		self.recordGiftCheckoutOnce(store, model.PaymentReconcileActionDisputed, sessionId, chargeDetails)
		glog.Infof("[reconcile]stripe: checkout %s of gift order %s is paid and its charge %s is disputed: not fulfilled\n", sessionId, order.GiftOrderId, charge.Id)
		return giftCheckoutReconcileResultDisputed, false
	case giftCheckoutChargeRefunded:
		self.recordGiftCheckoutOnce(store, model.PaymentReconcileActionRefunded, sessionId, chargeDetails)
		glog.Infof("[reconcile]stripe: checkout %s of gift order %s is paid and its charge %s is refunded: not fulfilled\n", sessionId, order.GiftOrderId, charge.Id)
		return giftCheckoutReconcileResultRefunded, false
	case giftCheckoutChargeUnsettled:
		fail("charge_not_settled", fmt.Errorf("the checkout session's charge %s is %q", charge.Id, charge.Status))
		return giftCheckoutReconcileResultError, false
	}

	if self.dryRun {
		// what the webhook's path would do, without doing it
		if !giftCheckoutPaysTotal(checkoutComplete, order.TotalCents) {
			self.recordGiftCheckoutOnce(
				model.SubscriptionMarketGift,
				model.PaymentReconcileActionCreditUnfulfillable,
				sessionId,
				giftPaymentMismatchDetails(checkoutComplete, order.GiftOrderId, order.TotalCents, stripeGiftCheckoutLegName),
			)
			return giftCheckoutReconcileResultMismatch, false
		}
		self.record(store, model.PaymentReconcileActionWouldCredit, nil, sessionId, paidDetails)
		return giftCheckoutReconcileResultFulfilled, false
	}

	// the webhook's own path, on the run's session: whichever of the two
	// reports the session first pays the order, the other finds it paid
	outcome, err := stripeFulfillGiftCheckoutInConn(
		self.connOwner,
		ctx,
		checkoutComplete,
		giftCheckoutAudit{runId: self.runId, leg: stripeGiftCheckoutLegName},
	)
	if err != nil {
		fail("fulfil", err)
		return giftCheckoutReconcileResultError, false
	}
	switch outcome {
	case giftCheckoutOutcomeFulfilled:
		self.record(store, model.PaymentReconcileActionCredited, nil, sessionId, paidDetails)
		glog.Infof("[reconcile]stripe: checkout %s paid gift order %s, which was still pending: fulfilled (the webhook was missed)\n", sessionId, order.GiftOrderId)
		return giftCheckoutReconcileResultFulfilled, false
	case giftCheckoutOutcomeAlreadyFulfilled:
		return giftCheckoutReconcileResultAlreadyFulfilled, false
	case giftCheckoutOutcomeRefunded:
		return giftCheckoutReconcileResultRefunded, false
	case giftCheckoutOutcomeMismatch:
		// recorded by the webhook's path, under store gift
		return giftCheckoutReconcileResultMismatch, false
	default:
		fail("not_fulfilled", fmt.Errorf("the fulfilment found no card gift order of the checkout session to fulfil (%s)", outcome))
		return giftCheckoutReconcileResultError, false
	}
}

// giftCheckoutUnknownToStore is the bounded retry of an order whose session
// Stripe says it does not know, as appleUnknownToStore is for an App Store
// renewal: false while the retry lasts (the answer is the order's
// session_not_found error, which the store-error alert reads); on the run
// that spends the last retry, and on every run once the disposition is
// recorded, it records unknown_to_store once, counts the order on the
// heartbeat and returns true, and the answer is no longer an error. Nothing
// is fulfilled from it, and the next run reads the session again.
func (self *paymentReconcileRun) giftCheckoutUnknownToStore(
	order *model.PendingStripeGiftOrder,
	details func(map[string]any) map[string]any,
) bool {
	store := model.SubscriptionMarketStripe
	recorded, errorRuns := model.GetPaymentReconcileUnknownToStoreInConn(
		self.connOwner,
		self.clientSession.Ctx,
		store,
		order.StripeSessionId,
		giftCheckoutSessionNotFoundReason,
		self.now.Add(-giftCheckoutUnknownToStoreRetryLookback),
	)
	// this run's answer is one more attempt
	if !recorded && errorRuns+1 < giftCheckoutUnknownToStoreRetryRuns {
		return false
	}
	self.countUnknownToStore(store)
	if !recorded {
		self.recordGiftCheckoutOnce(
			store,
			model.PaymentReconcileActionUnknownToStore,
			order.StripeSessionId,
			details(map[string]any{"reason": giftCheckoutSessionNotFoundReason}),
		)
	}
	return true
}

// recordGiftCheckoutError records one card gift order's failure: a stripe
// error row (leg gift_checkout, its reason and its order), which the payment
// monitor's payment-reconciliation-store-error reads (SIGNALS.md §2.21),
// counted in the run's errors and the stripe store's gift_checkout_errors but
// never in its Errors. The store's watermark advances on a complete pass with
// no store error (paymentReconcileStoreCanAdvanceWatermark), and it belongs
// to the invoice listing: an order that keeps failing must not hold it, or
// the listing's overlap grows through the order's whole window until the
// store's budget runs out before the renewal legs.
func (self *paymentReconcileRun) recordGiftCheckoutError(evidence string, details map[string]any) {
	store := model.SubscriptionMarketStripe
	self.errors += 1
	summary := self.storeResult(store)
	summary.GiftCheckoutErrors += 1
	self.storeDetail(store)["gift_checkout_errors"] = summary.GiftCheckoutErrors
	if err := addPaymentReconciliationEvent(self.connOwner, self.clientSession.Ctx, &model.PaymentReconciliationEvent{
		RunId:    self.runId,
		Store:    store,
		Action:   model.PaymentReconcileActionError,
		Evidence: evidence,
		Details:  details,
		DryRun:   self.dryRun,
	}); err != nil {
		glog.Errorf("[reconcile]could not record %s/%s event: %s\n", store, model.PaymentReconcileActionError, err)
	}
}

// recordGiftCheckoutOnce writes a card gift order's disposition once per
// session, as recordOnce does (a dry run writes its dry-run row), except that
// a write that fails is the order's error (recordGiftCheckoutError), never
// the store's: the next run reads the order and writes it.
func (self *paymentReconcileRun) recordGiftCheckoutOnce(store string, action string, evidence string, details map[string]any) {
	if self.dryRun {
		self.record(store, action, nil, evidence, details)
		return
	}
	if _, err := addPaymentReconciliationEventOnce(self.connOwner, self.clientSession.Ctx, &model.PaymentReconciliationEvent{
		RunId:    self.runId,
		Store:    store,
		Action:   action,
		Evidence: evidence,
		Details:  details,
	}); err != nil {
		glog.Errorf("[reconcile]could not record %s/%s event: %s\n", store, action, err)
		self.recordGiftCheckoutError(evidence, map[string]any{
			"leg":    stripeGiftCheckoutLegName,
			"reason": "record",
			"action": action,
			"error":  err.Error(),
		})
	}
}
