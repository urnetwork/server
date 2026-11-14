package controller

// Hermetic tests of the confirmation reminders (subscription_stripe_dunning.go):
// a renewal whose payment waits on the customer's bank (3D Secure) is a failing
// renewal before Stripe counts a failed attempt, what Stripe says about its
// payment is read at the pinned API version, and its notice asks the customer to
// confirm the payment. No database: a fake Stripe API on the raw-HTTP seam. The
// webhook paths through the database are in
// subscription_stripe_payment_action_db_test.go.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stripe/stripe-go/v82"
)

// A renewal at attempt 0 whose payment waits on the bank is a failing renewal
// and owes the first notice; at attempt 0 without it, it is not. It must still
// be an open renewal of a subscription Stripe is retrying.
func TestStripeDunningFailingReasonAcceptsAPaymentWaitingOnTheBank(t *testing.T) {
	created := time.Date(2026, 11, 10, 9, 12, 0, 0, time.UTC)
	next := stripeDunningTestTime(created.Add(3 * 24 * time.Hour))
	retrying := &stripeDunningSubscription{Id: "sub_synthetic_action", Status: "past_due"}

	waiting := stripeDunningTestInvoice(created, 0, next)
	waiting.PaymentActionRequired = true
	if reason := stripeDunningFailingReason(waiting, retrying); reason != "" {
		t.Fatalf("a renewal waiting on the bank at attempt 0 is a failing renewal, got %q", reason)
	}
	if notice := stripePaymentFailedNoticeDue(waiting, nil, defaultStripeDunningSettings()); notice != stripePaymentFailedNoticeFirst {
		t.Fatalf("a renewal waiting on the bank owes the first notice, got %q", notice)
	}

	notWaiting := stripeDunningTestInvoice(created, 0, next)
	if reason := stripeDunningFailingReason(notWaiting, retrying); reason != "no failed attempt" {
		t.Fatalf("attempt 0 not waiting on the bank, got %q", reason)
	}

	firstCharge := stripeDunningTestInvoice(created, 0, next)
	firstCharge.PaymentActionRequired = true
	firstCharge.BillingReason = "subscription_create"
	if reason := stripeDunningFailingReason(firstCharge, retrying); reason == "" {
		t.Fatalf("a first charge waiting on the bank is the checkout's, on-session")
	}
	paid := stripeDunningTestInvoice(created, 0, next)
	paid.PaymentActionRequired = true
	paid.Status = "paid"
	if reason := stripeDunningFailingReason(paid, retrying); reason == "" {
		t.Fatalf("a paid invoice is not a failing renewal")
	}
	if reason := stripeDunningFailingReason(waiting, &stripeDunningSubscription{Id: "sub_synthetic_action", Status: "canceled"}); reason == "" {
		t.Fatalf("an ended subscription's invoice is not a failing renewal")
	}
}

// A fake Stripe API for the reads of an invoice's payments: the payments of one
// invoice, the payment intents by id, and the Stripe-Version every read asked
// for. Safe for concurrent use.
type stripePaymentActionTestApi struct {
	stateLock sync.Mutex
	// the list's answer status; 0 answers the payments
	paymentsStatus int
	// refuse a list that asks to expand the payment intents, and answer one
	// that does not with the intents named only
	refuseExpansion bool
	listReads       int
	payments        []map[string]any
	// payment intent id -> status
	paymentIntentStatuses map[string]string
	paymentIntentReads    int
	versions              []string
}

