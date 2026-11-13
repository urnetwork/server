package controller

// Hermetic tests (no database) for POST /subscription/verify-stripe-checkout
// (subscription_verify_stripe_controller.go): what each Stripe state answers,
// that only the caller's own checkout or subscription is credited, and that
// the credit goes through the webhooks' own handlers once. Stripe is a fake
// api server; the ledger, the credit and the rate limit are seams.
//
// Support inbox 7247: a card payment credited only when Stripe's webhook
// arrived (or the hourly reconcile ran), so a late webhook left a paying
// customer on Free with nothing the app could do.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
)

// a fixed paid period (2026-11-12 .. 2026-12-12 UTC)
const (
	stripeVerifyTestPeriodStart = int64(1794441600)
	stripeVerifyTestPeriodEnd   = int64(1797033600)
)

var stripeVerifyTestNow = time.Unix(stripeVerifyTestPeriodStart, 0).UTC().Add(10 * time.Minute)

// The fake Stripe api's objects and what the seams observed.
type stripeVerifyTestEnv struct {
	stateLock     sync.Mutex
	sessions      map[string]map[string]any
	subscriptions map[string]map[string]any
	invoices      map[string]map[string]any
	charges       map[string]map[string]any
	// every path the fake api served
	requests []string
	// answer every request with this status instead
	failStatus int

	// the stripe_invoice ledger: invoice id -> network
	ledger map[string]server.Id
	// the invoices the invoice.paid handler was run for
	credited []*StripeEventInvoiceObject
	// the checkout sessions the checkout.session.completed handler was run for
	fulfilled []*StripeEventCheckoutCompleteObject
	// the data pack sessions with a balance code
	fulfilledSessions map[string]bool
	// the customers finalized, as "<customer> <country>"
	finalized []string
	// what finalizing a customer credits: customer -> held invoice
	heldInvoices   map[string]string
	rateLimitCalls int
}

