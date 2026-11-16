package controller

// Hermetic tests (no database) of the abandoned payment sheet sweep
// (subscription_stripe_abandoned_sheet_sweep.go). Stripe is the card check's
// fake api (subscription_stripe_card_check_fake_test.go); the stripe_invoice
// ledger is its seam.

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/server"
)

// A fake Stripe and a fake stripe_invoice ledger (invoice id -> credited).
func newAbandonedSheetSweepTest(t *testing.T) (*stripeCardCheckFake, map[string]bool) {
	fake := newStripeCardCheckFake(t)
	ledger := map[string]bool{}
	previousLedger := stripeInvoiceLedgerNetworkIdFunc
	stripeInvoiceLedgerNetworkIdFunc = func(ctx context.Context, invoiceId string) (server.Id, bool) {
		return server.Id{}, ledger[invoiceId]
	}
	t.Cleanup(func() {
		stripeInvoiceLedgerNetworkIdFunc = previousLedger
	})
	return fake, ledger
}

// A payment sheet's subscription for its own customer (with the customer's
// default card, "" for none), and its trial invoice.
func addAbandonedSheetTestSubscription(
	fake *stripeCardCheckFake,
	networkId server.Id,
	name string,
	status string,
	metadata map[string]string,
	created time.Time,
	defaultPaymentMethod string,
	customerPaymentMethod string,
) {
	fake.addCustomer("cus_"+name, customerPaymentMethod)
	trialEnd := created.Add(StripeSubscriptionTrialDays * 24 * time.Hour)
	fake.addSubscription("sub_"+name, "cus_"+name, networkId, status, metadata, defaultPaymentMethod, "in_"+name, created, trialEnd)
	fake.addInvoice("in_"+name, "sub_"+name, 0, created, trialEnd)
}

func abandonedSheetOutcomes() map[string]float64 {
	out := map[string]float64{}
	for _, outcome := range []string{
		stripeAbandonedSheetOutcomeCanceled,
		stripeAbandonedSheetOutcomeKept,
		stripeAbandonedSheetOutcomeCredited,
		stripeAbandonedSheetOutcomeFailed,
	} {
		out[outcome] = testutil.ToFloat64(stripeAbandonedSheetsTotal.WithLabelValues(outcome))
	}
	return out
}

