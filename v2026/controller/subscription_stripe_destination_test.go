package controller

// Hermetic tests (no database) for how invoice.paid resolves its network
// (UPGRADE.md S11): subscription metadata network_id, then the checkout
// session's client_reference_id, and nothing else. The legacy customer-email
// fallback is removed, so a legacy invoice that only shares an email with an
// account is never credited to it; the webhook records it for support and
// answers 2xx so Stripe does not retry it for 72h.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stripe/stripe-go/v82"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// a fixed paid period (2026-09-21 .. 2026-10-21 UTC)
const stripeDestinationTestPeriodStart = int64(1790000000)
const stripeDestinationTestPeriodEnd = int64(1792592000)

// stripeDestinationTestEnv is a fake Stripe API: expanded invoices, checkout
// session listings by subscription, and (for the stripe-go SDK) subscriptions,
// which it does not know.
type stripeDestinationTestEnv struct {
	invoices                     map[string]map[string]any
	subscriptionCheckoutSessions map[string][]map[string]any
	recordedEvents               []*model.PaymentReconciliationEvent
	recordErr                    error
}

func newStripeDestinationTestEnv(t *testing.T) *stripeDestinationTestEnv {
	env := &stripeDestinationTestEnv{
		invoices:                     map[string]map[string]any{},
		subscriptionCheckoutSessions: map[string][]map[string]any{},
	}
	writeJson := func(w http.ResponseWriter, object any) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(object)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/invoices/{invoiceId}", func(w http.ResponseWriter, r *http.Request) {
		invoice, ok := env.invoices[r.PathValue("invoiceId")]
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		writeJson(w, invoice)
	})
	mux.HandleFunc("GET /v1/checkout/sessions", func(w http.ResponseWriter, r *http.Request) {
		checkoutSessions := env.subscriptionCheckoutSessions[r.URL.Query().Get("subscription")]
		if checkoutSessions == nil {
			checkoutSessions = []map[string]any{}
		}
		writeJson(w, map[string]any{"data": checkoutSessions, "has_more": false})
	})
	mux.HandleFunc("GET /v1/subscriptions/{subscriptionId}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		writeJson(w, map[string]any{"error": map[string]any{"type": "invalid_request_error", "message": "not found"}})
	})
	testServer := httptest.NewServer(mux)

	previousBaseUrl := stripeApiBaseUrl
	previousTokenFunc := stripeApiTokenFunc
	previousToken := stripeApiToken
	previousBackend := stripe.GetBackend(stripe.APIBackend)
	previousRecord := stripeRecordUnresolvedInvoice
	stripeApiBaseUrl = testServer.URL
	stripeApiTokenFunc = func() string { return "sk_test_destination" }
	stripeApiToken = func() string { return "sk_test_destination" }
	stripe.SetBackend(stripe.APIBackend, stripe.GetBackendWithConfig(stripe.APIBackend, &stripe.BackendConfig{
		URL:               stripe.String(testServer.URL),
		MaxNetworkRetries: stripe.Int64(0),
	}))
	// the durable record, observed in memory (the once-per-invoice rule is the
	// model's; this fake applies the same rule)
	stripeRecordUnresolvedInvoice = func(ctx context.Context, event *model.PaymentReconciliationEvent) (bool, error) {
		if env.recordErr != nil {
			return false, env.recordErr
		}
		for _, recordedEvent := range env.recordedEvents {
			if recordedEvent.Store == event.Store && recordedEvent.Action == event.Action && recordedEvent.Evidence == event.Evidence {
				return false, nil
			}
		}
		env.recordedEvents = append(env.recordedEvents, event)
		return true, nil
	}
	t.Cleanup(func() {
		stripeApiBaseUrl = previousBaseUrl
		stripeApiTokenFunc = previousTokenFunc
		stripeApiToken = previousToken
		stripe.SetBackend(stripe.APIBackend, previousBackend)
		stripeRecordUnresolvedInvoice = previousRecord
		testServer.Close()
	})
	return env
}

// stripeDestinationTestInvoice is a complete expanded subscription invoice
// whose subscription carries the given metadata.
func stripeDestinationTestInvoice(invoiceId string, subscriptionId string, customerEmail string, metadata map[string]any) map[string]any {
	return map[string]any{
		"id":       invoiceId,
		"customer": map[string]any{"id": "cus_destination_legacy", "email": customerEmail},
		"lines": map[string]any{"data": []map[string]any{{
			"type":         "subscription",
			"subscription": subscriptionId,
			"period": map[string]any{
				"start": stripeDestinationTestPeriodStart,
				"end":   stripeDestinationTestPeriodEnd,
			},
		}}},
		"subscription": map[string]any{"id": subscriptionId, "metadata": metadata},
	}
}