func newStripePaymentActionTestApi(t testing.TB) *stripePaymentActionTestApi {
	api := &stripePaymentActionTestApi{paymentIntentStatuses: map[string]string{}}
	writeJson := func(w http.ResponseWriter, status int, object any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(object)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/invoice_payments", func(w http.ResponseWriter, r *http.Request) {
		api.stateLock.Lock()
		api.versions = append(api.versions, r.Header.Get("Stripe-Version"))
		api.listReads += 1
		status := api.paymentsStatus
		refuseExpansion := api.refuseExpansion
		payments := append([]map[string]any{}, api.payments...)
		api.stateLock.Unlock()
		if r.URL.Query().Get("invoice") != "in_synthetic_action" {
			writeJson(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": "another invoice"}})
			return
		}
		if status != 0 {
			writeJson(w, status, map[string]any{"error": map[string]any{"type": "invalid_request_error", "message": "synthetic refusal"}})
			return
		}
		if refuseExpansion {
			if r.URL.Query().Get("expand[]") != "" {
				writeJson(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"type": "invalid_request_error", "message": "this property cannot be expanded"}})
				return
			}
			// the payment intents named only
			named := []map[string]any{}
			for _, payment := range payments {
				paymentDetails := payment["payment"].(map[string]any)
				paymentIntentId := paymentDetails["payment_intent"]
				if paymentIntent, ok := paymentIntentId.(map[string]any); ok {
					paymentIntentId = paymentIntent["id"]
				}
				named = append(named, map[string]any{
					"id":      payment["id"],
					"object":  payment["object"],
					"invoice": payment["invoice"],
					"status":  payment["status"],
					"payment": map[string]any{"type": paymentDetails["type"], "payment_intent": paymentIntentId},
				})
			}
			payments = named
		}
		writeJson(w, http.StatusOK, map[string]any{"object": "list", "data": payments, "has_more": false})
	})
	mux.HandleFunc("GET /v1/payment_intents/{paymentIntentId}", func(w http.ResponseWriter, r *http.Request) {
		api.stateLock.Lock()
		api.versions = append(api.versions, r.Header.Get("Stripe-Version"))
		api.paymentIntentReads += 1
		status, ok := api.paymentIntentStatuses[r.PathValue("paymentIntentId")]
		api.stateLock.Unlock()
		if !ok {
			writeJson(w, http.StatusNotFound, map[string]any{"error": map[string]any{"message": "no such payment intent"}})
			return
		}
		writeJson(w, http.StatusOK, map[string]any{"id": r.PathValue("paymentIntentId"), "object": "payment_intent", "status": status})
	})
	testServer := httptest.NewServer(mux)
	previousBaseUrl := stripeApiBaseUrl
	previousTokenFunc := stripeApiTokenFunc
	stripeApiBaseUrl = testServer.URL
	stripeApiTokenFunc = func() string { return "sk_test_payment_action" }
	t.Cleanup(func() {
		stripeApiBaseUrl = previousBaseUrl
		stripeApiTokenFunc = previousTokenFunc
		testServer.Close()
	})
	return api
}

// One payment of the invoice, its payment intent expanded or, when status is
// "", only named.
func stripePaymentActionTestPayment(paymentStatus string, paymentType string, paymentIntentId string, paymentIntentStatus string) map[string]any {
	var paymentIntent any = paymentIntentId
	if paymentIntentStatus != "" {
		paymentIntent = map[string]any{"id": paymentIntentId, "object": "payment_intent", "status": paymentIntentStatus}
	}
	return map[string]any{
		"id":      "inpay_" + paymentIntentId,
		"object":  "invoice_payment",
		"invoice": "in_synthetic_action",
		"status":  paymentStatus,
		"payment": map[string]any{"type": paymentType, "payment_intent": paymentIntent},
	}
}