// What the sweep cancels and what it keeps.
func TestStripeAbandonedSheetSweep(t *testing.T) {
	fake, ledger := newAbandonedSheetSweepTest(t)
	ctx := context.Background()
	now := server.NowUtc()
	networkId := server.NewId()
	yearly := map[string]string{"plan": "yearly", "payment_sheet": "pending"}
	monthly := map[string]string{"plan": "monthly", "payment_sheet": "pending"}
	refused := map[string]string{"plan": "yearly", "payment_sheet": "pending", "card_check": "cvc_failed"}
	dayOld := now.Add(-stripeAbandonedPaymentSheetAge - time.Hour)

	// abandoned over a day: the yearly trial, the monthly sheet's incomplete
	// subscription, and a sheet refused for its card
	addAbandonedSheetTestSubscription(fake, networkId, "abandoned", "trialing", yearly, dayOld, "", "")
	addAbandonedSheetTestSubscription(fake, networkId, "incomplete", "incomplete", monthly, dayOld, "", "")
	addAbandonedSheetTestSubscription(fake, networkId, "refused", "trialing", refused, dayOld, "", "")
	// under a day: a customer may still be completing it
	addAbandonedSheetTestSubscription(fake, networkId, "young", "trialing", yearly, now.Add(-stripeAbandonedPaymentSheetAge+time.Hour), "", "")
	// a card on the subscription, or as the customer's default
	fake.addCard("pm_carded", "cus_carded", "pass")
	addAbandonedSheetTestSubscription(fake, networkId, "carded", "trialing", yearly, dayOld, "pm_carded", "")
	fake.addCard("pm_customercard", "cus_customercard", "pass")
	addAbandonedSheetTestSubscription(fake, networkId, "customercard", "trialing", yearly, dayOld, "", "pm_customercard")
	// attached, or given a card, between the search and the cancel
	addAbandonedSheetTestSubscription(fake, networkId, "attached", "trialing", yearly, dayOld, "", "")
	fake.addCard("pm_lastmoment", "cus_lastmoment", "pass")
	addAbandonedSheetTestSubscription(fake, networkId, "lastmoment", "trialing", yearly, dayOld, "", "")
	fake.afterSearch = func() {
		fake.subscriptions["sub_attached"]["metadata"].(map[string]any)["payment_sheet"] = "attached"
		fake.subscriptions["sub_lastmoment"]["default_payment_method"] = "pm_lastmoment"
	}
	// its trial credited before, by the reconcile that once credited abandoned
	// sheets: it runs out as it is (the owner, 2026-11-16)
	addAbandonedSheetTestSubscription(fake, networkId, "credited", "trialing", yearly, now.Add(-10*24*time.Hour), "", "")
	ledger["in_credited"] = true
	// not a payment sheet's, or already ended: never found
	addAbandonedSheetTestSubscription(fake, networkId, "checkout", "trialing", map[string]string{"plan": "yearly"}, dayOld, "", "")
	addAbandonedSheetTestSubscription(fake, networkId, "ended", "canceled", yearly, dayOld, "", "")

	before := abandonedSheetOutcomes()
	result, err := stripeSweepAbandonedSheets(ctx, now, defaultStripeAbandonedSheetSettings())
	if err != nil {
		t.Fatal(err)
	}

	_, _, canceled := fake.writes()
	slices.Sort(canceled)
	if want := []string{"sub_abandoned", "sub_incomplete", "sub_refused"}; !slices.Equal(canceled, want) {
		t.Fatalf("canceled %v, want %v", canceled, want)
	}
	for _, kept := range []string{"young", "carded", "customercard", "attached", "lastmoment", "credited", "checkout"} {
		if status := fake.status("sub_" + kept); status != "trialing" {
			t.Fatalf("%s left %s, want trialing", kept, status)
		}
	}
	if result.FoundCount != 9 || result.CanceledCount != 3 || result.KeptCount != 5 || result.CreditedCount != 1 || result.FailedCount != 0 || result.Stopped != "" {
		t.Fatalf("result = %+v", result)
	}
	after := abandonedSheetOutcomes()
	for outcome, want := range map[string]float64{
		stripeAbandonedSheetOutcomeCanceled: 3,
		stripeAbandonedSheetOutcomeKept:     5,
		stripeAbandonedSheetOutcomeCredited: 1,
		stripeAbandonedSheetOutcomeFailed:   0,
	} {
		if got := after[outcome] - before[outcome]; got != want {
			t.Fatalf("%s counted %v, want %v", outcome, got, want)
		}
	}

	// the credited sheet is never read or canceled, and neither is one under
	// a day or one the search found with a card
	for _, untouched := range []string{"credited", "young", "carded"} {
		if count := fake.requestCount("GET /v1/subscriptions/sub_" + untouched); count != 0 {
			t.Fatalf("%s was read %d times", untouched, count)
		}
	}
	// what was canceled was read again first
	for _, name := range []string{"abandoned", "incomplete", "refused", "attached", "lastmoment", "customercard"} {
		if count := fake.requestCount("GET /v1/subscriptions/sub_" + name); count != 1 {
			t.Fatalf("%s was read %d times before its cancel, want once", name, count)
		}
	}
	// both searches name the sheets, the status and the day's cutoff
	if len(fake.searchQueries) != 2 {
		t.Fatalf("searches = %v", fake.searchQueries)
	}
	cutoff := fmt.Sprintf("created<%d", now.Add(-stripeAbandonedPaymentSheetAge).Unix())
	for index, status := range []string{"trialing", "incomplete"} {
		query := fake.searchQueries[index]
		if !strings.Contains(query, "metadata['payment_sheet']:'pending'") || !strings.Contains(query, "status:'"+status+"'") || !strings.Contains(query, cutoff) {
			t.Fatalf("search %d = %q", index, query)
		}
	}

	// the next run finds what is left, and cancels nothing more
	fake.afterSearch = nil
	result, err = stripeSweepAbandonedSheets(ctx, now, defaultStripeAbandonedSheetSettings())
	if err != nil || result.CanceledCount != 0 {
		t.Fatalf("second run: %+v, %v", result, err)
	}
}

