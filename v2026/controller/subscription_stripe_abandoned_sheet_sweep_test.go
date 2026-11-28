package controller

// Hermetic tests (no database) of the abandoned payment sheet sweep
// (subscription_stripe_abandoned_sheet_sweep.go). Stripe is the card check's
// fake api (subscription_stripe_card_check_fake_test.go); the stripe_invoice
// ledger and the finalization are its seams. The finalization against the
// database is tested in subscription_stripe_card_check_db_test.go.

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
)

// A fake Stripe, a fake stripe_invoice ledger (invoice id -> credited), and a
// fake finalization: per customer, it attaches each pending trialing sheet
// whose own card passed and refuses one whose card failed, at the fake
// Stripe. finalizeErr makes it fail for a customer.
type abandonedSheetSweepTest struct {
	fake        *stripeCardCheckFake
	ledger      map[string]bool
	finalized   []string
	finalizeErr map[string]error
}

func newAbandonedSheetSweepTest(t *testing.T) *abandonedSheetSweepTest {
	test := &abandonedSheetSweepTest{
		fake:        newStripeCardCheckFake(t),
		ledger:      map[string]bool{},
		finalizeErr: map[string]error{},
	}
	previousLedger := stripeInvoiceLedgerNetworkIdFunc
	previousFinalize := stripeAbandonedSheetFinalizeFunc
	stripeInvoiceLedgerNetworkIdFunc = func(ctx context.Context, invoiceId string) (server.Id, bool) {
		return server.Id{}, test.ledger[invoiceId]
	}
	stripeAbandonedSheetFinalizeFunc = func(clientSession *session.ClientSession, customerId string, countryCode string) *stripeFinalizeResult {
		result := &stripeFinalizeResult{Refused: map[string]bool{}, Attached: map[string]bool{}}
		test.finalized = append(test.finalized, customerId)
		if err := test.finalizeErr[customerId]; err != nil {
			result.Err = err
			return result
		}
		fake := test.fake
		fake.stateLock.Lock()
		defer fake.stateLock.Unlock()
		for id, sub := range fake.subscriptions {
			metadata, _ := sub["metadata"].(map[string]any)
			paymentMethodId, _ := sub["default_payment_method"].(string)
			if sub["customer"] != customerId || metadata["payment_sheet"] != "pending" || sub["status"] != "trialing" || paymentMethodId == "" {
				continue
			}
			card, _ := fake.paymentMethods[paymentMethodId]["card"].(map[string]any)
			checks, _ := card["checks"].(map[string]any)
			if checks["cvc_check"] == "fail" {
				metadata["card_check"] = "cvc_failed"
				sub["default_payment_method"] = nil
				result.Refused[id] = true
			} else {
				metadata["payment_sheet"] = "attached"
				result.Attached[id] = true
			}
		}
		return result
	}
	t.Cleanup(func() {
		stripeInvoiceLedgerNetworkIdFunc = previousLedger
		stripeAbandonedSheetFinalizeFunc = previousFinalize
	})
	return test
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
		stripeAbandonedSheetOutcomeAttached,
		stripeAbandonedSheetOutcomeRefused,
		stripeAbandonedSheetOutcomeKept,
		stripeAbandonedSheetOutcomeCredited,
		stripeAbandonedSheetOutcomeFailed,
	} {
		out[outcome] = testutil.ToFloat64(stripeAbandonedSheetsTotal.WithLabelValues(outcome))
	}
	return out
}

