package controller

// The trial card check (owner, 2026-11-16): "Check the card properly when it's
// attached: reject a failed CVC and allow 3D Secure while the customer is
// still there, instead of finding out 14 days later."
//
// A Stripe trial (the yearly plan's 14 days, through the payment sheet or
// Checkout) saves its card with a $0 SetupIntent. Stripe accepts a card whose
// CVC check failed on a $0 setup, and nothing is charged until the trial ends,
// off-session, after the customer has left. On the production data of
// 2026-11-16 about a third of real trials then failed to charge, some blocked
// by Radar as incorrect_cvc: cards whose CVC check had failed at setup.
//
// So the card is checked where the server first accepts it for a trial:
//
//   - the payment sheet, before its pending subscription is marked attached
//     (stripeFinalizeTierForCustomer, which setup_intent.succeeded,
//     payment_method.attached, customer.updated, the app's report and the
//     abandoned-sheet sweep run). Only the card on the sheet's own
//     subscription is judged: a returning customer's older default, judged
//     before the new card lands, would refuse a good card;
//   - Checkout, before its $0 trial invoice is credited
//     (stripeInvoiceTrialCardGate, which invoice.paid, the report and the
//     hourly reconcile run). Only the trial's first invoice is judged.
//
// A card whose card.checks.cvc_check is "fail" is refused. The trial is not
// credited, so no Pro. The card is unset as the subscription's default and
// detached from the customer, so it is never charged. The subscription is
// stamped card_check=cvc_failed, which every later webhook, report and
// reconcile reads as the same refusal, whichever saw the card first; the
// refusal is counted and logged once (stripeRefuseTrialCard). The payment
// sheet's subscription stays pending: the app asks for a new sheet, which
// replaces it (stripeCancelPendingPaymentSheetSubscriptions; a refused sheet
// never blocks a new purchase), or the abandoned-sheet sweep cancels it a day
// later. A card that passes on a refused sheet's subscription clears the
// refusal and starts the trial. A Checkout trial has no sheet to try again
// in, so its subscription is canceled at once and the buyer starts a new
// checkout. The report answers the refusal as status invalid with error
// incorrect_cvc (verifyStripeCardRefused).
//
// pass, unavailable and unchecked proceed as before: many cards and regions
// return no CVC result, and those are never blocked. Nothing is refused,
// stamped, detached or canceled before the subscription is known to be ours
// (network_id) and its trial known not to be credited: an invoice already in
// the stripe_invoice ledger, and the window it credited, is never touched
// (the owner, 2026-11-16, on the windows the hourly reconcile had credited to
// abandoned sheets: "ok just let them run out").
//
// 3D Secure: the trial's SetupIntent (the subscription's pending_setup_intent,
// or Checkout's) is usage off_session, so an authentication the bank asks for
// at setup covers the charge at the trial's end. request_three_d_secure is
// left at Stripe's default, automatic: Stripe challenges when the bank
// requires it, while the customer is still in the sheet. Nothing here sets or
// suppresses it.

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stripe/stripe-go/v82"
	"github.com/stripe/stripe-go/v82/customer"
	"github.com/stripe/stripe-go/v82/paymentmethod"
	"github.com/stripe/stripe-go/v82/subscription"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// Subscription metadata: the trial's card was refused when it was attached.
const (
	stripeMetadataCardCheck  = "card_check"
	stripeCardCheckCvcFailed = "cvc_failed"
)

// What the purchase report answers a refused trial with, beside
// PurchaseErrorCodeIncorrectCvc.
const purchaseIncorrectCvcMessage = "Your card's security code (CVC) didn't match. Check the card details and try again."

// The trial flows a card is checked in (the flow label).
const (
	stripeCardCheckFlowPaymentSheet = "payment_sheet"
	stripeCardCheckFlowCheckout     = "checkout"
)

