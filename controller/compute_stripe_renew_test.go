//go:build sidecar_pending

package controller

// Auto-renew's Stripe against a fake Stripe (BAL-M12-11; DESIGN-COMPUTE §9.5, §9.8), the stand-in for CTEST's
// computetest.FakeStores until it lands: a top-up sent off-session on the payer's saved card under the job's key, its
// card errors read as answers; the monthly plan's Checkout in subscription mode carrying its metadata onto the
// subscription; and the renewals' webhook events, a top-up's success, a plan's completion, paid invoice and deletion,
// each taken by compute's branch, crediting once however often delivered, so none reaches Pro's handlers. Under
// -tags sidecar_pending, since the ledger's tables are MG-C1's.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	operator "github.com/urnetwork/operator-core"
	"github.com/urnetwork/sidecar"
	sidecarcontroller "github.com/urnetwork/sidecar/controller"
	"github.com/urnetwork/sidecar/model"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/core"
	"github.com/urnetwork/server/sidecarhost"
)

// A fake Stripe for auto-renew's calls: PaymentIntents made and read, a planned card error, payment methods and their
// detach, Checkout sessions, customers, subscriptions and their cancel, invoice payments and refunds.
type testComputeRenewFake struct {
	stateLock     sync.Mutex
	intents       map[string]map[string]any
	intentForms   map[string]url.Values
	keys          map[string]string
	cardError     string
	sessionForms  []url.Values
	subscriptions map[string]string
	cancelled     []string
	detached      []string
	invoiceIntent map[string]string
	refunds       []url.Values
	server        *httptest.Server
}

