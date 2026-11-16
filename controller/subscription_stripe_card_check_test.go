package controller

// Hermetic tests (no database) of the trial card check
// (subscription_stripe_card_check.go): which CVC results refuse a trial, what
// a refusal does at Stripe, and the gate every invoice credit goes through.
// Stripe is a fake api (subscription_stripe_card_check_fake_test.go).

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stripe/stripe-go/v82"

	"github.com/urnetwork/server"
)

// Only a CVC check that failed refuses: many cards and regions return no CVC
// result, and those are never blocked.
func TestStripeCardCvcFailed(t *testing.T) {
	card := func(check stripe.PaymentMethodCardChecksCVCCheck) *stripe.PaymentMethod {
		return &stripe.PaymentMethod{
			Type: stripe.PaymentMethodTypeCard,
			Card: &stripe.PaymentMethodCard{Checks: &stripe.PaymentMethodCardChecks{CVCCheck: check}},
		}
	}
	tests := []struct {
		name          string
		paymentMethod *stripe.PaymentMethod
		want          bool
	}{
		{"fail", card(stripe.PaymentMethodCardChecksCVCCheckFail), true},
		{"pass", card(stripe.PaymentMethodCardChecksCVCCheckPass), false},
		{"unavailable", card(stripe.PaymentMethodCardChecksCVCCheckUnavailable), false},
		{"unchecked", card(stripe.PaymentMethodCardChecksCVCCheckUnchecked), false},
		{"no cvc result", card(""), false},
		{"no checks", &stripe.PaymentMethod{Type: stripe.PaymentMethodTypeCard, Card: &stripe.PaymentMethodCard{}}, false},
		{"not a card", &stripe.PaymentMethod{Type: stripe.PaymentMethodTypeLink}, false},
		{"no payment method", nil, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := stripeCardCvcFailed(test.paymentMethod); got != test.want {
				t.Fatalf("cvc failed = %t, want %t", got, test.want)
			}
		})
	}
}

// The refusal the purchase report answers, exactly as the apps are briefed
// on it.
func TestStripeCardCheckRefusalAnswer(t *testing.T) {
	result := verifyStripeCardRefused()
	if result.Status != VerifyPurchaseStatusInvalid || result.Error == nil ||
		result.Error.Code != "incorrect_cvc" ||
		result.Error.Message != "Your card's security code (CVC) didn't match. Check the card details and try again." {
		t.Fatalf("refusal = %+v %+v", result, result.Error)
	}
}