// Trials refused for their card, by flow.
var stripeCardCheckRefusalsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "subscription",
	Name:      "stripe_card_check_refusals_total",
	Help:      "Stripe trials refused when their card was attached because the card's CVC check failed, by flow (payment_sheet, checkout): no Pro, and the card is detached so it is never charged",
}, []string{"flow"})

// Registers the metric, each flow's series from the start so the first
// refusal is an increase.
func init() {
	prometheus.MustRegister(stripeCardCheckRefusalsTotal)
	for _, flow := range []string{stripeCardCheckFlowPaymentSheet, stripeCardCheckFlowCheckout} {
		stripeCardCheckRefusalsTotal.WithLabelValues(flow)
	}
}

// The stripe_invoice ledger read before a trial's card is refused: an invoice
// already credited is never refused, so the window it credited stays.
// Replaceable only by the hermetic tests; production never mutates it.
var stripeInvoiceLedgerNetworkIdFunc = model.GetStripeInvoiceNetworkId

// The refusal the purchase report answers for a trial refused for its card.
func stripeCardCheckRefusalError() *OnboardingError {
	return &OnboardingError{
		Code:    PurchaseErrorCodeIncorrectCvc,
		Message: purchaseIncorrectCvcMessage,
	}
}

// stripeCardCvcFailed reports whether a card's CVC check failed at Stripe.
// pass, unavailable, unchecked, no check at all and a payment method that is
// not a card are not a failure.
func stripeCardCvcFailed(paymentMethod *stripe.PaymentMethod) bool {
	return paymentMethod != nil &&
		paymentMethod.Card != nil &&
		paymentMethod.Card.Checks != nil &&
		paymentMethod.Card.Checks.CVCCheck == stripe.PaymentMethodCardChecksCVCCheckFail
}

// stripeCardCheckRefused reports whether the subscription carries the stamp of
// a trial refused for its card.
func stripeCardCheckRefused(sub *stripe.Subscription) bool {
	return sub != nil && sub.Metadata[stripeMetadataCardCheck] == stripeCardCheckCvcFailed
}

// stripeCardCheckFlow is the trial flow a subscription came from: the payment
// sheet stamps payment_sheet on its subscriptions, Checkout does not.
func stripeCardCheckFlow(sub *stripe.Subscription) string {
	if sub.Metadata[stripeMetadataPaymentSheet] != "" {
		return stripeCardCheckFlowPaymentSheet
	}
	return stripeCardCheckFlowCheckout
}

// stripeSubscriptionInTrial reports whether the subscription is in its trial,
// so its card has not been charged yet.
func stripeSubscriptionInTrial(sub *stripe.Subscription, now time.Time) bool {
	return sub != nil &&
		sub.Status == stripe.SubscriptionStatusTrialing &&
		0 < sub.TrialEnd &&
		now.Before(time.Unix(sub.TrialEnd, 0))
}

// The HTTP status of a Stripe api error; 0 for any other error.
func stripeErrorStatus(err error) int {
	var stripeErr *stripe.Error
	if errors.As(err, &stripeErr) {
		return stripeErr.HTTPStatusCode
	}
	return 0
}

// stripeSubscriptionCard is the card the subscription would charge: its own
// default payment method, read from Stripe when only its id is at hand, and,
// with customerFallback, its customer's invoice default when it has none of
// its own. A payment sheet's trial is judged by its own card only: the sheet
// puts the card on the subscription, and a customer's older default judged
// before the new card lands would refuse a good card. Checkout collects its
// card before it makes the subscription, so for its trial the customer's
// default is what Stripe would charge when the subscription names none. nil
// when there is none, or Stripe no longer knows it. stripe.Key must be set.
func stripeSubscriptionCard(sub *stripe.Subscription, customerFallback bool) (*stripe.PaymentMethod, error) {
	if sub == nil {
		return nil, nil
	}
	paymentMethod := sub.DefaultPaymentMethod
	if paymentMethod == nil || paymentMethod.ID == "" {
		if sub.DefaultSource != nil || !customerFallback {
			// none of its own (a legacy source is not a payment method: no check
			// to read)
			return nil, nil
		}
		customerCard, err := stripeCustomerDefaultCard(sub)
		if err != nil || customerCard == nil {
			return nil, err
		}
		paymentMethod = customerCard
	}
	if paymentMethod.Type != "" {
		// expanded: the object is all there
		return paymentMethod, nil
	}
	read, err := paymentmethod.Get(paymentMethod.ID, nil)
	if err != nil {
		if stripeErrorStatus(err) == http.StatusNotFound {
			return nil, nil
		}
		return nil, fmt.Errorf("read the card of subscription %s: %w", sub.ID, err)
	}
	return read, nil
}

