package controller

// Root-cause tests of the confirmation reminders (owner, 2026-11-13). A
// renewal whose payment waits on the customer to confirm it with their bank (3D
// Secure) got no email: invoice.payment_action_required fell through
// StripeWebhook to "ignore and answer 200", and invoice.payment_failed of such
// an invoice, at attempt_count 0, was skipped as "no failed attempt". Two trial
// conversions in the 30 days to 2026-11-13 lapsed that way. These tests drive
// the public webhook against the fake Stripe API of
// subscription_stripe_payment_failed_db_test.go, so they read the same before
// and after the change. Needs the test database and redis
// (server.DefaultTestEnv).

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// Delivers an invoice event of the renewal invoice, as Stripe would.
func deliverStripeInvoiceTestEvent(t testing.TB, clientSession *session.ClientSession, eventType string, invoiceId string) {
	result, err := StripeWebhook(stripeWebhookEvent(t, eventType, map[string]any{
		"id":             invoiceId,
		"object":         "invoice",
		"billing_reason": "subscription_cycle",
		"status":         "open",
	}), clientSession)
	if err != nil || result == nil {
		t.Fatalf("%s for %s: %+v %v", eventType, invoiceId, result, err)
	}
}

// A renewal waiting on the customer's bank: a past-due subscription whose open
// renewal invoice has no failed attempt yet (attempt_count 0), the next attempt
// in three days, and a payment intent in `paymentIntentStatus`.
func setStripeRenewalAwaitingTheBank(
	env *stripePaymentFailedTestEnv,
	networkId server.Id,
	subscriptionId string,
	invoiceId string,
	paymentIntentStatus string,
) {
	created := server.NowUtc().Add(-time.Hour).Truncate(time.Second)
	next := created.Add(3 * 24 * time.Hour)
	env.setSubscription(subscriptionId, "cus_"+subscriptionId, &networkId, "past_due")
	env.setInvoice(invoiceId, subscriptionId, "cus_"+subscriptionId, "open", 0, created, &next, 4000)
	env.setInvoicePaymentIntentStatus(invoiceId, paymentIntentStatus)
}

// The notice asks for the confirmation, on the invoice's Stripe-hosted page,
// and does not say the payment was declined.
func requireStripeConfirmNotice(t testing.TB, notice outboxTestSend, invoiceId string) {
	subject, bodyHtml, bodyText, err := RenderEmailTemplate(notice.template)
	if err != nil {
		t.Fatal(err)
	}
	if subject != "Confirm your URnetwork payment" {
		t.Fatalf("the notice of a payment waiting on the bank has subject %q", subject)
	}
	hostedInvoiceUrl := "https://invoice.stripe.com/i/acct_synthetic/" + invoiceId
	if !strings.Contains(bodyText, "Confirm the payment: "+hostedInvoiceUrl) || !strings.Contains(bodyHtml, `href="`+hostedInvoiceUrl+`"`) {
		t.Fatalf("the notice must link the invoice's hosted page to confirm the payment:\n%s", bodyText)
	}
	if strings.Contains(bodyText, "didn't go through") || strings.Contains(subject, "didn't go through") {
		t.Fatalf("a payment waiting on the bank was not declined; the notice says it was:\n%s", bodyText)
	}
}

// invoice.payment_action_required of a renewal whose first charge waits on the
// bank asks the account, once, to confirm the payment; the
// invoice.payment_failed Stripe sends for the same attempt, and redeliveries,
// add nothing. Before, the event was ignored and nothing was sent.
func TestStripeRenewalWaitingOnTheBankAsksTheAccountToConfirm(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		env := newStripePaymentFailedTestEnv(t)
		const email = "payment-confirm@synthetic.example"
		networkId, clientSession := newStripePaymentFailedTestNetwork(t, ctx, "paymentconfirm", email)
		setStripeRenewalAwaitingTheBank(env, networkId, "sub_synthetic_confirm", "in_synthetic_confirm", "requires_action")

		sender := newOutboxTestSender()
		deliverStripeInvoiceTestEvent(t, clientSession, "invoice.payment_action_required", "in_synthetic_confirm")
		notices := deliverStripePaymentFailedTestNotices(ctx, sender, email)
		if len(notices) != 1 {
			t.Fatalf("a renewal waiting on the bank must ask the account to confirm it once, got %d notices", len(notices))
		}
		requireStripeConfirmNotice(t, notices[0], "in_synthetic_confirm")

		deliverStripeInvoiceTestEvent(t, clientSession, "invoice.payment_failed", "in_synthetic_confirm")
		deliverStripeInvoiceTestEvent(t, clientSession, "invoice.payment_action_required", "in_synthetic_confirm")
		if notices := deliverStripePaymentFailedTestNotices(ctx, sender, email); len(notices) != 1 {
			t.Fatalf("the same attempt's other events must not email again, got %d notices", len(notices))
		}

		// the in-app past-due notice and Manage Subscription's invoice link
		// show it too, for an account that reads no email
		if pastDue := paymentPastDueForNetwork(ctx, networkId, server.NowUtc(), stripeDunningSettingsFunc()); pastDue == nil {
			t.Fatalf("a renewal waiting on the bank must show the past-due notice")
		}
	})
}

