package controller

// When a Stripe checkout's payment has cleared, so what it bought can be
// delivered (owner, 2026-11-13).
//
// Checkout completes a session as soon as the customer finishes it. With a
// card the money is in by then (payment_status paid). With a delayed method,
// a bank debit or transfer (ACH, SEPA, pay by bank), the session completes
// unpaid and the money arrives days later, or never: 35 of 45 data-pack
// checkouts offered such a method. checkout.session.completed used to fulfil
// every session it was sent, so a data pack paid by a debit that later failed
// was delivered for free. Now:
//
//   - a session is fulfilled only once it is paid: on checkout.session.completed
//     for a payment that cleared at once, and on
//     checkout.session.async_payment_succeeded for a delayed one, both through
//     stripeHandleCheckoutSessionCompleted, whose balance-code ledger (one
//     purchase event id per line) fulfils each line once, whichever of them,
//     and the client's report (VerifyStripeCheckout), arrives first;
//   - checkout.session.async_payment_failed fulfils nothing and records the
//     failure once per session for support, as a payment_failed
//     payment_reconciliation_event.
//
// A Pro checkout fulfils nothing here either way: its subscription credits on
// invoice.paid, which Stripe sends only for a paid invoice.

import (
	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// The payment status of a checkout session whose money is in. A session can
// also be unpaid (a delayed payment on its way) or no_payment_required (a
// trial, or a total of zero).
const stripeCheckoutPaymentStatusPaid = "paid"

// Whether the session's payment cleared, so what it bought can be delivered.
func stripeCheckoutSessionPaid(checkoutComplete *StripeEventCheckoutCompleteObject) bool {
	return checkoutComplete.PaymentStatus == stripeCheckoutPaymentStatusPaid
}

// The durable record of a checkout's failed delayed payment. Replaceable only
// by hermetic tests (no database in that env); production never mutates it.
var stripeRecordCheckoutPaymentFailed = model.AddPaymentReconciliationEventOnce

// checkout.session.async_payment_failed: the delayed payment of a completed
// checkout failed. Nothing is fulfilled; the failure is recorded once per
// session (PaymentReconcileActionPaymentFailed) with what support needs to
// answer the customer: the session, its network when it names one, the amount
// and the customer's email. Answers 200 once the record is durable; a record
// that cannot be written fails the delivery, so Stripe delivers it again.
func stripeHandleCheckoutSessionAsyncPaymentFailed(
	checkout *StripeEventCheckoutCompleteObject,
	clientSession *session.ClientSession,
) (*StripeWebhookResult, error) {
	if checkout.Id == "" {
		glog.Infof("[sub]checkout.session.async_payment_failed with no session id; ignoring\n")
		return &StripeWebhookResult{}, nil
	}
	var networkId *server.Id
	if checkout.ClientReferenceId != "" {
		if id, err := server.ParseId(checkout.ClientReferenceId); err == nil {
			networkId = &id
		}
	}
	customerEmail := ""
	if checkout.CustomerDetails != nil {
		customerEmail = checkout.CustomerDetails.Email
	}
	added, err := stripeRecordCheckoutPaymentFailed(clientSession.Ctx, &model.PaymentReconciliationEvent{
		RunId:     server.NewId(),
		Store:     model.SubscriptionMarketStripe,
		NetworkId: networkId,
		Action:    model.PaymentReconcileActionPaymentFailed,
		Evidence:  checkout.Id,
		Details: map[string]any{
			"reason":         "async_payment_failed",
			"leg":            "webhook",
			"mode":           checkout.Mode,
			"payment_status": checkout.PaymentStatus,
			"amount_total":   checkout.AmountTotal,
			"subscription":   checkout.Subscription,
			// for support only; it never picks an account
			"customer_email": customerEmail,
		},
	})
	if err != nil {
		glog.Errorf("[sub]checkout %s: could not record its failed delayed payment: %s\n", checkout.Id, err)
		return nil, err
	}
	if added {
		glog.Infof("[sub]checkout %s (network %v): its delayed payment failed; nothing was fulfilled\n", checkout.Id, networkId)
	}
	return &StripeWebhookResult{}, nil
}
