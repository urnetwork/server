package controller

// Hermetic tests of when a checkout is fulfilled
// (subscription_stripe_checkout_payment.go): a session whose payment has not
// cleared is not fulfilled, so nothing of its fulfilment runs, and a delayed
// payment that failed is recorded once and fulfils nothing. No database: a
// fake Stripe API on the raw-HTTP seam, and the record's seam. The fulfilment
// through the ledger, exactly once, is in
// subscription_stripe_checkout_payment_db_test.go.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// The product a test session's line sells.
const stripeCheckoutPaymentTestProduct = "prod_synthetic_checkout_payment"

// A fake Stripe API for checkout fulfilment: the line items of every session,
// one line of the test product, and how many times they were read. Safe for
// concurrent use.
type stripeCheckoutPaymentTestApi struct {
	stateLock     sync.Mutex
	lineItemReads int
}

// Installs the fake API and a sku for the test product: supporter, which the
// fulfilment leaves to invoice.paid (no database), unless dataPack.
func newStripeCheckoutPaymentTestApi(t testing.TB, dataPack bool) *stripeCheckoutPaymentTestApi {
	api := &stripeCheckoutPaymentTestApi{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/checkout/sessions/{sessionId}/line_items", func(w http.ResponseWriter, r *http.Request) {
		api.stateLock.Lock()
		api.lineItemReads += 1
		api.stateLock.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"object": "list",
			"data": []any{map[string]any{
				"id":           "li_" + r.PathValue("sessionId"),
				"object":       "item",
				"amount_total": 500,
				"currency":     "usd",
				"quantity":     1,
				"price":        map[string]any{"id": "price_synthetic", "product": stripeCheckoutPaymentTestProduct, "unit_amount": 500},
			}},
			"has_more": false,
		})
	})
	testServer := httptest.NewServer(mux)
	previousBaseUrl := stripeApiBaseUrl
	previousTokenFunc := stripeApiTokenFunc
	previousSkus := stripeSkusFunc
	stripeApiBaseUrl = testServer.URL
	stripeApiTokenFunc = func() string { return "sk_test_checkout_payment" }
	stripeSkusFunc = func() map[string]*Sku {
		return map[string]*Sku{
			stripeCheckoutPaymentTestProduct: {
				Supporter:                     !dataPack,
				FeeFraction:                   0.3,
				BalanceByteCountHumanReadable: "1tib",
			},
		}
	}
	t.Cleanup(func() {
		stripeApiBaseUrl = previousBaseUrl
		stripeApiTokenFunc = previousTokenFunc
		stripeSkusFunc = previousSkus
		testServer.Close()
	})
	return api
}

// How many times the line items were read: the first step of a fulfilment.
func (self *stripeCheckoutPaymentTestApi) reads() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.lineItemReads
}

// A data pack checkout session as Stripe's checkout events carry it, made for
// the network.
func stripeCheckoutPaymentTestSession(sessionId string, paymentStatus string, networkId server.Id) map[string]any {
	return map[string]any{
		"id":                  sessionId,
		"object":              "checkout.session",
		"mode":                "payment",
		"status":              "complete",
		"payment_status":      paymentStatus,
		"client_reference_id": networkId.String(),
		"amount_total":        500,
		"customer_details":    map[string]any{"email": "buyer@example.invalid"},
	}
}