// A fake Stripe api and the endpoint's seams, restored when the test ends.
func newStripeVerifyTestEnv(t *testing.T) *stripeVerifyTestEnv {
	env := &stripeVerifyTestEnv{
		sessions:          map[string]map[string]any{},
		subscriptions:     map[string]map[string]any{},
		invoices:          map[string]map[string]any{},
		charges:           map[string]map[string]any{},
		ledger:            map[string]server.Id{},
		fulfilledSessions: map[string]bool{},
		heldInvoices:      map[string]string{},
	}
	serveFrom := func(objects func() map[string]map[string]any, key string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			env.stateLock.Lock()
			env.requests = append(env.requests, r.URL.Path)
			failStatus := env.failStatus
			object, ok := objects()[r.PathValue(key)]
			env.stateLock.Unlock()
			w.Header().Set("Content-Type", "application/json")
			if failStatus != 0 {
				w.WriteHeader(failStatus)
				json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"type": "api_error", "message": "synthetic"}})
				return
			}
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"type": "invalid_request_error", "message": "No such object"}})
				return
			}
			json.NewEncoder(w).Encode(object)
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/checkout/sessions/{id}", serveFrom(func() map[string]map[string]any { return env.sessions }, "id"))
	mux.HandleFunc("GET /v1/subscriptions/{id}", serveFrom(func() map[string]map[string]any { return env.subscriptions }, "id"))
	mux.HandleFunc("GET /v1/invoices/{id}", serveFrom(func() map[string]map[string]any { return env.invoices }, "id"))
	mux.HandleFunc("GET /v1/charges/{id}", serveFrom(func() map[string]map[string]any { return env.charges }, "id"))
	testServer := httptest.NewServer(mux)

	previousBaseUrl := stripeApiBaseUrl
	previousTokenFunc := stripeApiTokenFunc
	previousToken := stripeApiToken
	previousRateLimit := stripeVerifyRateLimitFunc
	previousLedger := stripeVerifyInvoiceNetworkIdFunc
	previousCredit := stripeVerifyCreditInvoiceFunc
	previousFinalize := stripeVerifyFinalizeCustomerFunc
	previousCheckoutCredited := stripeVerifyCheckoutCreditedFunc
	previousFulfill := stripeVerifyFulfillCheckoutFunc
	previousCountry := stripeVerifyPaymentMethodCountry
	previousNow := stripeVerifyNowFunc
	previousResolve := stripeResolveInvoicePaymentFailureFunc

	stripeApiBaseUrl = testServer.URL
	stripeApiTokenFunc = func() string { return "sk_test_verify" }
	stripeApiToken = func() string { return "sk_test_verify" }
	stripeVerifyRateLimitFunc = func(clientSession *session.ClientSession) error {
		env.stateLock.Lock()
		defer env.stateLock.Unlock()
		env.rateLimitCalls += 1
		return nil
	}
	stripeVerifyInvoiceNetworkIdFunc = func(ctx context.Context, invoiceId string) (server.Id, bool) {
		env.stateLock.Lock()
		defer env.stateLock.Unlock()
		networkId, ok := env.ledger[invoiceId]
		return networkId, ok
	}
	// the invoice.paid handler, as far as the ledger goes: the network is the
	// subscription's network_id, as stripeResolveInvoiceCredit resolves it
	stripeVerifyCreditInvoiceFunc = func(invoice *StripeEventInvoiceObject, clientSession *session.ClientSession) (*StripeWebhookResult, error) {
		env.stateLock.Lock()
		defer env.stateLock.Unlock()
		env.credited = append(env.credited, invoice)
		stored := env.invoices[invoice.Id]
		subscription, _ := stored["subscription_metadata"].(map[string]any)
		networkIdText, _ := subscription["network_id"].(string)
		networkId, err := server.ParseId(networkIdText)
		if err != nil {
			return nil, err
		}
		if _, ok := env.ledger[invoice.Id]; !ok {
			env.ledger[invoice.Id] = networkId
		}
		return &StripeWebhookResult{}, nil
	}
	stripeVerifyFinalizeCustomerFunc = func(clientSession *session.ClientSession, customerId string, countryCode string) {
		env.stateLock.Lock()
		defer env.stateLock.Unlock()
		env.finalized = append(env.finalized, customerId+" "+countryCode)
		if invoiceId, ok := env.heldInvoices[customerId]; ok {
			env.ledger[invoiceId] = clientSession.ByJwt.NetworkId
		}
	}
	stripeVerifyCheckoutCreditedFunc = func(clientSession *session.ClientSession, sessionId string) (bool, error) {
		env.stateLock.Lock()
		defer env.stateLock.Unlock()
		return env.fulfilledSessions[sessionId], nil
	}
	stripeVerifyFulfillCheckoutFunc = func(checkoutComplete *StripeEventCheckoutCompleteObject, clientSession *session.ClientSession) (*StripeWebhookResult, error) {
		env.stateLock.Lock()
		defer env.stateLock.Unlock()
		env.fulfilled = append(env.fulfilled, checkoutComplete)
		env.fulfilledSessions[checkoutComplete.Id] = true
		return &StripeWebhookResult{}, nil
	}
	stripeVerifyPaymentMethodCountry = func(paymentMethodId string) string {
		return "DE"
	}
	stripeVerifyNowFunc = func() time.Time { return stripeVerifyTestNow }
	stripeResolveInvoicePaymentFailureFunc = func(ctx context.Context, invoiceId string, reason string, now time.Time) {}

	t.Cleanup(func() {
		stripeApiBaseUrl = previousBaseUrl
		stripeApiTokenFunc = previousTokenFunc
		stripeApiToken = previousToken
		stripeVerifyRateLimitFunc = previousRateLimit
		stripeVerifyInvoiceNetworkIdFunc = previousLedger
		stripeVerifyCreditInvoiceFunc = previousCredit
		stripeVerifyFinalizeCustomerFunc = previousFinalize
		stripeVerifyCheckoutCreditedFunc = previousCheckoutCredited
		stripeVerifyFulfillCheckoutFunc = previousFulfill
		stripeVerifyPaymentMethodCountry = previousCountry
		stripeVerifyNowFunc = previousNow
		stripeResolveInvoicePaymentFailureFunc = previousResolve
		testServer.Close()
	})
	return env
}

// A signed-in session of the network.
func stripeVerifyTestSession(networkId server.Id) *session.ClientSession {
	return session.NewLocalClientSession(context.Background(), "127.0.0.1:1", &session.ByJwt{
		NetworkId: networkId,
		UserId:    server.NewId(),
	})
}