func newTestComputeRenewFake(t testing.TB) *testComputeRenewFake {
	fake := &testComputeRenewFake{
		intents: map[string]map[string]any{}, intentForms: map[string]url.Values{}, keys: map[string]string{},
		subscriptions: map[string]string{}, invoiceIntent: map[string]string{},
	}
	writeJson := func(w http.ResponseWriter, status int, object any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(object)
	}
	form := func(r *http.Request) url.Values {
		body, _ := io.ReadAll(r.Body)
		values, _ := url.ParseQuery(string(body))
		return values
	}
	metadataOf := func(values url.Values) map[string]any {
		metadata := map[string]any{}
		for key, value := range values {
			if strings.HasPrefix(key, "metadata[") {
				metadata[strings.TrimSuffix(strings.TrimPrefix(key, "metadata["), "]")] = value[0]
			}
		}
		return metadata
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/payment_intents", func(w http.ResponseWriter, r *http.Request) {
		values := form(r)
		fake.stateLock.Lock()
		defer fake.stateLock.Unlock()
		if id, ok := fake.keys[r.Header.Get("Idempotency-Key")]; ok {
			writeJson(w, http.StatusOK, fake.intents[id])
			return
		}
		id := fmt.Sprintf("pi_synthetic_renew_%d", len(fake.intents)+1)
		amount := values.Get("amount")
		intent := map[string]any{
			"id": id, "object": "payment_intent", "status": "succeeded", "amount": json.Number(amount), "amount_received": json.Number(amount),
			"currency": values.Get("currency"), "metadata": metadataOf(values),
			"payment_method": map[string]any{"id": values.Get("payment_method"), "object": "payment_method", "card": map[string]any{"fingerprint": "fp_synthetic_renew"}},
		}
		fake.intents[id] = intent
		fake.intentForms[id] = values
		fake.keys[r.Header.Get("Idempotency-Key")] = id
		if fake.cardError != "" {
			intent["status"] = "requires_payment_method"
			writeJson(w, http.StatusPaymentRequired, map[string]any{"error": map[string]any{
				"type": "card_error", "code": fake.cardError, "message": "synthetic", "payment_intent": intent,
			}})
			return
		}
		writeJson(w, http.StatusOK, intent)
	})
	mux.HandleFunc("GET /v1/payment_intents/{intentId}", func(w http.ResponseWriter, r *http.Request) {
		fake.stateLock.Lock()
		defer fake.stateLock.Unlock()
		intent, ok := fake.intents[r.PathValue("intentId")]
		if !ok {
			writeJson(w, http.StatusNotFound, map[string]any{"error": map[string]any{"type": "invalid_request_error", "code": "resource_missing"}})
			return
		}
		writeJson(w, http.StatusOK, intent)
	})
	mux.HandleFunc("GET /v1/payment_methods/{methodId}", func(w http.ResponseWriter, r *http.Request) {
		writeJson(w, http.StatusOK, map[string]any{
			"id": r.PathValue("methodId"), "object": "payment_method", "customer": map[string]any{"id": "cus_synthetic_renew", "object": "customer"},
			"card": map[string]any{"fingerprint": "fp_synthetic_" + r.PathValue("methodId")},
		})
	})
	mux.HandleFunc("POST /v1/payment_methods/{methodId}/detach", func(w http.ResponseWriter, r *http.Request) {
		fake.stateLock.Lock()
		defer fake.stateLock.Unlock()
		fake.detached = append(fake.detached, r.PathValue("methodId"))
		writeJson(w, http.StatusOK, map[string]any{"id": r.PathValue("methodId"), "object": "payment_method"})
	})
	mux.HandleFunc("GET /v1/customers/search", func(w http.ResponseWriter, r *http.Request) {
		writeJson(w, http.StatusOK, map[string]any{"object": "search_result", "data": []any{}, "has_more": false})
	})
	mux.HandleFunc("POST /v1/customers", func(w http.ResponseWriter, r *http.Request) {
		writeJson(w, http.StatusOK, map[string]any{"id": "cus_synthetic_renew", "object": "customer", "metadata": metadataOf(form(r))})
	})
	mux.HandleFunc("POST /v1/checkout/sessions", func(w http.ResponseWriter, r *http.Request) {
		values := form(r)
		fake.stateLock.Lock()
		defer fake.stateLock.Unlock()
		fake.sessionForms = append(fake.sessionForms, values)
		writeJson(w, http.StatusOK, map[string]any{
			"id": fmt.Sprintf("cs_synthetic_renew_%d", len(fake.sessionForms)), "object": "checkout.session",
			"url": "https://checkout.stripe.com/c/pay/cs_synthetic_renew", "expires_at": 4102444800,
		})
	})
	mux.HandleFunc("GET /v1/subscriptions/{subscriptionId}", func(w http.ResponseWriter, r *http.Request) {
		fake.stateLock.Lock()
		defer fake.stateLock.Unlock()
		status, ok := fake.subscriptions[r.PathValue("subscriptionId")]
		if !ok {
			writeJson(w, http.StatusNotFound, map[string]any{"error": map[string]any{"type": "invalid_request_error", "code": "resource_missing"}})
			return
		}
		writeJson(w, http.StatusOK, map[string]any{"id": r.PathValue("subscriptionId"), "object": "subscription", "status": status})
	})
	mux.HandleFunc("DELETE /v1/subscriptions/{subscriptionId}", func(w http.ResponseWriter, r *http.Request) {
		fake.stateLock.Lock()
		defer fake.stateLock.Unlock()
		fake.subscriptions[r.PathValue("subscriptionId")] = "canceled"
		fake.cancelled = append(fake.cancelled, r.PathValue("subscriptionId"))
		writeJson(w, http.StatusOK, map[string]any{"id": r.PathValue("subscriptionId"), "object": "subscription", "status": "canceled"})
	})
	mux.HandleFunc("GET /v1/invoice_payments", func(w http.ResponseWriter, r *http.Request) {
		fake.stateLock.Lock()
		defer fake.stateLock.Unlock()
		data := []any{}
		if intentId, ok := fake.invoiceIntent[r.URL.Query().Get("invoice")]; ok {
			data = append(data, map[string]any{
				"id": "inpay_synthetic", "object": "invoice_payment", "status": "paid",
				"payment": map[string]any{"type": "payment_intent", "payment_intent": map[string]any{"id": intentId, "object": "payment_intent"}},
			})
		}
		writeJson(w, http.StatusOK, map[string]any{"object": "list", "data": data, "has_more": false})
	})
	mux.HandleFunc("POST /v1/refunds", func(w http.ResponseWriter, r *http.Request) {
		values := form(r)
		fake.stateLock.Lock()
		defer fake.stateLock.Unlock()
		fake.refunds = append(fake.refunds, values)
		writeJson(w, http.StatusOK, map[string]any{"id": fmt.Sprintf("re_synthetic_renew_%d", len(fake.refunds)), "object": "refund", "status": "succeeded"})
	})
	fake.server = httptest.NewServer(mux)
	t.Cleanup(fake.server.Close)
	return fake
}