// A checkout that completes before its money arrives (a bank debit or
// transfer) is not fulfilled: nothing of the fulfilment runs, not even the
// read of what it bought, and the delivery is answered 200 so Stripe does not
// retry it. Before, checkout.session.completed fulfilled every session.
func TestStripeCheckoutNotPaidStartsNoFulfilment(t *testing.T) {
	api := newStripeCheckoutPaymentTestApi(t, false)
	clientSession := session.Testing_CreateClientSession(context.Background(), nil)
	networkId := server.NewId()
	for _, c := range []struct {
		eventType     string
		paymentStatus string
	}{
		{eventType: "checkout.session.completed", paymentStatus: "unpaid"},
		{eventType: "checkout.session.completed", paymentStatus: "no_payment_required"},
		{eventType: "checkout.session.completed", paymentStatus: ""},
		// an out-of-order or inconsistent success event still needs the money
		{eventType: "checkout.session.async_payment_succeeded", paymentStatus: "unpaid"},
	} {
		result, err := StripeWebhook(stripeWebhookEvent(t, c.eventType, stripeCheckoutPaymentTestSession("cs_synthetic_unpaid", c.paymentStatus, networkId)), clientSession)
		if err != nil || result == nil {
			t.Fatalf("%s %q: %+v %v", c.eventType, c.paymentStatus, result, err)
		}
		if reads := api.reads(); reads != 0 {
			t.Fatalf("%s %q started a fulfilment (%d line item reads) before the payment cleared", c.eventType, c.paymentStatus, reads)
		}
	}

	// a paid session does start it
	result, err := StripeWebhook(stripeWebhookEvent(t, "checkout.session.completed", stripeCheckoutPaymentTestSession("cs_synthetic_paid", "paid", networkId)), clientSession)
	if err != nil || result == nil {
		t.Fatalf("checkout.session.completed paid: %+v %v", result, err)
	}
	if reads := api.reads(); reads != 1 {
		t.Fatalf("a paid session must be fulfilled, got %d line item reads", reads)
	}
}

// checkout.session.async_payment_succeeded fulfils a delayed payment that
// cleared, through the same code as an immediate one.
func TestStripeCheckoutAsyncPaymentSucceededStartsTheFulfilment(t *testing.T) {
	api := newStripeCheckoutPaymentTestApi(t, false)
	clientSession := session.Testing_CreateClientSession(context.Background(), nil)
	result, err := StripeWebhook(stripeWebhookEvent(t, "checkout.session.async_payment_succeeded", stripeCheckoutPaymentTestSession("cs_synthetic_async_paid", "paid", server.NewId())), clientSession)
	if err != nil || result == nil {
		t.Fatalf("checkout.session.async_payment_succeeded: %+v %v", result, err)
	}
	if reads := api.reads(); reads != 1 {
		t.Fatalf("a delayed payment that cleared must be fulfilled, got %d line item reads", reads)
	}
}

// checkout.session.async_payment_failed fulfils nothing and records the
// failure for support: the session, its network and the action; a record that
// cannot be written fails the delivery, so Stripe delivers it again.
func TestStripeCheckoutAsyncPaymentFailedRecordsAndFulfilsNothing(t *testing.T) {
	api := newStripeCheckoutPaymentTestApi(t, true)
	clientSession := session.Testing_CreateClientSession(context.Background(), nil)
	networkId := server.NewId()

	recorded := []*model.PaymentReconciliationEvent{}
	recordErr := error(nil)
	previousRecord := stripeRecordCheckoutPaymentFailed
	stripeRecordCheckoutPaymentFailed = func(ctx context.Context, event *model.PaymentReconciliationEvent) (bool, error) {
		if recordErr != nil {
			return false, recordErr
		}
		recorded = append(recorded, event)
		return true, nil
	}
	t.Cleanup(func() {
		stripeRecordCheckoutPaymentFailed = previousRecord
	})

	event := stripeWebhookEvent(t, "checkout.session.async_payment_failed", stripeCheckoutPaymentTestSession("cs_synthetic_async_failed", "unpaid", networkId))
	result, err := StripeWebhook(event, clientSession)
	if err != nil || result == nil {
		t.Fatalf("checkout.session.async_payment_failed: %+v %v", result, err)
	}
	if reads := api.reads(); reads != 0 {
		t.Fatalf("a failed delayed payment must fulfil nothing, got %d line item reads", reads)
	}
	if len(recorded) != 1 {
		t.Fatalf("the failure must be recorded once, got %d records", len(recorded))
	}
	record := recorded[0]
	if record.Store != model.SubscriptionMarketStripe ||
		record.Action != model.PaymentReconcileActionPaymentFailed ||
		record.Evidence != "cs_synthetic_async_failed" ||
		record.NetworkId == nil || *record.NetworkId != networkId {
		t.Fatalf("the failure record is %+v", record)
	}

	recordErr = errors.New("synthetic database failure")
	if result, err := StripeWebhook(event, clientSession); err == nil {
		t.Fatalf("a failure record that cannot be written must fail the delivery, got %+v", result)
	}
}