// The invoice waits on the bank when an open payment's intent requires action,
// whether Stripe expanded the intent or only named it; a declined, canceled or
// charge payment does not. Every read is pinned to the stripe-go API version.
func TestStripeDunningInvoiceActionRequiredReadsTheInvoicePayments(t *testing.T) {
	ctx := context.Background()
	settings := defaultStripeDunningSettings()
	for _, c := range []struct {
		name              string
		payments          []map[string]any
		namedIntentStatus string
		want              bool
		wantIntentReads   int
	}{
		{name: "expanded, requires action", payments: []map[string]any{stripePaymentActionTestPayment("open", "payment_intent", "pi_synthetic_a", "requires_action")}, want: true},
		{name: "named, requires action", payments: []map[string]any{stripePaymentActionTestPayment("open", "payment_intent", "pi_synthetic_b", "")}, namedIntentStatus: "requires_action", want: true, wantIntentReads: 1},
		{name: "declined", payments: []map[string]any{stripePaymentActionTestPayment("open", "payment_intent", "pi_synthetic_c", "requires_payment_method")}},
		{name: "canceled payment", payments: []map[string]any{stripePaymentActionTestPayment("canceled", "payment_intent", "pi_synthetic_d", "requires_action")}},
		{name: "a charge", payments: []map[string]any{stripePaymentActionTestPayment("open", "charge", "pi_synthetic_e", "requires_action")}},
		{name: "no payments", payments: []map[string]any{}},
		{name: "the second payment requires action", payments: []map[string]any{
			stripePaymentActionTestPayment("open", "payment_intent", "pi_synthetic_f", "requires_payment_method"),
			stripePaymentActionTestPayment("open", "payment_intent", "pi_synthetic_g", "requires_action"),
		}, want: true},
	} {
		api := newStripePaymentActionTestApi(t)
		api.payments = c.payments
		for _, payment := range c.payments {
			if paymentIntentId, ok := payment["payment"].(map[string]any)["payment_intent"].(string); ok {
				api.paymentIntentStatuses[paymentIntentId] = c.namedIntentStatus
			}
		}
		got, err := stripeDunningInvoiceActionRequired(ctx, "in_synthetic_action", settings)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got != c.want {
			t.Errorf("%s: action required %t, want %t", c.name, got, c.want)
		}
		if api.paymentIntentReads != c.wantIntentReads {
			t.Errorf("%s: %d payment intent reads, want %d", c.name, api.paymentIntentReads, c.wantIntentReads)
		}
		for _, version := range api.versions {
			if version != stripe.APIVersion {
				t.Errorf("%s: a read asked for Stripe-Version %q, want the pinned %q", c.name, version, stripe.APIVersion)
			}
		}
	}
}

// A list Stripe refuses with the payment intents expanded is read once more
// without the expansion, and each payment intent it names is read on its own.
func TestStripeDunningInvoiceActionRequiredReadsAgainWithoutExpansion(t *testing.T) {
	api := newStripePaymentActionTestApi(t)
	api.refuseExpansion = true
	api.payments = []map[string]any{stripePaymentActionTestPayment("open", "payment_intent", "pi_synthetic_unexpanded", "")}
	api.paymentIntentStatuses["pi_synthetic_unexpanded"] = "requires_action"
	got, err := stripeDunningInvoiceActionRequired(context.Background(), "in_synthetic_action", defaultStripeDunningSettings())
	if err != nil || !got {
		t.Fatalf("a refused expansion must be read without it: action required %t, %v", got, err)
	}
	if api.listReads != 2 || api.paymentIntentReads != 1 {
		t.Fatalf("%d list reads and %d payment intent reads, want 2 and 1", api.listReads, api.paymentIntentReads)
	}
}

// Stripe refusing the read for good leaves the answer unknown, which reads as
// not waiting (the notice then says what it always said); a rate limit or a
// server error is an error, so the webhook is delivered again.
func TestStripeDunningInvoiceActionRequiredRefusedReadIsUnknown(t *testing.T) {
	ctx := context.Background()
	settings := defaultStripeDunningSettings()
	for _, c := range []struct {
		status    int
		wantError bool
	}{
		{status: http.StatusBadRequest, wantError: false},
		{status: http.StatusNotFound, wantError: false},
		{status: http.StatusTooManyRequests, wantError: true},
		{status: http.StatusInternalServerError, wantError: true},
	} {
		api := newStripePaymentActionTestApi(t)
		api.paymentsStatus = c.status
		got, err := stripeDunningInvoiceActionRequired(ctx, "in_synthetic_action", settings)
		if (err != nil) != c.wantError {
			t.Errorf("status %d: error %v, want error %t", c.status, err, c.wantError)
		}
		if got {
			t.Errorf("status %d: a failed read must not read as waiting on the bank", c.status)
		}
	}
}