// What the sweep cancels, attaches and keeps.
func TestStripeAbandonedSheetSweep(t *testing.T) {
	test := newAbandonedSheetSweepTest(t)
	fake := test.fake
	now := server.NowUtc()
	networkId := server.NewId()
	clientSession := stripeVerifyTestSession(networkId)
	yearly := map[string]string{"plan": "yearly", "payment_sheet": "pending"}
	monthly := map[string]string{"plan": "monthly", "payment_sheet": "pending"}
	refused := map[string]string{"plan": "yearly", "payment_sheet": "pending", "card_check": "cvc_failed"}
	dayOld := now.Add(-stripeAbandonedPaymentSheetAge - time.Hour)
	hourOld := now.Add(-time.Hour)

	// abandoned over a day: the yearly trial, the monthly sheet's incomplete
	// subscription, and a sheet refused for its card
	addAbandonedSheetTestSubscription(fake, networkId, "abandoned", "trialing", yearly, dayOld, "", "")
	addAbandonedSheetTestSubscription(fake, networkId, "incomplete", "incomplete", monthly, dayOld, "", "")
	addAbandonedSheetTestSubscription(fake, networkId, "refused", "trialing", refused, dayOld, "", "")
	// under a day with no card: a customer may still be completing it
	addAbandonedSheetTestSubscription(fake, networkId, "young", "trialing", yearly, now.Add(-stripeAbandonedPaymentSheetAge+time.Hour), "", "")
	// completed, a card on the subscription, but never attached: attached at
	// any age, and refused for a failed card
	fake.addCard("pm_carded", "cus_carded", "pass")
	addAbandonedSheetTestSubscription(fake, networkId, "carded", "trialing", yearly, dayOld, "pm_carded", "")
	fake.addCard("pm_youngcarded", "cus_youngcarded", "pass")
	addAbandonedSheetTestSubscription(fake, networkId, "youngcarded", "trialing", yearly, hourOld, "pm_youngcarded", "")
	fake.addCard("pm_badcard", "cus_badcard", "fail")
	addAbandonedSheetTestSubscription(fake, networkId, "badcard", "trialing", yearly, hourOld, "pm_badcard", "")
	// a card only as the customer's default, from an earlier purchase, and
	// none on the subscription: abandoned like any card-less sheet, canceled
	// after the day with the customer's card left as it is, kept before it
	// (the owner, 2026-11-16: "Cancel after a day")
	fake.addCard("pm_customercard", "cus_customercard", "pass")
	addAbandonedSheetTestSubscription(fake, networkId, "customercard", "trialing", yearly, dayOld, "", "pm_customercard")
	fake.addCard("pm_youngcustomercard", "cus_youngcustomercard", "pass")
	addAbandonedSheetTestSubscription(fake, networkId, "youngcustomercard", "trialing", yearly, now.Add(-stripeAbandonedPaymentSheetAge+time.Hour), "", "pm_youngcustomercard")
	// attached, or given a card, between the search and the cancel
	addAbandonedSheetTestSubscription(fake, networkId, "attached", "trialing", yearly, dayOld, "", "")
	fake.addCard("pm_lastmoment", "cus_lastmoment", "pass")
	addAbandonedSheetTestSubscription(fake, networkId, "lastmoment", "trialing", yearly, dayOld, "", "")
	fake.afterSearch = func() {
		if metadata, _ := fake.subscriptions["sub_attached"]["metadata"].(map[string]any); metadata["payment_sheet"] == "pending" {
			metadata["payment_sheet"] = "attached"
		}
		if fake.subscriptions["sub_lastmoment"]["default_payment_method"] == nil {
			fake.subscriptions["sub_lastmoment"]["default_payment_method"] = "pm_lastmoment"
		}
	}
	// its trial credited before, by the reconcile that once credited abandoned
	// sheets: it runs out as it is (the owner, 2026-11-16), card or not
	addAbandonedSheetTestSubscription(fake, networkId, "credited", "trialing", yearly, now.Add(-10*24*time.Hour), "", "")
	test.ledger["in_credited"] = true
	fake.addCard("pm_creditedcard", "cus_creditedcard", "fail")
	addAbandonedSheetTestSubscription(fake, networkId, "creditedcard", "trialing", yearly, dayOld, "pm_creditedcard", "")
	test.ledger["in_creditedcard"] = true
	fake.addCard("pm_creditedcustomercard", "cus_creditedcustomercard", "pass")
	addAbandonedSheetTestSubscription(fake, networkId, "creditedcustomercard", "trialing", yearly, dayOld, "", "pm_creditedcustomercard")
	test.ledger["in_creditedcustomercard"] = true
	// another product's subscription, not a payment sheet's, or already ended:
	// never touched
	addAbandonedSheetTestSubscription(fake, networkId, "notours", "trialing", yearly, dayOld, "", "")
	fake.deleteMetadata("sub_notours", "network_id")
	addAbandonedSheetTestSubscription(fake, networkId, "checkout", "trialing", map[string]string{"plan": "yearly"}, dayOld, "", "")
	addAbandonedSheetTestSubscription(fake, networkId, "ended", "canceled", yearly, dayOld, "", "")

	before := abandonedSheetOutcomes()
	result, err := stripeSweepAbandonedSheets(clientSession, now, defaultStripeAbandonedSheetSettings())
	if err != nil {
		t.Fatal(err)
	}

	_, detached, canceled := fake.writes()
	slices.Sort(canceled)
	if want := []string{"sub_abandoned", "sub_customercard", "sub_incomplete", "sub_refused"}; !slices.Equal(canceled, want) {
		t.Fatalf("canceled %v, want %v", canceled, want)
	}
	// the canceled sheet's customer keeps its card: nothing detached, still its
	// default
	if len(detached) != 0 {
		t.Fatalf("detached %v", detached)
	}
	fake.stateLock.Lock()
	customerDefault := fake.customers["cus_customercard"]["invoice_settings"].(map[string]any)["default_payment_method"]
	customerCardOwner := fake.paymentMethods["pm_customercard"]["customer"]
	fake.stateLock.Unlock()
	if customerDefault != "pm_customercard" || customerCardOwner != "cus_customercard" {
		t.Fatalf("the customer's card was touched: default %v, owner %v", customerDefault, customerCardOwner)
	}
	for _, name := range []string{"carded", "youngcarded", "lastmoment", "attached"} {
		if fake.metadata("sub_" + name)["payment_sheet"] != "attached" {
			t.Fatalf("%s is %v, want attached", name, fake.metadata("sub_"+name))
		}
	}
	if fake.metadata("sub_badcard")["card_check"] != "cvc_failed" {
		t.Fatalf("the failed card was not refused")
	}
	for _, name := range []string{"young", "youngcustomercard", "credited", "creditedcard", "creditedcustomercard", "notours", "checkout", "lastmoment"} {
		if status := fake.status("sub_" + name); status != "trialing" {
			t.Fatalf("%s left %s, want trialing", name, status)
		}
	}
	// 13 trialing sheets of ours at every age, 1 incomplete over a day old
	if result.FoundCount != 14 || result.CanceledCount != 4 || result.AttachedCount != 3 || result.RefusedCount != 1 ||
		result.KeptCount != 1 || result.CreditedCount != 3 || result.FailedCount != 0 || result.Stopped != "" {
		t.Fatalf("result = %+v", result)
	}
	after := abandonedSheetOutcomes()
	for outcome, want := range map[string]float64{
		stripeAbandonedSheetOutcomeCanceled: 4,
		stripeAbandonedSheetOutcomeAttached: 3,
		stripeAbandonedSheetOutcomeRefused:  1,
		stripeAbandonedSheetOutcomeKept:     1,
		stripeAbandonedSheetOutcomeCredited: 3,
		stripeAbandonedSheetOutcomeFailed:   0,
	} {
		if got := after[outcome] - before[outcome]; got != want {
			t.Fatalf("%s counted %v, want %v", outcome, got, want)
		}
	}

	// the credited sheets, the young one and the ones the search found with a
	// card are never read; their customers are finalized once each
	for _, untouched := range []string{"credited", "creditedcard", "creditedcustomercard", "young", "youngcustomercard", "carded", "youngcarded", "badcard", "notours"} {
		if count := fake.requestCount("GET /v1/subscriptions/sub_" + untouched); count != 0 {
			t.Fatalf("%s was read %d times", untouched, count)
		}
	}
	slices.Sort(test.finalized)
	if want := []string{"cus_badcard", "cus_carded", "cus_lastmoment", "cus_youngcarded"}; !slices.Equal(test.finalized, want) {
		t.Fatalf("finalized %v, want %v", test.finalized, want)
	}
	// what was canceled or kept was read again first
	for _, name := range []string{"abandoned", "incomplete", "refused", "attached", "lastmoment", "customercard"} {
		if count := fake.requestCount("GET /v1/subscriptions/sub_" + name); count != 1 {
			t.Fatalf("%s was read %d times, want once", name, count)
		}
	}
	// the trialing sheets are searched at every age, the incomplete ones from
	// a day back
	if len(fake.searchQueries) != 2 {
		t.Fatalf("searches = %v", fake.searchQueries)
	}
	cutoff := fmt.Sprintf("created<%d", now.Add(-stripeAbandonedPaymentSheetAge).Unix())
	trialing, incomplete := fake.searchQueries[0], fake.searchQueries[1]
	if !strings.Contains(trialing, "metadata['payment_sheet']:'pending'") || !strings.Contains(trialing, "status:'trialing'") || strings.Contains(trialing, "created<") {
		t.Fatalf("trialing search = %q", trialing)
	}
	if !strings.Contains(incomplete, "metadata['payment_sheet']:'pending'") || !strings.Contains(incomplete, "status:'incomplete'") || !strings.Contains(incomplete, cutoff) {
		t.Fatalf("incomplete search = %q", incomplete)
	}

	// the next run finds what is left, and cancels or attaches nothing more
	fake.afterSearch = nil
	result, err = stripeSweepAbandonedSheets(clientSession, now, defaultStripeAbandonedSheetSettings())
	if err != nil || result.CanceledCount != 0 || result.AttachedCount != 0 || result.RefusedCount != 0 {
		t.Fatalf("second run: %+v, %v", result, err)
	}
}