// stripeCustomerDefaultCard is the invoice default payment method of the
// subscription's customer, nil for none. The customer is read from Stripe,
// with that payment method expanded, unless the subscription carries it
// expanded already. stripe.Key must be set.
func stripeCustomerDefaultCard(sub *stripe.Subscription) (*stripe.PaymentMethod, error) {
	if sub.Customer == nil || sub.Customer.ID == "" {
		return nil, nil
	}
	owner := sub.Customer
	if owner.InvoiceSettings == nil {
		// only the id: read it
		params := &stripe.CustomerParams{}
		params.AddExpand("invoice_settings.default_payment_method")
		read, err := customer.Get(sub.Customer.ID, params)
		if err != nil {
			if stripeErrorStatus(err) == http.StatusNotFound {
				return nil, nil
			}
			return nil, fmt.Errorf("read the customer of subscription %s: %w", sub.ID, err)
		}
		owner = read
	}
	if owner.InvoiceSettings == nil || owner.InvoiceSettings.DefaultPaymentMethod == nil || owner.InvoiceSettings.DefaultPaymentMethod.ID == "" {
		return nil, nil
	}
	return owner.InvoiceSettings.DefaultPaymentMethod, nil
}

// The idempotency key of a card's refusal on a subscription: the calls that
// refuse the same card at once send the same update under it.
func stripeCardCheckRefusalKey(subscriptionId string, paymentMethodId string) string {
	return fmt.Sprintf("card-check-refusal-%s-%s", subscriptionId, paymentMethodId)
}

// The header Stripe sets on the answer it replays for an idempotency key it
// has applied before.
const stripeIdempotentReplayedHeader = "Idempotent-Replayed"

// stripeRefuseTrialCard refuses the card a trial was to start on. One update
// stamps the subscription card_check=cvc_failed and unsets the card as its
// default; once the stamp is on, nothing credits the trial, so it is written
// first and a later step that fails is finished by the next pass. Then the
// card is detached from the customer, and a Checkout trial is canceled.
//
// The update carries the idempotency key of the subscription and the card, so
// of the calls that refuse the same card at once (the webhook and the app's
// report land together), Stripe applies one: that call counts, logs and goes
// on, and the others get Stripe's replay, or its conflict while the first is
// in flight, and stop there. stripe.Key must be set.
func stripeRefuseTrialCard(sub *stripe.Subscription, card *stripe.PaymentMethod) error {
	flow := stripeCardCheckFlow(sub)
	params := &stripe.SubscriptionParams{
		Metadata: map[string]string{stripeMetadataCardCheck: stripeCardCheckCvcFailed},
	}
	params.IdempotencyKey = stripe.String(stripeCardCheckRefusalKey(sub.ID, card.ID))
	if sub.DefaultPaymentMethod != nil && sub.DefaultPaymentMethod.ID == card.ID {
		// an empty value unsets it: the subscription keeps no card to charge
		params.DefaultPaymentMethod = stripe.String("")
	}
	updated, err := subscription.Update(sub.ID, params)
	if err != nil {
		var stripeErr *stripe.Error
		if errors.As(err, &stripeErr) && stripeErr.Code == stripe.ErrorCodeIdempotencyKeyInUse {
			// another call is refusing this card now
			return nil
		}
		return fmt.Errorf("stamp the refused card on subscription %s: %w", sub.ID, err)
	}
	if updated != nil && updated.LastResponse != nil && updated.LastResponse.Header.Get(stripeIdempotentReplayedHeader) == "true" {
		// another call refused this card already
		return nil
	}
	stripeCardCheckRefusalsTotal.WithLabelValues(flow).Inc()
	glog.Infof(
		"[stripe]card check: refused the %s trial of subscription %s for network %s: the card's CVC check failed; no trial, and the card is detached\n",
		flow, sub.ID, sub.Metadata[stripeMetadataNetworkId],
	)
	stripeDetachRefusedCard(sub.ID, card.ID)
	if flow == stripeCardCheckFlowCheckout {
		return stripeCancelRefusedTrial(sub)
	}
	return nil
}