// a Pro checkout session as Stripe answers it once the customer paid
func (self *stripeVerifyTestEnv) setProCheckout(sessionId string, networkId server.Id, status string, invoiceId string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.sessions[sessionId] = map[string]any{
		"id":                  sessionId,
		"object":              "checkout.session",
		"mode":                "subscription",
		"status":              status,
		"payment_status":      "paid",
		"client_reference_id": networkId.String(),
		"customer":            "cus_verify",
		"subscription":        "sub_verify",
		"invoice":             invoiceId,
		"created":             stripeVerifyTestNow.Add(-5 * time.Minute).Unix(),
	}
}

// an invoice of a subscription stamped with networkId
func (self *stripeVerifyTestEnv) setInvoice(invoiceId string, networkId server.Id, status string, total int, chargeId string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	var charge any
	if chargeId != "" {
		charge = chargeId
	}
	self.invoices[invoiceId] = map[string]any{
		"id":             invoiceId,
		"object":         "invoice",
		"status":         status,
		"total":          total,
		"amount_paid":    total,
		"currency":       "usd",
		"customer":       "cus_verify",
		"billing_reason": "subscription_create",
		"charge":         charge,
		"lines": map[string]any{"data": []map[string]any{{
			"type":   "subscription",
			"period": map[string]any{"start": stripeVerifyTestPeriodStart, "end": stripeVerifyTestPeriodEnd},
		}}},
		// read only by the fake credit, standing for the subscription's
		// network_id metadata
		"subscription_metadata": map[string]any{"network_id": networkId.String()},
	}
}

// The charge that paid an invoice, with what it gave back.
func (self *stripeVerifyTestEnv) setCharge(chargeId string, amount int64, refunded int64, disputed bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.charges[chargeId] = map[string]any{
		"id":              chargeId,
		"status":          "succeeded",
		"amount":          amount,
		"amount_refunded": refunded,
		"refunded":        0 < refunded && refunded == amount,
		"disputed":        disputed,
	}
}

// A subscription the payment sheet created for the network.
func (self *stripeVerifyTestEnv) setPaymentSheetSubscription(subscriptionId string, networkId server.Id, status string, paymentSheet string, defaultPaymentMethod string, latestInvoiceId string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	metadata := map[string]any{"network_id": networkId.String(), "plan": "monthly"}
	if paymentSheet != "" {
		metadata["payment_sheet"] = paymentSheet
	}
	var paymentMethod any
	if defaultPaymentMethod != "" {
		paymentMethod = defaultPaymentMethod
	}
	self.subscriptions[subscriptionId] = map[string]any{
		"id":                     subscriptionId,
		"object":                 "subscription",
		"status":                 status,
		"customer":               "cus_sheet",
		"metadata":               metadata,
		"default_payment_method": paymentMethod,
		"latest_invoice":         map[string]any{"id": latestInvoiceId, "object": "invoice"},
	}
}

// Fails unless the endpoint answered status without an error.
func requireVerifyStatus(t *testing.T, name string, result *VerifyStorePurchaseResult, err error, status string) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: err = %v, want status %s", name, err, status)
	}
	if result == nil || result.Status != status {
		t.Fatalf("%s: result = %+v, want status %s", name, result, status)
	}
}