// A run's calls to Stripe, and the errors it takes, are bounded.
func TestStripeAbandonedSheetSweepBoundsStripe(t *testing.T) {
	test := newAbandonedSheetSweepTest(t)
	fake := test.fake
	now := server.NowUtc()
	networkId := server.NewId()
	clientSession := stripeVerifyTestSession(networkId)
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
	result, err := stripeSweepAbandonedSheets(clientSession, now, settings(200, 3))
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
	result, err = stripeSweepAbandonedSheets(clientSession, now, settings(200, 2))
	if err != nil || result.FailedCount != 2 || result.CanceledCount != 0 || result.Stopped != "error budget" {
		t.Fatalf("failing cancels: %+v, %v", result, err)
	}

	// the call budget: one search page, then a read and a cancel per sheet
	fake.failRoute("DELETE /v1/subscriptions/sub_*", 0)
	searchesBefore := len(fake.searchQueries)
	result, err = stripeSweepAbandonedSheets(clientSession, now, settings(5, 5))
	if err != nil || result.CanceledCount != 2 || result.Stopped != "call budget" {
		t.Fatalf("call budget: %+v, %v", result, err)
	}
	if searches := len(fake.searchQueries) - searchesBefore; searches != 1 {
		t.Fatalf("call budget: %d searches, want 1", searches)
	}

	// a search Stripe does not answer fails the run, which the task retries
	fake.failRoute("GET /v1/subscriptions/search", http.StatusInternalServerError)
	if _, err := stripeSweepAbandonedSheets(clientSession, now, settings(200, 5)); err == nil {
		t.Fatalf("a failed search did not fail the run")
	}
}

