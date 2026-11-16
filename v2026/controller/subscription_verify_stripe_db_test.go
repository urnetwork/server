package controller

// Root-cause tests, against the database, of support inbox 7247 for card
// payments: a paid Stripe checkout credited Pro only when invoice.paid
// arrived (or the hourly reconcile ran). The app had nothing to report, so a
// late webhook kept a paying customer on Free. The report
// (POST /subscription/verify-stripe-checkout) now credits it through the
// invoice.paid handler and its stripe_invoice ledger, and the webhook's late
// delivery credits nothing more. Stripe is a fake api server, read through the
// raw-HTTP seam and the stripe-go backend alike. Needs the test database and
// redis (server.DefaultTestEnv).

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stripe/stripe-go/v82"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// What the fake Stripe api answers.
type stripeVerifyDbFake struct {
	stateLock     sync.Mutex
	sessions      map[string]map[string]any
	subscriptions map[string]map[string]any
	invoices      map[string]map[string]any
}

// A fake Stripe api, through the raw-HTTP seam and the stripe-go backend,
// restored when the test ends.
func newStripeVerifyDbFake(t testing.TB) *stripeVerifyDbFake {
	fake := &stripeVerifyDbFake{
		sessions:      map[string]map[string]any{},
		subscriptions: map[string]map[string]any{},
		invoices:      map[string]map[string]any{},
	}
	serveFrom := func(objects func() map[string]map[string]any) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			fake.stateLock.Lock()
			object, ok := objects()[r.PathValue("id")]
			fake.stateLock.Unlock()
			w.Header().Set("Content-Type", "application/json")
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"type": "invalid_request_error", "message": "No such object"}})
				return
			}
			json.NewEncoder(w).Encode(object)
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/checkout/sessions/{id}", serveFrom(func() map[string]map[string]any { return fake.sessions }))
	mux.HandleFunc("GET /v1/subscriptions/{id}", serveFrom(func() map[string]map[string]any { return fake.subscriptions }))
	mux.HandleFunc("GET /v1/invoices/{id}", serveFrom(func() map[string]map[string]any { return fake.invoices }))
	testServer := httptest.NewServer(mux)

	previousBaseUrl := stripeApiBaseUrl
	previousTokenFunc := stripeApiTokenFunc
	previousToken := stripeApiToken
	previousBackend := stripe.GetBackend(stripe.APIBackend)
	stripeApiBaseUrl = testServer.URL
	stripeApiTokenFunc = func() string { return "sk_test_verify_db" }
	stripeApiToken = func() string { return "sk_test_verify_db" }
	stripe.SetBackend(stripe.APIBackend, stripe.GetBackendWithConfig(stripe.APIBackend, &stripe.BackendConfig{
		URL:               stripe.String(testServer.URL),
		MaxNetworkRetries: stripe.Int64(0),
	}))
	t.Cleanup(func() {
		stripeApiBaseUrl = previousBaseUrl
		stripeApiTokenFunc = previousTokenFunc
		stripeApiToken = previousToken
		stripe.SetBackend(stripe.APIBackend, previousBackend)
		testServer.Close()
	})
	return fake
}

// A subscription stamped with the network, and its paid invoice, served
// expanded (customer and subscription as objects), which every reader of an
// invoice accepts.
func (self *stripeVerifyDbFake) setPaidSubscription(subscriptionId string, invoiceId string, networkId server.Id, status string, total int, metadata map[string]any, periodStart time.Time) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if metadata == nil {
		metadata = map[string]any{}
	}
	metadata["network_id"] = networkId.String()
	self.subscriptions[subscriptionId] = map[string]any{
		"id":             subscriptionId,
		"object":         "subscription",
		"status":         status,
		"customer":       "cus_verify_db",
		"metadata":       metadata,
		"latest_invoice": invoiceId,
		"items":          map[string]any{"object": "list", "data": []any{}},
	}
	self.invoices[invoiceId] = map[string]any{
		"id":             invoiceId,
		"object":         "invoice",
		"status":         "paid",
		"total":          total,
		"amount_paid":    total,
		"currency":       "usd",
		"billing_reason": "subscription_create",
		"customer":       map[string]any{"id": "cus_verify_db", "email": "verify-db@example.com"},
		"subscription":   map[string]any{"id": subscriptionId, "metadata": metadata},
		"lines": map[string]any{"data": []map[string]any{{
			"type":         "subscription",
			"subscription": subscriptionId,
			"period": map[string]any{
				"start": periodStart.Unix(),
				"end":   periodStart.Add(30 * 24 * time.Hour).Unix(),
			},
		}}},
	}
}

// A completed, paid Pro checkout of the network.
func (self *stripeVerifyDbFake) setCompletedCheckout(sessionId string, networkId server.Id, subscriptionId string, invoiceId string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.sessions[sessionId] = map[string]any{
		"id":                  sessionId,
		"object":              "checkout.session",
		"mode":                "subscription",
		"status":              "complete",
		"payment_status":      "paid",
		"client_reference_id": networkId.String(),
		"customer":            "cus_verify_db",
		"subscription":        subscriptionId,
		"invoice":             invoiceId,
		"created":             server.NowUtc().Add(-time.Minute).Unix(),
	}
}