// The root cause of 7247 for card payments: a paid Pro checkout whose
// invoice.paid webhook has not arrived is credited as soon as the app reports
// its session, through the invoice.paid handler, once.
func TestVerifyStripeCheckoutCreditsAPaidCheckoutTheWebhookHasNotDelivered(t *testing.T) {
	env := newStripeVerifyTestEnv(t)
	networkId := server.NewId()
	env.setProCheckout("cs_test_paid", networkId, "complete", "in_paid")
	env.setInvoice("in_paid", networkId, "paid", 400, "ch_paid")
	env.setCharge("ch_paid", 400, 0, false)
	clientSession := stripeVerifyTestSession(networkId)

	result, err := VerifyStripeCheckout(&VerifyStripeCheckoutArgs{SessionId: "cs_test_paid"}, clientSession)
	requireVerifyStatus(t, "first report", result, err, VerifyPurchaseStatusCredited)
	if result.ExpiryTime == nil || !result.ExpiryTime.Equal(time.Unix(stripeVerifyTestPeriodEnd, 0)) {
		t.Fatalf("expiry = %v, want the paid period's end", result.ExpiryTime)
	}
	if len(env.credited) != 1 {
		t.Fatalf("invoice.paid handler ran %d times, want 1", len(env.credited))
	}
	// the handler gets the invoice as invoice.paid delivers it
	if invoice := env.credited[0]; invoice.Id != "in_paid" || invoice.Total != 400 || invoice.Customer != "cus_verify" || invoice.Currency != "usd" || invoice.BillingReason != "subscription_create" {
		t.Fatalf("credited invoice = %+v", invoice)
	}
	if env.ledger["in_paid"] != networkId {
		t.Fatalf("ledger = %v, want the invoice on the caller's network", env.ledger)
	}

	// a retried report, or one after the webhook: no second credit
	result, err = VerifyStripeCheckout(&VerifyStripeCheckoutArgs{SessionId: "cs_test_paid"}, clientSession)
	requireVerifyStatus(t, "second report", result, err, VerifyPurchaseStatusAlreadyCredited)
	if len(env.credited) != 1 {
		t.Fatalf("invoice.paid handler ran %d times after the second report, want 1", len(env.credited))
	}
}

// A checkout of another network is never credited by this caller's report:
// the session's client_reference_id names the network.
func TestVerifyStripeCheckoutRefusesAnotherNetworksCheckout(t *testing.T) {
	env := newStripeVerifyTestEnv(t)
	otherNetworkId := server.NewId()
	env.setProCheckout("cs_test_other", otherNetworkId, "complete", "in_other")
	env.setInvoice("in_other", otherNetworkId, "paid", 400, "")

	result, err := VerifyStripeCheckout(&VerifyStripeCheckoutArgs{SessionId: "cs_test_other"}, stripeVerifyTestSession(server.NewId()))
	requireVerifyStatus(t, "another network's checkout", result, err, VerifyPurchaseStatusWrongNetwork)
	if len(env.credited) != 0 {
		t.Fatalf("credited %d invoices of another network's checkout", len(env.credited))
	}

	// an invoice the ledger holds for another network answers wrong_network
	// too, and is not credited again
	networkId := server.NewId()
	env.setProCheckout("cs_test_moved", networkId, "complete", "in_moved")
	env.setInvoice("in_moved", networkId, "paid", 400, "")
	env.ledger["in_moved"] = otherNetworkId
	result, err = VerifyStripeCheckout(&VerifyStripeCheckoutArgs{SessionId: "cs_test_moved"}, stripeVerifyTestSession(networkId))
	requireVerifyStatus(t, "invoice credited elsewhere", result, err, VerifyPurchaseStatusWrongNetwork)
	if len(env.credited) != 0 {
		t.Fatalf("credited an invoice the ledger holds for another network")
	}
}

// What is not paid yet is pending, and stays uncredited until it is.
func TestVerifyStripeCheckoutIsPendingUntilStripeHasThePayment(t *testing.T) {
	env := newStripeVerifyTestEnv(t)
	networkId := server.NewId()
	clientSession := stripeVerifyTestSession(networkId)

	env.setProCheckout("cs_test_open", networkId, "open", "")
	result, err := VerifyStripeCheckout(&VerifyStripeCheckoutArgs{SessionId: "cs_test_open"}, clientSession)
	requireVerifyStatus(t, "open checkout", result, err, VerifyPurchaseStatusPending)

	env.setProCheckout("cs_test_unpaid", networkId, "complete", "in_unpaid")
	env.setInvoice("in_unpaid", networkId, "open", 400, "")
	result, err = VerifyStripeCheckout(&VerifyStripeCheckoutArgs{SessionId: "cs_test_unpaid"}, clientSession)
	requireVerifyStatus(t, "open invoice", result, err, VerifyPurchaseStatusPending)

	if len(env.credited) != 0 {
		t.Fatalf("credited %d unpaid invoices", len(env.credited))
	}

	// the same invoice once paid
	env.setInvoice("in_unpaid", networkId, "paid", 400, "")
	result, err = VerifyStripeCheckout(&VerifyStripeCheckoutArgs{SessionId: "cs_test_unpaid"}, clientSession)
	requireVerifyStatus(t, "paid invoice", result, err, VerifyPurchaseStatusCredited)
}