func stripeDestinationTestSession() *session.ClientSession {
	return session.NewLocalClientSession(context.Background(), "0.0.0.0:0", nil)
}

// The metadata path is unchanged: network_id on the subscription names the
// network, without consulting checkout sessions.
func TestStripeResolveInvoiceCreditUsesSubscriptionMetadata(t *testing.T) {
	env := newStripeDestinationTestEnv(t)
	networkId := server.NewId()
	env.invoices["in_destination_metadata"] = stripeDestinationTestInvoice(
		"in_destination_metadata", "sub_destination_metadata", "payer@example.invalid",
		map[string]any{"network_id": networkId.String()},
	)
	// a conflicting checkout reference must not matter when metadata exists
	env.subscriptionCheckoutSessions["sub_destination_metadata"] = []map[string]any{
		{"id": "cs_destination_other", "client_reference_id": server.NewId().String()},
	}

	credit, err := stripeResolveInvoiceCredit("in_destination_metadata", stripeDestinationTestSession())
	connect.AssertEqual(t, err, nil)
	connect.AssertNotEqual(t, credit, nil)
	connect.AssertEqual(t, credit.networkId, networkId)
	connect.AssertEqual(t, credit.subscriptionId, "sub_destination_metadata")
	connect.AssertEqual(t, credit.startTime.Unix(), stripeDestinationTestPeriodStart)
}

// The checkout path is unchanged: without metadata, the checkout session's
// client_reference_id names the network.
func TestStripeResolveInvoiceCreditUsesCheckoutClientReference(t *testing.T) {
	env := newStripeDestinationTestEnv(t)
	networkId := server.NewId()
	env.invoices["in_destination_checkout"] = stripeDestinationTestInvoice(
		"in_destination_checkout", "sub_destination_checkout", "payer@example.invalid", map[string]any{},
	)
	env.subscriptionCheckoutSessions["sub_destination_checkout"] = []map[string]any{
		{"id": "cs_destination_checkout", "client_reference_id": networkId.String()},
	}

	credit, err := stripeResolveInvoiceCredit("in_destination_checkout", stripeDestinationTestSession())
	connect.AssertEqual(t, err, nil)
	connect.AssertNotEqual(t, credit, nil)
	connect.AssertEqual(t, credit.networkId, networkId)
	connect.AssertEqual(t, credit.subscriptionId, "sub_destination_checkout")
}

// S11 root cause: a legacy invoice with no metadata and no checkout reference
// resolved its network by looking up an account with the Stripe customer's
// email. Now it is unresolved, carrying what support needs, and resolving it
// performs no account lookup at all (this test has no database: the removed
// lookup panicked here with "resource not found in vault (pg.yml)").
func TestStripeResolveInvoiceCreditNeverUsesCustomerEmail(t *testing.T) {
	env := newStripeDestinationTestEnv(t)
	env.invoices["in_destination_email"] = stripeDestinationTestInvoice(
		"in_destination_email", "sub_destination_email", "legacy-payer@example.invalid", map[string]any{},
	)
	env.subscriptionCheckoutSessions["sub_destination_email"] = []map[string]any{
		{"id": "cs_destination_email", "client_reference_id": nil},
	}

	credit, err := stripeResolveInvoiceCredit("in_destination_email", stripeDestinationTestSession())
	if credit != nil || !errors.Is(err, errStripeInvoiceDestinationUnresolved) {
		t.Fatalf("legacy invoice with only a customer email must be unresolved: credit=%v err=%v", credit, err)
	}
	var unresolved *stripeInvoiceDestinationUnresolvedError
	connect.AssertEqual(t, errors.As(err, &unresolved), true)
	details := unresolved.details()
	connect.AssertEqual(t, details["reason"], "destination_unresolved")
	connect.AssertEqual(t, details["invoice"], "in_destination_email")
	connect.AssertEqual(t, details["subscription"], "sub_destination_email")
	connect.AssertEqual(t, details["customer"], "cus_destination_legacy")
	connect.AssertEqual(t, details["customer_email"], "legacy-payer@example.invalid")
	connect.AssertEqual(t, details["period_start"], time.Unix(stripeDestinationTestPeriodStart, 0).UTC().Format(time.RFC3339))
	connect.AssertEqual(t, details["period_end"], time.Unix(stripeDestinationTestPeriodEnd, 0).UTC().Format(time.RFC3339))
}

