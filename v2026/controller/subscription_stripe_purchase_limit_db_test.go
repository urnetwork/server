package controller

// Root-cause test of the purchase start budget (owner, 2026-11-13).
// StripeCreateCheckoutSession had no limit per network, so one account could
// make Checkout sessions without end, and card testers ran stolen cards
// through them (20 cards in 92 seconds through two sessions made 2.5 minutes
// after their account). It drives the public endpoints against a fake Stripe
// API that makes every session, so it reads the same before and after the
// change. Needs the test database and redis (server.DefaultTestEnv).

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stripe/stripe-go/v82"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// A fake Stripe API that makes every Checkout session and counts them, and
// answers anything else not found. Safe for concurrent use.
type stripePurchaseLimitTestApi struct {
	stateLock       sync.Mutex
	sessionsCreated int
}

func newStripePurchaseLimitTestApi(t testing.TB) *stripePurchaseLimitTestApi {
	api := &stripePurchaseLimitTestApi{}
	writeJson := func(w http.ResponseWriter, status int, body string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		io.WriteString(w, body)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/checkout/sessions", func(w http.ResponseWriter, r *http.Request) {
		api.stateLock.Lock()
		api.sessionsCreated += 1
		n := api.sessionsCreated
		api.stateLock.Unlock()
		writeJson(w, http.StatusOK, fmt.Sprintf(
			`{"id": "cs_synthetic_limit_%d", "object": "checkout.session", "client_secret": "cs_synthetic_limit_%d_secret", "ui_mode": "embedded", "mode": "subscription", "status": "open"}`,
			n, n,
		))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeJson(w, http.StatusNotFound, `{"error": {"type": "invalid_request_error", "message": "not in the fake"}}`)
	})
	testServer := httptest.NewServer(mux)

	previousToken := stripeApiToken
	previousPublishableKey := stripePublishableKey
	previousBackend := stripe.GetBackend(stripe.APIBackend)
	stripeApiToken = func() string { return "sk_test_purchase_limit" }
	stripePublishableKey = func() string { return "pk_test_purchase_limit" }
	stripe.SetBackend(stripe.APIBackend, stripe.GetBackendWithConfig(stripe.APIBackend, &stripe.BackendConfig{
		URL:               stripe.String(testServer.URL),
		MaxNetworkRetries: stripe.Int64(0),
	}))
	t.Cleanup(func() {
		stripeApiToken = previousToken
		stripePublishableKey = previousPublishableKey
		stripe.SetBackend(stripe.APIBackend, previousBackend)
		testServer.Close()
	})
	return api
}

// How many Checkout sessions were made.
func (self *stripePurchaseLimitTestApi) created() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.sessionsCreated
}

// A network that can buy (it has a sign-in), with the yearly price of its
// tier known, so a checkout reaches Stripe without pro.yml.
func newStripePurchaseLimitTestNetwork(t testing.TB, ctx context.Context, name string) *session.ClientSession {
	_, clientSession := newStripePaymentFailedTestNetwork(t, ctx, name, name+"@synthetic.example")
	cacheKey := ResolvePriceTier(clientSession, "").Tier.Name + "/" + model.PlanYearly
	if _, ok := stripeTierPriceCache.Load(cacheKey); !ok {
		stripeTierPriceCache.Store(cacheKey, "price_synthetic_limit_yearly")
		t.Cleanup(func() {
			stripeTierPriceCache.Delete(cacheKey)
		})
	}
	return clientSession
}

// A network starts at most ten card purchases an hour: the eleventh Checkout
// session is refused as rate_limited before anything reaches Stripe, the
// payment sheet shares the budget, and another network is not held back.
// Before, every request made a session.
func TestStripeCheckoutRefusesANetworkPastItsPurchaseStarts(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		cryptoTestHasAuthMethod(t, true)
		api := newStripePurchaseLimitTestApi(t)
		clientSession := newStripePurchaseLimitTestNetwork(t, ctx, "purchaselimit")

		checkout := func(clientSession *session.ClientSession) *StripeCreateCheckoutSessionResult {
			result, err := StripeCreateCheckoutSession(&StripeCreateCheckoutSessionArgs{
				ItemId:               StripeItemProYearly,
				UiMode:               StripeUiModeEmbedded,
				RedirectOnCompletion: "never",
			}, clientSession)
			if err != nil {
				t.Fatalf("checkout: %v", err)
			}
			return result
		}
		for i := range 10 {
			if result := checkout(clientSession); result.Error != nil || result.ClientSecret == "" {
				t.Fatalf("purchase start %d was refused: %+v", i+1, result.Error)
			}
		}
		if created := api.created(); created != 10 {
			t.Fatalf("%d sessions made, want 10", created)
		}

		refused := checkout(clientSession)
		if refused.Error == nil || refused.Error.Code != PurchaseErrorCodeRateLimited {
			t.Fatalf("the eleventh purchase start within the hour must be refused, got %+v", refused)
		}
		if created := api.created(); created != 10 {
			t.Fatalf("a refused purchase start made a session: %d sessions", created)
		}

		sheet, err := StripePaymentSheet(&StripePaymentSheetArgs{Plan: model.PlanYearly}, clientSession)
		if err != nil {
			t.Fatal(err)
		}
		if sheet.Error == nil || sheet.Error.Code != PurchaseErrorCodeRateLimited {
			t.Fatalf("the payment sheet shares the network's purchase starts, got %+v", sheet)
		}

		otherSession := newStripePurchaseLimitTestNetwork(t, ctx, "purchaselimitother")
		if result := checkout(otherSession); result.Error != nil {
			t.Fatalf("another network's purchase was refused: %+v", result.Error)
		}
	})
}