// Reports that can never credit answer invalid; malformed ones before the
// rate limit or Stripe is touched.
func TestVerifyStripeCheckoutInvalidReports(t *testing.T) {
	env := newStripeVerifyTestEnv(t)
	networkId := server.NewId()
	clientSession := stripeVerifyTestSession(networkId)

	for _, args := range []*VerifyStripeCheckoutArgs{
		{},
		{SessionId: "cs_test_a", SubscriptionId: "sub_a"},
		{SessionId: "pi_not_a_session"},
		{SessionId: "cs_test_../../v1/customers"},
		{SubscriptionId: "cs_test_a"},
	} {
		result, err := VerifyStripeCheckout(args, clientSession)
		requireVerifyStatus(t, "malformed report", result, err, VerifyPurchaseStatusInvalid)
	}
	if env.rateLimitCalls != 0 || len(env.requests) != 0 {
		t.Fatalf("malformed reports burned %d rate limit calls and %d Stripe requests", env.rateLimitCalls, len(env.requests))
	}

	// unknown to Stripe
	result, err := VerifyStripeCheckout(&VerifyStripeCheckoutArgs{SessionId: "cs_test_unknown"}, clientSession)
	requireVerifyStatus(t, "unknown session", result, err, VerifyPurchaseStatusInvalid)

	// expired
	env.setProCheckout("cs_test_expired", networkId, "expired", "")
	result, err = VerifyStripeCheckout(&VerifyStripeCheckoutArgs{SessionId: "cs_test_expired"}, clientSession)
	requireVerifyStatus(t, "expired session", result, err, VerifyPurchaseStatusInvalid)

	// a checkout that names no network (not one this server's apps started)
	env.setProCheckout("cs_test_anonymous", networkId, "complete", "in_anonymous")
	env.sessions["cs_test_anonymous"]["client_reference_id"] = nil
	result, err = VerifyStripeCheckout(&VerifyStripeCheckoutArgs{SessionId: "cs_test_anonymous"}, clientSession)
	requireVerifyStatus(t, "session without a network", result, err, VerifyPurchaseStatusInvalid)

	// a void invoice
	env.setProCheckout("cs_test_void", networkId, "complete", "in_void")
	env.setInvoice("in_void", networkId, "void", 400, "")
	result, err = VerifyStripeCheckout(&VerifyStripeCheckoutArgs{SessionId: "cs_test_void"}, clientSession)
	requireVerifyStatus(t, "void invoice", result, err, VerifyPurchaseStatusInvalid)

	// money that went back: refunded in part, or disputed
	env.setProCheckout("cs_test_refunded", networkId, "complete", "in_refunded")
	env.setInvoice("in_refunded", networkId, "paid", 400, "ch_refunded")
	env.setCharge("ch_refunded", 400, 100, false)
	result, err = VerifyStripeCheckout(&VerifyStripeCheckoutArgs{SessionId: "cs_test_refunded"}, clientSession)
	requireVerifyStatus(t, "refunded charge", result, err, VerifyPurchaseStatusInvalid)
	env.setProCheckout("cs_test_disputed", networkId, "complete", "in_disputed")
	env.setInvoice("in_disputed", networkId, "paid", 400, "ch_disputed")
	env.setCharge("ch_disputed", 400, 0, true)
	result, err = VerifyStripeCheckout(&VerifyStripeCheckoutArgs{SessionId: "cs_test_disputed"}, clientSession)
	requireVerifyStatus(t, "disputed charge", result, err, VerifyPurchaseStatusInvalid)

	if len(env.credited) != 0 {
		t.Fatalf("credited %d invoices of invalid reports", len(env.credited))
	}
}

// A Stripe failure is an error the client retries, never a terminal answer.
func TestVerifyStripeCheckoutStripeFailureIsRetryable(t *testing.T) {
	env := newStripeVerifyTestEnv(t)
	networkId := server.NewId()
	env.setProCheckout("cs_test_down", networkId, "complete", "in_down")
	env.failStatus = http.StatusInternalServerError

	result, err := VerifyStripeCheckout(&VerifyStripeCheckoutArgs{SessionId: "cs_test_down"}, stripeVerifyTestSession(networkId))
	if err == nil {
		t.Fatalf("Stripe failure answered %+v, want an error", result)
	}
}