// What a $0 trial invoice may do, by its subscription and its card.
func TestStripeInvoiceTrialCardGate(t *testing.T) {
	fake := newStripeCardCheckFake(t)
	ctx := context.Background()
	now := server.NowUtc()
	trialEnd := now.Add(StripeSubscriptionTrialDays * 24 * time.Hour)
	networkId := server.NewId()
	notCredited := func() bool { return false }

	// a subscription of each kind, its card and its $0 trial invoice
	add := func(name string, status string, metadata map[string]string, cvcCheck string, total int) {
		fake.addCard("pm_"+name, "cus_"+name, cvcCheck)
		fake.addSubscription("sub_"+name, "cus_"+name, networkId, status, metadata, "pm_"+name, "in_"+name, now.Add(-time.Minute), trialEnd)
		fake.addInvoice("in_"+name, "sub_"+name, total, now.Add(-time.Minute), trialEnd)
	}
	gate := func(name string, total int, dryRun bool, credited func() bool) (stripeTrialCardDecision, error) {
		t.Helper()
		sub, err := stripeSubscriptionForInvoice(ctx, "in_"+name)
		if err != nil || sub == nil {
			t.Fatalf("%s: subscription = %v, err = %v", name, sub, err)
		}
		return stripeInvoiceTrialCardGate(sub, total, now, dryRun, credited)
	}
	requireDecision := func(name string, total int, dryRun bool, credited func() bool, want stripeTrialCardDecision) {
		t.Helper()
		decision, err := gate(name, total, dryRun, credited)
		if err != nil || decision != want {
			t.Fatalf("%s: decision = %d, err = %v, want %d", name, decision, err, want)
		}
	}
	requireNoWrites := func(name string) {
		t.Helper()
		if updates, detached, canceled := fake.writes(); len(updates) != 0 || len(detached) != 0 || len(canceled) != 0 {
			t.Fatalf("%s: wrote updates %v, detached %v, canceled %v", name, updates, detached, canceled)
		}
	}
	checkout := map[string]string{"plan": "yearly"}
	refusals := func() float64 {
		return testutil.ToFloat64(stripeCardCheckRefusalsTotal.WithLabelValues(stripeCardCheckFlowCheckout))
	}
	refusalsBefore := refusals()

	// a paid invoice charged its card: never gated
	add("paid", "trialing", checkout, "fail", 500)
	requireDecision("paid", 500, false, notCredited, stripeTrialCardCredit)

	// the payment sheet: held while pending; checked when it was attached
	add("pending", "trialing", map[string]string{"plan": "yearly", "payment_sheet": "pending"}, "pass", 0)
	requireDecision("pending", 0, false, notCredited, stripeTrialCardHeld)
	add("attached", "trialing", map[string]string{"plan": "yearly", "payment_sheet": "attached"}, "fail", 0)
	requireDecision("attached", 0, false, notCredited, stripeTrialCardCredit)

	// checkout trials whose card did not fail its CVC check
	for _, cvcCheck := range []string{"pass", "unavailable", "unchecked", ""} {
		name := "checkout_" + cvcCheck
		if cvcCheck == "" {
			name = "checkout_nocheck"
		}
		add(name, "trialing", checkout, cvcCheck, 0)
		requireDecision(name, 0, false, notCredited, stripeTrialCardCredit)
	}

	// a $0 invoice outside a trial (a fully discounted period) is no trial
	add("discounted", "active", checkout, "fail", 0)
	requireDecision("discounted", 0, false, notCredited, stripeTrialCardCredit)

	// an invoice the ledger holds is never refused: its window stays
	add("credited", "trialing", checkout, "fail", 0)
	requireDecision("credited", 0, false, func() bool { return true }, stripeTrialCardCredit)

	// a dry run decides and does nothing
	add("dryrun", "trialing", checkout, "fail", 0)
	requireDecision("dryrun", 0, true, notCredited, stripeTrialCardRefused)
	requireNoWrites("before any refusal")
	if refusalsNow := refusals(); refusalsNow != refusalsBefore {
		t.Fatalf("refusals counted %v before any refusal", refusalsNow-refusalsBefore)
	}

	// a checkout trial whose card failed: stamped, the card unset and
	// detached, the subscription canceled, counted once
	add("refused", "trialing", checkout, "fail", 0)
	requireDecision("refused", 0, false, notCredited, stripeTrialCardRefused)
	updates, detached, canceled := fake.writes()
	if len(updates) != 1 || !strings.HasPrefix(updates[0], "sub_refused ") ||
		!strings.Contains(updates[0], "metadata[card_check]=cvc_failed") ||
		!strings.Contains(updates[0], "default_payment_method=") {
		t.Fatalf("updates = %v, want the stamp with the card unset", updates)
	}
	if !slices.Equal(detached, []string{"pm_refused"}) || !slices.Equal(canceled, []string{"sub_refused"}) {
		t.Fatalf("detached %v, canceled %v", detached, canceled)
	}
	if fake.defaultPaymentMethod("sub_refused") != "" || fake.metadata("sub_refused")["card_check"] != "cvc_failed" || fake.status("sub_refused") != "canceled" {
		t.Fatalf("refused subscription at stripe: %v %s %s", fake.metadata("sub_refused"), fake.defaultPaymentMethod("sub_refused"), fake.status("sub_refused"))
	}
	if refusals() != refusalsBefore+1 {
		t.Fatalf("refusals counted %v, want 1", refusals()-refusalsBefore)
	}

	// read again: the stamp refuses with nothing more done or counted
	requireDecision("refused", 0, false, notCredited, stripeTrialCardRefused)
	if updates, detached, canceled := fake.writes(); len(updates) != 1 || len(detached) != 1 || len(canceled) != 1 {
		t.Fatalf("a second pass wrote again: %v %v %v", updates, detached, canceled)
	}
	if refusals() != refusalsBefore+1 {
		t.Fatalf("a second pass counted again")
	}

	// a stamp whose cancel did not go through: the next pass cancels
	add("stamped", "trialing", map[string]string{"plan": "yearly", "card_check": "cvc_failed"}, "fail", 0)
	requireDecision("stamped", 0, false, notCredited, stripeTrialCardRefused)
	if fake.status("sub_stamped") != "canceled" {
		t.Fatalf("a stamped trial left %s", fake.status("sub_stamped"))
	}

	// the stamp could not be written: still a refusal, with the error to retry,
	// and nothing else done
	add("stampfails", "trialing", checkout, "fail", 0)
	fake.failRoute("POST /v1/subscriptions/sub_stampfails", http.StatusInternalServerError)
	decision, err := gate("stampfails", 0, false, notCredited)
	if decision != stripeTrialCardRefused || err == nil {
		t.Fatalf("stamp failure: decision %d, err %v", decision, err)
	}
	if _, detachedNow, canceledNow := fake.writes(); fake.status("sub_stampfails") != "trialing" || len(detachedNow) != 1 || len(canceledNow) != 2 {
		t.Fatalf("a refusal whose stamp failed went on: detached %v, canceled %v", detachedNow, canceledNow)
	}

	// a subscription with no card of its own charges its customer's default:
	// that card is the one checked, and the one detached
	fake.addCard("pm_customerbad", "cus_customerbad", "fail")
	fake.addCustomer("cus_customerbad", "pm_customerbad")
	fake.addSubscription("sub_customerbad", "cus_customerbad", networkId, "trialing", checkout, "", "in_customerbad", now.Add(-time.Minute), trialEnd)
	fake.addInvoice("in_customerbad", "sub_customerbad", 0, now.Add(-time.Minute), trialEnd)
	requireDecision("customerbad", 0, false, notCredited, stripeTrialCardRefused)
	if _, detachedNow, _ := fake.writes(); !slices.Contains(detachedNow, "pm_customerbad") || fake.status("sub_customerbad") != "canceled" {
		t.Fatalf("the customer's failed card: detached %v, subscription %s", detachedNow, fake.status("sub_customerbad"))
	}
	fake.addCard("pm_customergood", "cus_customergood", "pass")
	fake.addCustomer("cus_customergood", "pm_customergood")
	fake.addSubscription("sub_customergood", "cus_customergood", networkId, "trialing", checkout, "", "in_customergood", now.Add(-time.Minute), trialEnd)
	fake.addInvoice("in_customergood", "sub_customergood", 0, now.Add(-time.Minute), trialEnd)
	requireDecision("customergood", 0, false, notCredited, stripeTrialCardCredit)
	// no card anywhere (a customer Stripe no longer knows): nothing to check
	fake.addSubscription("sub_nocard", "cus_gone", networkId, "trialing", checkout, "", "in_nocard", now.Add(-time.Minute), trialEnd)
	fake.addInvoice("in_nocard", "sub_nocard", 0, now.Add(-time.Minute), trialEnd)
	requireDecision("nocard", 0, false, notCredited, stripeTrialCardCredit)
	if fake.requestCount("GET /v1/customers/cus_gone") != 1 {
		t.Fatalf("the customer of a subscription with no card was not read")
	}

	// a card Stripe does not answer for: no decision, an error to retry
	fake.addCard("pm_unread", "cus_unread", "fail")
	fake.failRoute("GET /v1/payment_methods/pm_unread", http.StatusInternalServerError)
	unexpanded := &stripe.Subscription{
		ID:                   "sub_unread",
		Status:               stripe.SubscriptionStatusTrialing,
		TrialEnd:             trialEnd.Unix(),
		Metadata:             map[string]string{"plan": "yearly"},
		DefaultPaymentMethod: &stripe.PaymentMethod{ID: "pm_unread"},
	}
	if _, err := stripeInvoiceTrialCardGate(unexpanded, 0, now, false, notCredited); err == nil {
		t.Fatalf("an unreadable card was judged")
	}
	if fake.requestCount("GET /v1/payment_methods/pm_unread") != 1 {
		t.Fatalf("the unexpanded card was not read")
	}
}