func stripeDestinationTestInvoicePaidEvent(t *testing.T, invoiceId string) *StripeWebhookArgs {
	objectJson, err := json.Marshal(map[string]any{
		"id":       invoiceId,
		"total":    500,
		"currency": "usd",
		"customer": "cus_destination_legacy",
	})
	connect.AssertEqual(t, err, nil)
	return &StripeWebhookArgs{
		Id:   "evt_" + server.NewId().String(),
		Type: "invoice.paid",
		Data: &StripeEventData{Object: objectJson},
	}
}

// An invoice.paid that names no network is answered 2xx (no 72h retry storm
// that could get the endpoint disabled) and recorded once for support with the
// invoice, subscription, customer, email, amount and period. A redelivery is
// answered 2xx without a second record.
func TestStripeWebhookUnresolvedInvoiceIsRecordedAndAcknowledged(t *testing.T) {
	env := newStripeDestinationTestEnv(t)
	for _, customerEmail := range []string{"", "legacy-payer@example.invalid"} {
		invoiceId := "in_destination_webhook_" + server.NewId().String()
		env.invoices[invoiceId] = stripeDestinationTestInvoice(invoiceId, "sub_destination_webhook", customerEmail, map[string]any{})

		for delivery := 0; delivery < 2; delivery++ {
			result, err := StripeWebhook(stripeDestinationTestInvoicePaidEvent(t, invoiceId), stripeDestinationTestSession())
			if err != nil || result == nil {
				t.Fatalf("unresolved invoice.paid must be acknowledged (2xx) so Stripe does not retry for 72h: result=%v err=%v", result, err)
			}
		}

		var events []*model.PaymentReconciliationEvent
		for _, event := range env.recordedEvents {
			if event.Evidence == invoiceId {
				events = append(events, event)
			}
		}
		connect.AssertEqual(t, len(events), 1)
		event := events[0]
		connect.AssertEqual(t, event.Store, model.SubscriptionMarketStripe)
		connect.AssertEqual(t, event.Action, model.PaymentReconcileActionCreditUnfulfillable)
		// credited to no one
		connect.AssertEqual(t, event.NetworkId, nil)
		connect.AssertEqual(t, event.DryRun, false)
		connect.AssertEqual(t, event.Details["reason"], "destination_unresolved")
		connect.AssertEqual(t, event.Details["leg"], "webhook")
		connect.AssertEqual(t, event.Details["invoice"], invoiceId)
		connect.AssertEqual(t, event.Details["subscription"], "sub_destination_webhook")
		connect.AssertEqual(t, event.Details["customer"], "cus_destination_legacy")
		connect.AssertEqual(t, event.Details["customer_email"], customerEmail)
		connect.AssertEqual(t, event.Details["amount_total"], 500)
		connect.AssertEqual(t, event.Details["currency"], "usd")
		connect.AssertEqual(t, event.Details["period_start"], time.Unix(stripeDestinationTestPeriodStart, 0).UTC().Format(time.RFC3339))
		connect.AssertEqual(t, event.Details["period_end"], time.Unix(stripeDestinationTestPeriodEnd, 0).UTC().Format(time.RFC3339))
	}
}

// If the support record cannot be written the delivery fails (non-2xx), so
// Stripe redelivers until the payment is on file rather than it being lost.
func TestStripeWebhookUnresolvedInvoiceRecordFailureIsRetried(t *testing.T) {
	env := newStripeDestinationTestEnv(t)
	env.recordErr = errors.New("synthetic record failure")
	env.invoices["in_destination_record_failure"] = stripeDestinationTestInvoice(
		"in_destination_record_failure", "sub_destination_record_failure", "legacy-payer@example.invalid", map[string]any{},
	)

	result, err := StripeWebhook(stripeDestinationTestInvoicePaidEvent(t, "in_destination_record_failure"), stripeDestinationTestSession())
	connect.AssertEqual(t, result, nil)
	connect.AssertNotEqual(t, err, nil)
	connect.AssertEqual(t, len(env.recordedEvents), 0)
}