// The rate limit's refusal passes through, before Stripe is read.
func TestVerifyStripeCheckoutRateLimited(t *testing.T) {
	env := newStripeVerifyTestEnv(t)
	stripeVerifyRateLimitFunc = func(clientSession *session.ClientSession) error {
		return errors.New("429 too many verify attempts")
	}
	_, err := VerifyStripeCheckout(&VerifyStripeCheckoutArgs{SessionId: "cs_test_limited"}, stripeVerifyTestSession(server.NewId()))
	if err == nil || err.Error() != "429 too many verify attempts" {
		t.Fatalf("err = %v, want the rate limit's refusal", err)
	}
	if len(env.requests) != 0 {
		t.Fatalf("a rate limited report read Stripe %d times", len(env.requests))
	}
}

// The payment sheet (the Android flavors without Play) reports its
// subscription: a paid first invoice credits at once, a sheet that has not
// confirmed is pending, and the trial whose card is attached is finalized as
// setup_intent.succeeded does.
func TestVerifyStripePaymentSheetSubscription(t *testing.T) {
	env := newStripeVerifyTestEnv(t)
	networkId := server.NewId()
	clientSession := stripeVerifyTestSession(networkId)

	// monthly, first invoice paid
	env.setPaymentSheetSubscription("sub_monthly", networkId, "active", "pending", "pm_card", "in_monthly")
	env.setInvoice("in_monthly", networkId, "paid", 500, "")
	result, err := VerifyStripeCheckout(&VerifyStripeCheckoutArgs{SubscriptionId: "sub_monthly"}, clientSession)
	requireVerifyStatus(t, "paid monthly sheet", result, err, VerifyPurchaseStatusCredited)
	if len(env.credited) != 1 || env.credited[0].Id != "in_monthly" {
		t.Fatalf("credited = %v, want in_monthly once", env.credited)
	}

	// not confirmed yet
	env.setPaymentSheetSubscription("sub_incomplete", networkId, "incomplete", "pending", "", "in_incomplete")
	result, err = VerifyStripeCheckout(&VerifyStripeCheckoutArgs{SubscriptionId: "sub_incomplete"}, clientSession)
	requireVerifyStatus(t, "incomplete sheet", result, err, VerifyPurchaseStatusPending)

	// abandoned
	env.setPaymentSheetSubscription("sub_abandoned", networkId, "incomplete_expired", "pending", "", "in_abandoned")
	result, err = VerifyStripeCheckout(&VerifyStripeCheckoutArgs{SubscriptionId: "sub_abandoned"}, clientSession)
	requireVerifyStatus(t, "abandoned sheet", result, err, VerifyPurchaseStatusInvalid)

	// the yearly trial, card not attached yet: held, nothing finalized
	env.setPaymentSheetSubscription("sub_trial", networkId, "trialing", "pending", "", "in_trial")
	env.setInvoice("in_trial", networkId, "paid", 0, "")
	result, err = VerifyStripeCheckout(&VerifyStripeCheckoutArgs{SubscriptionId: "sub_trial"}, clientSession)
	requireVerifyStatus(t, "trial without a card", result, err, VerifyPurchaseStatusPending)
	if len(env.finalized) != 0 {
		t.Fatalf("finalized %v before the card was attached", env.finalized)
	}

	// the card is attached: finalized with its country, the held invoice credits
	env.setPaymentSheetSubscription("sub_trial", networkId, "trialing", "pending", "pm_card", "in_trial")
	env.heldInvoices["cus_sheet"] = "in_trial"
	result, err = VerifyStripeCheckout(&VerifyStripeCheckoutArgs{SubscriptionId: "sub_trial"}, clientSession)
	requireVerifyStatus(t, "trial with a card", result, err, VerifyPurchaseStatusCredited)
	if len(env.finalized) != 1 || env.finalized[0] != "cus_sheet DE" {
		t.Fatalf("finalized = %v, want the customer once with the card's country", env.finalized)
	}
	result, err = VerifyStripeCheckout(&VerifyStripeCheckoutArgs{SubscriptionId: "sub_trial"}, clientSession)
	requireVerifyStatus(t, "trial reported again", result, err, VerifyPurchaseStatusAlreadyCredited)
	if len(env.finalized) != 1 {
		t.Fatalf("finalized %d times, want once", len(env.finalized))
	}

	// another network's subscription
	env.setPaymentSheetSubscription("sub_other", server.NewId(), "active", "attached", "pm_card", "in_other_sheet")
	result, err = VerifyStripeCheckout(&VerifyStripeCheckoutArgs{SubscriptionId: "sub_other"}, clientSession)
	requireVerifyStatus(t, "another network's sheet", result, err, VerifyPurchaseStatusWrongNetwork)

	// a canceled subscription is never credited by a report
	env.setPaymentSheetSubscription("sub_canceled", networkId, "canceled", "attached", "pm_card", "in_canceled")
	env.setInvoice("in_canceled", networkId, "paid", 500, "")
	result, err = VerifyStripeCheckout(&VerifyStripeCheckoutArgs{SubscriptionId: "sub_canceled"}, clientSession)
	requireVerifyStatus(t, "canceled, never credited", result, err, VerifyPurchaseStatusInvalid)
	env.ledger["in_canceled"] = networkId
	result, err = VerifyStripeCheckout(&VerifyStripeCheckoutArgs{SubscriptionId: "sub_canceled"}, clientSession)
	requireVerifyStatus(t, "canceled, credited before", result, err, VerifyPurchaseStatusAlreadyCredited)

	if len(env.credited) != 1 {
		t.Fatalf("invoice.paid handler ran %d times, want only for in_monthly", len(env.credited))
	}
}