// invoice.payment_failed of a renewal that waits on the bank, with no failed
// attempt counted yet, asks the account to confirm the payment. It used to be
// skipped as "no failed attempt".
func TestStripePaymentFailedAtAttemptZeroWaitingOnTheBankAsksToConfirm(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		env := newStripePaymentFailedTestEnv(t)
		const email = "payment-confirm-failed@synthetic.example"
		networkId, clientSession := newStripePaymentFailedTestNetwork(t, ctx, "paymentconfirmfailed", email)
		setStripeRenewalAwaitingTheBank(env, networkId, "sub_synthetic_confirm_failed", "in_synthetic_confirm_failed", "requires_action")

		sender := newOutboxTestSender()
		deliverStripeInvoiceTestEvent(t, clientSession, "invoice.payment_failed", "in_synthetic_confirm_failed")
		notices := deliverStripePaymentFailedTestNotices(ctx, sender, email)
		if len(notices) != 1 {
			t.Fatalf("a payment waiting on the bank at attempt 0 must ask to confirm it, got %d notices", len(notices))
		}
		requireStripeConfirmNotice(t, notices[0], "in_synthetic_confirm_failed")
	})
}

// A confirmation reminder still unsent when the subscription ends is
// withdrawn with the failure: the reuse of the dunning's withdrawal. The
// reminder was owed first (the ledger row says so), so the empty delivery is
// the withdrawal, not a reminder that was never owed.
func TestStripeConfirmationReminderWithdrawnWhenTheSubscriptionEnds(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		env := newStripePaymentFailedTestEnv(t)
		const email = "payment-confirm-ended@synthetic.example"
		networkId, clientSession := newStripePaymentFailedTestNetwork(t, ctx, "paymentconfirmended", email)
		setStripeRenewalAwaitingTheBank(env, networkId, "sub_synthetic_confirm_ended", "in_synthetic_confirm_ended", "requires_action")

		deliverStripeInvoiceTestEvent(t, clientSession, "invoice.payment_action_required", "in_synthetic_confirm_ended")
		failure := model.GetStripeInvoicePaymentFailure(ctx, "in_synthetic_confirm_ended")
		if failure == nil || failure.FirstNoticeTime == nil {
			t.Fatalf("the renewal waiting on the bank must owe its reminder, got %+v", failure)
		}

		result, err := StripeWebhook(stripeWebhookEvent(t, "customer.subscription.deleted", map[string]any{
			"id":       "sub_synthetic_confirm_ended",
			"object":   "subscription",
			"status":   "canceled",
			"metadata": map[string]any{"network_id": networkId.String()},
		}), clientSession)
		if err != nil || result == nil {
			t.Fatalf("customer.subscription.deleted: %+v %v", result, err)
		}

		sender := newOutboxTestSender()
		if notices := deliverStripePaymentFailedTestNotices(ctx, sender, email); len(notices) != 0 {
			t.Fatalf("an ended subscription's unsent reminder must be withdrawn, got %d notices", len(notices))
		}
		failure = model.GetStripeInvoicePaymentFailure(ctx, "in_synthetic_confirm_ended")
		if failure == nil || failure.ResolveTime == nil || failure.ResolveReason != model.StripeInvoicePaymentFailureResolveSubscriptionEnded {
			t.Fatalf("the failure must be resolved by the subscription's end, got %+v", failure)
		}
	})
}

// The new rule is only for a payment that waits on the bank: an open renewal
// at attempt 0 whose payment intent wants a payment method (no attempt failed,
// nothing to confirm) still owes nothing.
func TestStripeRenewalAtAttemptZeroNotWaitingOnTheBankOwesNothing(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		env := newStripePaymentFailedTestEnv(t)
		const email = "payment-not-confirm@synthetic.example"
		networkId, clientSession := newStripePaymentFailedTestNetwork(t, ctx, "paymentnotconfirm", email)
		setStripeRenewalAwaitingTheBank(env, networkId, "sub_synthetic_not_confirm", "in_synthetic_not_confirm", "requires_payment_method")

		sender := newOutboxTestSender()
		deliverStripeInvoiceTestEvent(t, clientSession, "invoice.payment_failed", "in_synthetic_not_confirm")
		deliverStripeInvoiceTestEvent(t, clientSession, "invoice.payment_action_required", "in_synthetic_not_confirm")
		if notices := deliverStripePaymentFailedTestNotices(ctx, sender, email); len(notices) != 0 {
			t.Fatalf("attempt 0 not waiting on the bank owes nothing, got %d notices", len(notices))
		}
		if failure := model.GetStripeInvoicePaymentFailure(ctx, "in_synthetic_not_confirm"); failure != nil {
			t.Fatalf("attempt 0 not waiting on the bank must not be recorded, got %+v", failure)
		}
	})
}