// stripeDetachRefusedCard detaches a refused card from its customer, so no
// subscription or invoice of the customer can charge it. Best effort: the
// refusal stands either way, and the subscription no longer names the card.
// stripe.Key must be set.
func stripeDetachRefusedCard(subscriptionId string, paymentMethodId string) {
	if _, err := paymentmethod.Detach(paymentMethodId, nil); err != nil {
		glog.Warningf("[stripe]card check: could not detach the refused card of subscription %s: %s\n", subscriptionId, err)
	}
}

// stripeCancelRefusedTrial cancels a Checkout trial refused for its card: it
// has no sheet to try again in, and left trialing without a card it would try
// to charge at the trial's end. A subscription already ended is left alone.
// stripe.Key must be set.
func stripeCancelRefusedTrial(sub *stripe.Subscription) error {
	switch sub.Status {
	case stripe.SubscriptionStatusCanceled, stripe.SubscriptionStatusIncompleteExpired:
		return nil
	}
	if _, err := subscription.Cancel(sub.ID, nil); err != nil {
		return fmt.Errorf("cancel the refused trial %s: %w", sub.ID, err)
	}
	glog.Infof("[stripe]card check: canceled the refused checkout trial %s\n", sub.ID)
	return nil
}

// What stripeInvoiceTrialCardGate decided about an invoice.
type stripeTrialCardDecision int

const (
	// credit it as before
	stripeTrialCardCredit stripeTrialCardDecision = iota
	// the trial invoice of a payment sheet still waiting on its card: credited
	// once the card is attached (stripeFinalizeTierForCustomer)
	stripeTrialCardHeld
	// the trial's card was refused: never credited
	stripeTrialCardRefused
)

// The billing reason of a subscription's first invoice: a trial's $0 invoice
// that starts it.
const stripeInvoiceBillingReasonSubscriptionCreate = "subscription_create"