// A data pack checkout is fulfilled through the checkout.session.completed
// handler once, inside the window Stripe would still deliver its webhook.
func TestVerifyStripeDataPackCheckout(t *testing.T) {
	env := newStripeVerifyTestEnv(t)
	networkId := server.NewId()
	clientSession := stripeVerifyTestSession(networkId)
	setDataCheckout := func(sessionId string, paymentStatus string, created time.Time) {
		env.sessions[sessionId] = map[string]any{
			"id":                  sessionId,
			"object":              "checkout.session",
			"mode":                "payment",
			"status":              "complete",
			"payment_status":      paymentStatus,
			"client_reference_id": networkId.String(),
			"customer_details":    map[string]any{"email": "buyer@example.com"},
			"amount_total":        500,
			"payment_intent":      "pi_data",
			"created":             created.Unix(),
		}
	}

	setDataCheckout("cs_test_data", "paid", stripeVerifyTestNow.Add(-time.Minute))
	result, err := VerifyStripeCheckout(&VerifyStripeCheckoutArgs{SessionId: "cs_test_data"}, clientSession)
	requireVerifyStatus(t, "paid data pack", result, err, VerifyPurchaseStatusCredited)
	if len(env.fulfilled) != 1 {
		t.Fatalf("fulfilled %d times, want 1", len(env.fulfilled))
	}
	if fulfilled := env.fulfilled[0]; fulfilled.Id != "cs_test_data" || fulfilled.ClientReferenceId != networkId.String() || fulfilled.CustomerDetails == nil || fulfilled.CustomerDetails.Email != "buyer@example.com" || stripeIdFromRaw(fulfilled.PaymentIntent) != "pi_data" {
		t.Fatalf("fulfilled = %+v", fulfilled)
	}
	result, err = VerifyStripeCheckout(&VerifyStripeCheckoutArgs{SessionId: "cs_test_data"}, clientSession)
	requireVerifyStatus(t, "data pack reported again", result, err, VerifyPurchaseStatusAlreadyCredited)

	setDataCheckout("cs_test_data_unpaid", "unpaid", stripeVerifyTestNow.Add(-time.Minute))
	result, err = VerifyStripeCheckout(&VerifyStripeCheckoutArgs{SessionId: "cs_test_data_unpaid"}, clientSession)
	requireVerifyStatus(t, "unpaid data pack", result, err, VerifyPurchaseStatusPending)

	setDataCheckout("cs_test_data_old", "paid", stripeVerifyTestNow.Add(-73*time.Hour))
	result, err = VerifyStripeCheckout(&VerifyStripeCheckoutArgs{SessionId: "cs_test_data_old"}, clientSession)
	requireVerifyStatus(t, "data pack past the webhook window", result, err, VerifyPurchaseStatusInvalid)

	if len(env.fulfilled) != 1 {
		t.Fatalf("fulfilled %d times, want only cs_test_data", len(env.fulfilled))
	}
}