// An instance of compute's Stripe at fake, active, over the test's database and Redis.
func newTestComputeRenew(t testing.TB) (*ComputeServerStripe, *testComputeRenewFake, operator.Env) {
	fake := newTestComputeRenewFake(t)
	env, err := core.NewEnv(&sidecar.ServiceHost{Redis: sidecarhost.NewRedis(nil), Metrics: sidecartest.NewFakeMetrics(nil), Clock: sidecartest.NewFakeClock(server.NowUtc())}, &core.Grants{Db: true})
	if err != nil {
		t.Fatal(err)
	}
	settings := DefaultComputeStripeSettings()
	settings.ApiUrl = fake.server.URL
	instance := NewComputeServerStripe(settings, &ComputeStripeDeps{
		ApiKey: func() (string, error) { return "sk_test_synthetic_renew", nil },
		Config: func() (*ComputeStripeConfig, error) {
			return &ComputeStripeConfig{ProductId: testComputeStripeProduct, SuccessUrl: "https://ur.example/compute/success", CancelUrl: "https://ur.example/compute/cancel"}, nil
		},
		Env:     func() operator.Env { return env },
		Secrets: func() sidecarcontroller.ComputeLedgerSecrets { return testComputeSecrets{} },
		Ledger:  model.DefaultComputeLedgerSettings(),
		Active:  func() bool { return true },
	})
	return instance, fake, env
}

// A top-up is a PaymentIntent of P on the saved card, confirm=true and off_session=true, card payments only, under the
// job's key with its renewal's metadata; Stripe's answers read as the job's outcomes: success, a decline as failed, the
// bank's ask as authentication_required, and the same key twice one charge.
func TestComputeChargeOffSessionSendsTheTopUp(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		instance, fake, _ := newTestComputeRenew(tb)
		ctx := context.Background()
		networkId, payerId := server.NewId(), server.NewId()
		charge := &sidecarcontroller.ComputeOffSessionCharge{
			NetworkId: networkId, PayerMemberId: payerId, StripeCustomerId: "cus_synthetic_renew", StripePaymentMethodId: "pm_synthetic_renew",
			PackCredits: 20, AmountNanoUsd: 20 * model.ComputeCreditNanoUsd, IdempotencyKey: "compute_top_up_synthetic_1",
		}
		answer, err := instance.ChargeOffSession(ctx, charge)
		if err != nil || answer.Outcome != sidecarcontroller.ComputeOffSessionSucceeded || answer.PaymentIntentId == "" {
			tb.Fatalf("a top-up: %+v %v", answer, err)
		}
		form := fake.intentForms[answer.PaymentIntentId]
		for key, want := range map[string]string{
			"amount": "2000", "currency": "usd", "customer": "cus_synthetic_renew", "payment_method": "pm_synthetic_renew",
			"confirm": "true", "off_session": "true", "payment_method_types[0]": "card",
			"metadata[urnetwork_compute]": "1", "metadata[compute_renewal]": "threshold", "metadata[compute_pack_credits]": "20",
			"metadata[compute_network_id]": networkId.String(), "metadata[compute_member_id]": payerId.String(),
		} {
			if form.Get(key) != want {
				tb.Errorf("%s = %q, want %q", key, form.Get(key), want)
			}
		}
		if form.Has("metadata[compute_auto_renew]") || form.Has("setup_future_usage") {
			tb.Error("a top-up names a pack checkout's auto-renew or saves a card again")
		}
		again, err := instance.ChargeOffSession(ctx, charge)
		if err != nil || again.PaymentIntentId != answer.PaymentIntentId || len(fake.intents) != 1 {
			tb.Fatalf("the same key again: %+v %v", again, err)
		}
		for code, want := range map[string]sidecarcontroller.ComputeOffSessionOutcome{
			"card_declined":           sidecarcontroller.ComputeOffSessionFailed,
			"authentication_required": sidecarcontroller.ComputeOffSessionAuthenticationRequired,
		} {
			fake.cardError = code
			charge.IdempotencyKey = "compute_top_up_synthetic_" + code
			answer, err := instance.ChargeOffSession(ctx, charge)
			if err != nil || answer.Outcome != want || answer.PaymentIntentId == "" {
				tb.Errorf("%s: %+v %v", code, answer, err)
			}
		}
	})
}