// A notice of a payment waiting on the bank asks to confirm it: the subject,
// the button and the text part link the invoice's hosted page, where the bank's
// check runs, and nothing says the payment was declined. Without a hosted page
// it sends the customer to Manage Subscription. The final notice says it is the
// last reminder. The SMS stays one segment.
func TestSubscriptionPaymentFailedTemplateAsksToConfirmAPaymentWaitingOnTheBank(t *testing.T) {
	const hostedInvoiceUrl = "https://invoice.stripe.com/i/acct_synthetic/live_synthetic?s=ap"
	nextAttempt := time.Date(2026, 11, 16, 19, 24, 0, 0, time.UTC)

	first := &SubscriptionPaymentFailedTemplate{
		ActionRequired:   true,
		HostedInvoiceUrl: hostedInvoiceUrl,
		AmountDue:        4000,
		Currency:         "usd",
		NextAttemptTime:  &nextAttempt,
	}
	subject, bodyHtml, bodyText, err := RenderEmailTemplate(first)
	if err != nil {
		t.Fatal(err)
	}
	if subject != "Confirm your URnetwork payment" {
		t.Errorf("first subject %q", subject)
	}
	normalText := strings.Join(strings.Fields(bodyText), " ")
	for _, want := range []string{"$40.00", "Your bank needs you to confirm the payment", "Confirm the payment: " + hostedInvoiceUrl, StripeUpdatePaymentMethodUrl} {
		if !strings.Contains(normalText, want) {
			t.Errorf("first text part missing %q:\n%s", want, bodyText)
		}
	}
	for _, want := range []string{`href="` + hostedInvoiceUrl + `"`, "Confirm payment", `href="` + StripeUpdatePaymentMethodUrl + `"`, "$40.00"} {
		if !strings.Contains(bodyHtml, want) {
			t.Errorf("first html part missing %q", want)
		}
	}
	for _, unwanted := range []string{"didn't go through", "Pay invoice", "last reminder"} {
		if strings.Contains(bodyHtml, unwanted) || strings.Contains(normalText, unwanted) {
			t.Errorf("the first reminder to confirm must not say %q", unwanted)
		}
	}

	final := &SubscriptionPaymentFailedTemplate{
		Final:            true,
		ActionRequired:   true,
		HostedInvoiceUrl: hostedInvoiceUrl,
		AmountDue:        500,
		Currency:         "usd",
	}
	subject, bodyHtml, bodyText, err = RenderEmailTemplate(final)
	if err != nil {
		t.Fatal(err)
	}
	if subject != "Final notice: confirm your URnetwork payment" {
		t.Errorf("final subject %q", subject)
	}
	normalText = strings.Join(strings.Fields(bodyText), " ")
	for _, want := range []string{"$5.00", "last reminder", "isn't confirmed", "subscription will end"} {
		if !strings.Contains(bodyHtml, want) || !strings.Contains(normalText, want) {
			t.Errorf("final reminder missing %q", want)
		}
	}

	noHostedPage := &SubscriptionPaymentFailedTemplate{ActionRequired: true, AmountDue: 4000, Currency: "usd"}
	_, bodyHtml, bodyText, err = RenderEmailTemplate(noHostedPage)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(bodyHtml, "invoice.stripe.com") || strings.Contains(bodyText, "invoice.stripe.com") {
		t.Errorf("without a hosted page the email must not link one")
	}
	if !strings.Contains(bodyText, "Confirm the payment: "+StripeUpdatePaymentMethodUrl) ||
		!strings.Contains(bodyHtml, `href="`+StripeUpdatePaymentMethodUrl+`"`) ||
		!strings.Contains(bodyText, "Manage Subscription") {
		t.Errorf("without a hosted page the email must send the customer to Manage Subscription to confirm:\n%s", bodyText)
	}

	sms, err := RenderSmsTemplate(first)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sms, "confirm") || !strings.Contains(sms, "$40.00") || !strings.Contains(sms, StripeUpdatePaymentMethodUrl) || 160 < len(sms) {
		t.Errorf("sms must ask to confirm, name the amount and the link in one segment: %d bytes %q", len(sms), sms)
	}
}