// A finalization counts as its calls against the budget, and one that fails
// counts against the error budget.
func TestStripeAbandonedSheetSweepBoundsFinalizations(t *testing.T) {
	test := newAbandonedSheetSweepTest(t)
	fake := test.fake
	now := server.NowUtc()
	networkId := server.NewId()
	clientSession := stripeVerifyTestSession(networkId)
	for i := range 3 {
		name := fmt.Sprintf("completed%02d", i)
		fake.addCard("pm_"+name, "cus_"+name, "pass")
		addAbandonedSheetTestSubscription(fake, networkId, name, "trialing", map[string]string{"payment_sheet": "pending"}, now.Add(-time.Hour), "pm_"+name, "")
	}
	test.finalizeErr["cus_completed00"] = fmt.Errorf("synthetic finalization failure")
	settings := defaultStripeAbandonedSheetSettings()
	// the search page, then two finalizations
	settings.MaxCalls = 1 + 2*stripeAbandonedSheetFinalizeCalls
	result, err := stripeSweepAbandonedSheets(clientSession, now, settings)
	if err != nil || result.FailedCount != 1 || result.AttachedCount != 1 || result.Stopped != "call budget" {
		t.Fatalf("finalization budget: %+v, %v", result, err)
	}
	if fake.metadata("sub_completed01")["payment_sheet"] != "attached" || fake.metadata("sub_completed02")["payment_sheet"] != "pending" {
		t.Fatalf("attached %v and %v", fake.metadata("sub_completed01"), fake.metadata("sub_completed02"))
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