// The monthly plan opens as a Checkout session in subscription mode: M credits a month at a dollar a credit, tax added,
// its subscription's metadata naming the network, the payer and M.
func TestComputeMonthlyCheckoutOpensASubscription(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		instance, fake, _ := newTestComputeRenew(tb)
		networkId, payerId := server.NewId(), server.NewId()
		session, err := instance.MonthlyCheckout(context.Background(), &sidecarcontroller.ComputeStripeMonthlyRequest{
			NetworkId: networkId, MemberId: payerId, Credits: 25, AmountNanoUsd: 25 * model.ComputeCreditNanoUsd,
		})
		if err != nil || session.Url == "" || len(fake.sessionForms) != 1 {
			tb.Fatalf("the plan's session: %+v %v", session, err)
		}
		form := fake.sessionForms[0]
		for key, want := range map[string]string{
			"mode": "subscription", "customer": "cus_synthetic_renew", "automatic_tax[enabled]": "true",
			"line_items[0][price_data][unit_amount]": "2500", "line_items[0][price_data][recurring][interval]": "month",
			"line_items[0][price_data][product]": testComputeStripeProduct, "line_items[0][quantity]": "1",
			"subscription_data[metadata][compute_renewal]": "monthly", "subscription_data[metadata][compute_monthly_credits]": "25",
			"subscription_data[metadata][compute_network_id]": networkId.String(), "subscription_data[metadata][urnetwork_compute]": "1",
			"metadata[compute_renewal]": "monthly",
		} {
			if form.Get(key) != want {
				tb.Errorf("%s = %q, want %q", key, form.Get(key), want)
			}
		}
	})
}

// An event of the webhook's, its object JSON.
func testComputeRenewEvent(kind string, object map[string]any) *StripeWebhookArgs {
	data, _ := json.Marshal(object)
	return &StripeWebhookArgs{Id: "evt_synthetic_" + kind, Type: kind, Data: &StripeEventData{Object: data}}
}