// A run's calls to Stripe, and the errors it takes, are bounded.
func TestStripeAbandonedSheetSweepBoundsStripe(t *testing.T) {
	fake, _ := newAbandonedSheetSweepTest(t)
	ctx := context.Background()
	now := server.NowUtc()
	networkId := server.NewId()
	dayOld := now.Add(-stripeAbandonedPaymentSheetAge - time.Hour)
	for i := range 12 {
		addAbandonedSheetTestSubscription(fake, networkId, fmt.Sprintf("bound%02d", i), "trialing", map[string]string{"payment_sheet": "pending"}, dayOld, "", "")
	}
	settings := func(maxCalls int, maxErrors int) *stripeAbandonedSheetSettings {
		settings := defaultStripeAbandonedSheetSettings()
		settings.MaxCalls = maxCalls
		settings.MaxErrors = maxErrors
		return settings
	}

	// every read fails: the run stops at its error budget
	fake.failRoute("GET /v1/subscriptions/sub_*", http.StatusInternalServerError)
	result, err := stripeSweepAbandonedSheets(ctx, now, settings(200, 3))
	if err != nil {
		t.Fatal(err)
	}
	if result.FailedCount != 3 || result.CanceledCount != 0 || result.Stopped != "error budget" {
		t.Fatalf("failing reads: %+v", result)
	}
	if reads := fake.requestCount("GET /v1/subscriptions/sub_"); reads != 3 {
		t.Fatalf("failing reads: %d reads, want 3", reads)
	}

	// cancels that fail count against it too
	fake.failRoute("GET /v1/subscriptions/sub_*", 0)
	fake.failRoute("DELETE /v1/subscriptions/sub_*", http.StatusInternalServerError)
	result, err = stripeSweepAbandonedSheets(ctx, now, settings(200, 2))
	if err != nil || result.FailedCount != 2 || result.CanceledCount != 0 || result.Stopped != "error budget" {
		t.Fatalf("failing cancels: %+v, %v", result, err)
	}

	// the call budget: one search page, then a read and a cancel per sheet
	fake.failRoute("DELETE /v1/subscriptions/sub_*", 0)
	searchesBefore := len(fake.searchQueries)
	result, err = stripeSweepAbandonedSheets(ctx, now, settings(5, 5))
	if err != nil || result.CanceledCount != 2 || result.Stopped != "call budget" {
		t.Fatalf("call budget: %+v, %v", result, err)
	}
	if searches := len(fake.searchQueries) - searchesBefore; searches != 1 {
		t.Fatalf("call budget: %d searches, want 1", searches)
	}

	// a search Stripe does not answer fails the run, which the task retries
	fake.failRoute("GET /v1/subscriptions/search", http.StatusInternalServerError)
	if _, err := stripeSweepAbandonedSheets(ctx, now, settings(200, 5)); err == nil {
		t.Fatalf("a failed search did not fail the run")
	}
}

// An env without Stripe credentials skips the run and keeps the schedule.
func TestStripeAbandonedSheetSweepSkipsWithoutCredentials(t *testing.T) {
	previous := stripeReconcileHasCredentials
	stripeReconcileHasCredentials = func() bool { return false }
	t.Cleanup(func() { stripeReconcileHasCredentials = previous })
	result, err := SweepStripeAbandonedSheets(&SweepStripeAbandonedSheetsArgs{}, stripeVerifyTestSession(server.NewId()))
	if err != nil || result.Skip == "" || result.CanceledCount != 0 {
		t.Fatalf("result = %+v, err = %v", result, err)
	}
}
