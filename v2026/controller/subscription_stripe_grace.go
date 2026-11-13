package controller

// The grace a Stripe Pro window runs past the period its invoice billed (owner,
// 2026-11-13). A window whose invoice took payment keeps Pro through the
// manual-payment grace, in which a failed renewal can still be paid by hand,
// and keeps it after Stripe gives up on the renewal. A window that no payment
// paid for (the yearly plan's 14-day free trial, an invoice discounted to
// nothing) gets the day App Store and Play trials get, and ends when Stripe
// cancels its subscription for non-payment:
//
//   - the credit (stripeResolveInvoiceCredit) writes each window's end from its
//     own invoice's payment;
//   - the hourly reconciler (reconcileStripe) keeps the manual-payment grace
//     through unpaid and a payment_failed cancel only for a window whose invoice
//     took payment, as it keeps a requested cancel's paid period
//     (stripeCancelLeavesPaidPeriod). It ends a window no payment paid for once
//     its subscription is over, or, while its first charge is retried, a day
//     after its period. That also brings the windows credited before this rule,
//     whose free periods carried the 30-day grace, to it.
//
// Whether a window was paid for is read from the invoice that credited it, not
// from the network's history: a network that paid on another subscription or
// rail before gets the same day after a trial no payment paid for, and keeps
// Pro through that other rail's own window while it lasts.

import (
	"time"
)

// Stripe's cancellation_details.reason when Stripe canceled a subscription
// whose payment failed through every retry (the final action set in the
// dashboard).
const stripeCancellationReasonPaymentFailed = "payment_failed"

// The grace's tunables.
type stripeGraceSettings struct {
	// How long the window of an invoice that took payment runs past the period
	// it paid for: the manual-payment grace, in which a failed renewal can still
	// be paid from its hosted invoice or Manage Subscription without a gap in
	// Pro. Kept while Stripe retries the renewal and after Stripe gives up on it
	// (unpaid, or canceled for payment_failed). 30 days, as the other rails paid
	// by hand (manualPaymentGracePeriod). Non-positive ends the window with its
	// period.
	PaidPeriodGrace time.Duration
	// How long the window of an invoice that took no payment runs past its
	// period: the yearly plan's free trial, or an invoice discounted to nothing.
	// A day, as App Store and Play trials (SubscriptionGracePeriod), so a trial
	// whose first charge fails stays Pro for a day at most. Non-positive ends the
	// window with its period.
	FreePeriodGrace time.Duration
}

// A new instance on every call.
func defaultStripeGraceSettings() *stripeGraceSettings {
	return &stripeGraceSettings{
		PaidPeriodGrace: manualPaymentGracePeriod,
		FreePeriodGrace: SubscriptionGracePeriod,
	}
}

// The settings the credit and the reconciler run with. Replaceable only by
// hermetic tests; production never mutates it.
var stripeGraceSettingsFunc = defaultStripeGraceSettings

// The grace past its period of a window whose invoice took payment, or took
// none.
func (self *stripeGraceSettings) periodGrace(tookPayment bool) time.Duration {
	grace := self.FreePeriodGrace
	if tookPayment {
		grace = self.PaidPeriodGrace
	}
	return max(0, grace)
}

// Whether a Stripe invoice took payment for the period it bills: it is paid,
// with money (amount_paid, in the currency's minor unit). A trial's $0 invoice
// and an invoice discounted to nothing took none, and neither did an invoice
// settled entirely from the customer's Stripe credit balance.
func stripeInvoiceTookPayment(status string, amountPaid int64) bool {
	return status == "paid" && 0 < amountPaid
}