// The renewals' events are compute's branch's: a top-up's success credits its lot once however often delivered; a
// plan's completion turns monthly on, its paid invoice credits its month once by the invoice's PaymentIntent, and its
// deletion turns monthly off; an invoice of no plan of ours goes on to Pro's handlers.
func TestComputeRenewalEventsCreditOnceAndReachNoProHandler(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		instance, fake, env := newTestComputeRenew(tb)
		ctx := context.Background()
		ledger := model.DefaultComputeLedgerSettings()
		networkId, payerId := server.NewId(), server.NewId()
		deliver := func(event *StripeWebhookArgs) {
			tb.Helper()
			handled, _, err := instance.Webhook(ctx, event)
			if err != nil || !handled {
				tb.Fatalf("%s: handled %t, %v", event.Type, handled, err)
			}
		}
		// a top-up sent by the job, its success delivered twice
		answer, err := instance.ChargeOffSession(ctx, &sidecarcontroller.ComputeOffSessionCharge{
			NetworkId: networkId, PayerMemberId: payerId, StripeCustomerId: "cus_synthetic_renew", StripePaymentMethodId: "pm_synthetic_renew",
			PackCredits: 20, AmountNanoUsd: 20 * model.ComputeCreditNanoUsd, IdempotencyKey: "compute_top_up_synthetic_event",
		})
		if err != nil {
			tb.Fatal(err)
		}
		intentEvent := testComputeRenewEvent("payment_intent.succeeded", fake.intents[answer.PaymentIntentId])
		deliver(intentEvent)
		deliver(intentEvent)
		lot, err := instance.lotOf(ctx, answer.PaymentIntentId)
		if err != nil || lot == nil || lot.AmountNanoUsd != 20*model.ComputeCreditNanoUsd || lot.Kind != model.ComputeLotKindPurchase {
			tb.Fatalf("the top-up's lot: %+v %v", lot, err)
		}
		// the plan
		metadata := map[string]any{
			"urnetwork_compute": "1", "compute_renewal": "monthly", "compute_monthly_credits": "25",
			"compute_network_id": networkId.String(), "compute_member_id": payerId.String(),
		}
		deliver(testComputeRenewEvent("checkout.session.completed", map[string]any{
			"id": "cs_synthetic_plan", "object": "checkout.session", "mode": "subscription", "metadata": metadata,
			"subscription": "sub_synthetic_plan", "customer": "cus_synthetic_renew",
		}))
		state, err := sidecarcontroller.ComputeAutoRenew(env, ctx, ledger, networkId)
		if err != nil || !state.Monthly.On || state.Monthly.Credits != 25 || state.Monthly.PayerMemberId == nil || *state.Monthly.PayerMemberId != payerId {
			tb.Fatalf("the plan: %+v %v", state, err)
		}
		fake.stateLock.Lock()
		fake.intents["pi_synthetic_invoice"] = map[string]any{
			"id": "pi_synthetic_invoice", "object": "payment_intent", "status": "succeeded", "amount": 2500, "currency": "usd",
			"payment_method": map[string]any{"id": "pm_synthetic_renew", "object": "payment_method", "card": map[string]any{"fingerprint": "fp_synthetic_renew"}},
		}
		fake.invoiceIntent["in_synthetic_plan"] = "pi_synthetic_invoice"
		fake.subscriptions["sub_synthetic_plan"] = "active"
		fake.stateLock.Unlock()
		invoice := testComputeRenewEvent("invoice.paid", map[string]any{
			"id": "in_synthetic_plan", "object": "invoice", "subtotal": 2500, "currency": "usd",
			"parent": map[string]any{"type": "subscription_details", "subscription_details": map[string]any{"metadata": metadata, "subscription": "sub_synthetic_plan"}},
		})
		deliver(invoice)
		deliver(invoice)
		month, err := instance.lotOf(ctx, "pi_synthetic_invoice")
		if err != nil || month == nil || month.Kind != model.ComputeLotKindSubscription || month.AmountNanoUsd != 25*model.ComputeCreditNanoUsd {
			tb.Fatalf("the month's lot: %+v %v", month, err)
		}
		deliver(testComputeRenewEvent("customer.subscription.deleted", map[string]any{
			"id": "sub_synthetic_plan", "object": "subscription", "metadata": metadata, "status": "canceled",
		}))
		if state, err := sidecarcontroller.ComputeAutoRenew(env, ctx, ledger, networkId); err != nil || state.Monthly.On {
			tb.Fatalf("after the plan's deletion: %+v %v", state, err)
		}
		// an invoice of no plan of ours is Pro's
		handled, _, err := instance.Webhook(ctx, testComputeRenewEvent("invoice.paid", map[string]any{
			"id": "in_synthetic_pro", "object": "invoice", "subtotal": 4000, "currency": "usd",
			"parent": map[string]any{"type": "subscription_details", "subscription_details": map[string]any{"metadata": map[string]any{"network_id": networkId.String()}, "subscription": "sub_synthetic_pro"}},
		}))
		if handled || err != nil {
			tb.Fatalf("Pro's invoice: handled %t, %v", handled, err)
		}
	})
}