// stripeInvoiceTrialCardGate decides whether an invoice of sub may be credited
// now. Only a $0 invoice is gated: a paid one charged its card. Nothing is
// written to Stripe before the subscription is known to be ours and the
// invoice known not to be credited.
//
//	no network_id metadata: not a subscription of ours  -> credit, untouched
//	a payment sheet still pending                       -> held (refused when
//	                                                         its card was)
//	a payment sheet attached (its card checked then)    -> credit
//	the invoice is already credited (credited())        -> credit (the ledger
//	                                                         absorbs it; its
//	                                                         window is never
//	                                                         touched)
//	the subscription carries a refused card's stamp      -> refused (a Checkout
//	                                                         trial whose cancel
//	                                                         failed is canceled)
//	not in its trial, or not its first invoice          -> credit (a later $0
//	                                                         invoice never refuses)
//	a trial with no card, or a card that did not fail   -> credit
//	a trial whose card failed its CVC check             -> refused now (dryRun:
//	                                                         decided, nothing done)
//
// An error means the trial could not be judged: credit nothing now and try
// again. A refused decision with an error is still a refusal. stripe.Key must
// be set.
func stripeInvoiceTrialCardGate(
	sub *stripe.Subscription,
	invoice *StripeEventInvoiceObject,
	now time.Time,
	dryRun bool,
	credited func() bool,
) (stripeTrialCardDecision, error) {
	if sub == nil || invoice.Total != 0 {
		return stripeTrialCardCredit, nil
	}
	if sub.Metadata[stripeMetadataNetworkId] == "" {
		// another product's subscription in the account: never touched
		return stripeTrialCardCredit, nil
	}
	refused := stripeCardCheckRefused(sub)
	switch sub.Metadata[stripeMetadataPaymentSheet] {
	case stripePaymentSheetPending:
		if refused {
			return stripeTrialCardRefused, nil
		}
		return stripeTrialCardHeld, nil
	case stripePaymentSheetAttached:
		return stripeTrialCardCredit, nil
	}
	if credited() {
		return stripeTrialCardCredit, nil
	}
	if refused {
		if !dryRun {
			if err := stripeCancelRefusedTrial(sub); err != nil {
				return stripeTrialCardRefused, err
			}
		}
		return stripeTrialCardRefused, nil
	}
	if !stripeSubscriptionInTrial(sub, now) || invoice.BillingReason != stripeInvoiceBillingReasonSubscriptionCreate {
		return stripeTrialCardCredit, nil
	}
	card, err := stripeSubscriptionCard(sub, true)
	if err != nil {
		return stripeTrialCardCredit, err
	}
	if !stripeCardCvcFailed(card) {
		return stripeTrialCardCredit, nil
	}
	if dryRun {
		return stripeTrialCardRefused, nil
	}
	return stripeTrialCardRefused, stripeRefuseTrialCard(sub, card)
}

// stripeReconcileTrialInvoiceMaxAge is how long after its creation the hourly
// reconcile still looks at a $0 invoice its ledger does not hold. A trial's
// invoice is paid when its subscription is made; within a day its webhook,
// Stripe's redeliveries and the buyer's report have had their chance, and a
// payment sheet's trial is the abandoned-sheet sweep's after a day (it
// attaches the sheets with a card and cancels the rest). Held invoices are
// never settled into the ledger, so this bounds what a run spends on them to
// the last day's, even when the watermark is stuck.
const stripeReconcileTrialInvoiceMaxAge = stripeAbandonedPaymentSheetAge

// stripeReconcileTrialInvoiceTooOld reports whether the reconcile leaves a $0
// invoice alone for its age. An invoice that names no creation time is looked
// at.
func stripeReconcileTrialInvoiceTooOld(invoice *StripeEventInvoiceObject, now time.Time) bool {
	return invoice.Total == 0 && 0 < invoice.Created &&
		time.Unix(invoice.Created, 0).Before(now.Add(-stripeReconcileTrialInvoiceMaxAge))
}

// stripeReconcileTrialInvoiceHeld is the hourly reconcile's gate for a $0
// invoice the stripe_invoice ledger does not hold: true when invoice.paid
// would not credit it now (a payment sheet waiting on its card, a trial
// refused for its card). A dry run decides without refusing anything. It reads
// and writes Stripe only, never the database: the reconcile holds its one
// connection for the whole run. An error means credit nothing now; the next
// run tries again.
func stripeReconcileTrialInvoiceHeld(
	invoice *StripeEventInvoiceObject,
	clientSession *session.ClientSession,
	now time.Time,
	dryRun bool,
) (bool, error) {
	sub, err := stripeSubscriptionForInvoice(clientSession.Ctx, invoice.Id)
	if err != nil {
		return false, err
	}
	decision, err := stripeInvoiceTrialCardGate(sub, invoice, now, dryRun, func() bool {
		// the reconcile read the ledger before
		return false
	})
	return decision != stripeTrialCardCredit, err
}

// The purchase report's answer for a trial refused for its card: invalid,
// which every client already treats as terminal, with the refusal to show.
// The buyer tries again with a new payment sheet or a new checkout.
func verifyStripeCardRefused() *VerifyStorePurchaseResult {
	return &VerifyStorePurchaseResult{
		Status: VerifyPurchaseStatusInvalid,
		Error:  stripeCardCheckRefusalError(),
	}
}