// The invoice's rows in the stripe_invoice ledger and subscription_renewal.
func stripeVerifyDbCounts(t testing.TB, ctx context.Context, networkId server.Id, invoiceId string) (ledgerRows int, renewalRows int) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				(SELECT COUNT(*) FROM stripe_invoice WHERE invoice_id = $2 AND network_id = $1),
				(SELECT COUNT(*) FROM subscription_renewal WHERE network_id = $1 AND transaction_id = $2)
			`,
			networkId,
			invoiceId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&ledgerRows, &renewalRows))
			}
		})
	})
	return
}

// A paid Pro checkout whose invoice.paid has not arrived: the network stays
// Free until the app reports the session, the report credits it at once, and
// the webhook's late delivery credits nothing more.
func TestStripeCheckoutReportCreditsProBeforeTheWebhook(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		setProCycleTestData(t, 10*model.Tib)
		fake := newStripeVerifyDbFake(t)

		networkId := server.NewId()
		userId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "stripeverifydb", userId)
		clientSession := session.NewLocalClientSession(ctx, "127.0.0.1:1", &session.ByJwt{
			NetworkId:   networkId,
			UserId:      userId,
			NetworkName: "stripeverifydb",
		})

		periodStart := server.NowUtc().Add(-time.Minute)
		fake.setPaidSubscription("sub_verify_db", "in_verify_db", networkId, "active", 500, nil, periodStart)
		fake.setCompletedCheckout("cs_test_verify_db", networkId, "sub_verify_db", "in_verify_db")

		// paid at Stripe, no webhook yet: Free
		connect.AssertEqual(t, model.IsProNetworkFresh(ctx, networkId), false)

		result, err := VerifyStripeCheckout(&VerifyStripeCheckoutArgs{SessionId: "cs_test_verify_db"}, clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Status, VerifyPurchaseStatusCredited)
		connect.AssertEqual(t, model.IsProNetworkFresh(ctx, networkId), true)
		ledgerRows, renewalRows := stripeVerifyDbCounts(t, ctx, networkId, "in_verify_db")
		connect.AssertEqual(t, ledgerRows, 1)
		connect.AssertEqual(t, renewalRows, 1)

		// invoice.paid arrives late: absorbed by the ledger
		invoiceObject, err := json.Marshal(map[string]any{
			"id":       "in_verify_db",
			"total":    500,
			"customer": "cus_verify_db",
			"currency": "usd",
		})
		connect.AssertEqual(t, err, nil)
		_, err = StripeWebhook(&StripeWebhookArgs{
			Id:   "evt_verify_db",
			Type: "invoice.paid",
			Data: &StripeEventData{Object: invoiceObject},
		}, clientSession)
		connect.AssertEqual(t, err, nil)
		ledgerRows, renewalRows = stripeVerifyDbCounts(t, ctx, networkId, "in_verify_db")
		connect.AssertEqual(t, ledgerRows, 1)
		connect.AssertEqual(t, renewalRows, 1)
		connect.AssertEqual(t, len(model.GetActiveTransferBalances(ctx, networkId)), 1)

		// the app's retried report: already credited, nothing more
		result, err = VerifyStripeCheckout(&VerifyStripeCheckoutArgs{SessionId: "cs_test_verify_db"}, clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Status, VerifyPurchaseStatusAlreadyCredited)
		ledgerRows, renewalRows = stripeVerifyDbCounts(t, ctx, networkId, "in_verify_db")
		connect.AssertEqual(t, ledgerRows, 1)
		connect.AssertEqual(t, renewalRows, 1)
	})
}

// The payment sheet (Android without Play, the macOS direct build, the
// desktop apps' pay page) reports its subscription: its paid first invoice
// credits Pro at once, and a report from another network credits nothing.
func TestStripePaymentSheetReportCreditsProBeforeTheWebhook(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		setProCycleTestData(t, 10*model.Tib)
		fake := newStripeVerifyDbFake(t)

		networkId := server.NewId()
		userId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "stripesheetdb", userId)
		otherNetworkId := server.NewId()
		otherUserId := server.NewId()
		model.Testing_CreateNetwork(ctx, otherNetworkId, "stripesheetother", otherUserId)

		fake.setPaidSubscription(
			"sub_sheet_db",
			"in_sheet_db",
			networkId,
			"active",
			500,
			map[string]any{"plan": "monthly", "payment_sheet": "pending"},
			server.NowUtc().Add(-time.Minute),
		)

		otherSession := session.NewLocalClientSession(ctx, "127.0.0.1:1", &session.ByJwt{NetworkId: otherNetworkId, UserId: otherUserId})
		result, err := VerifyStripeCheckout(&VerifyStripeCheckoutArgs{SubscriptionId: "sub_sheet_db"}, otherSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Status, VerifyPurchaseStatusWrongNetwork)
		connect.AssertEqual(t, model.IsProNetworkFresh(ctx, otherNetworkId), false)
		connect.AssertEqual(t, model.IsProNetworkFresh(ctx, networkId), false)

		clientSession := session.NewLocalClientSession(ctx, "127.0.0.1:1", &session.ByJwt{NetworkId: networkId, UserId: userId})
		result, err = VerifyStripeCheckout(&VerifyStripeCheckoutArgs{SubscriptionId: "sub_sheet_db"}, clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Status, VerifyPurchaseStatusCredited)
		connect.AssertEqual(t, model.IsProNetworkFresh(ctx, networkId), true)
		ledgerRows, renewalRows := stripeVerifyDbCounts(t, ctx, networkId, "in_sheet_db")
		connect.AssertEqual(t, ledgerRows, 1)
		connect.AssertEqual(t, renewalRows, 1)
	})
}
