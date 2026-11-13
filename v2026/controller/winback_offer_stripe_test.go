package controller

// The win-back offer at Stripe's two purchase paths: the inline payment sheet
// every Android Stripe flavor (github, ungoogle, solana_dapp, ethos_dapp),
// Windows and Linux buy through, and the web's Checkout session. Each applies
// the network's promotion code to the monthly plan only, replaces a code a
// sheet used up without paying, and applies nothing where Stripe would not
// charge the half month (the regional tier). Against the test database and a
// fake Stripe API.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/stripe/stripe-go/v82"
	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/reengagement"
	"github.com/urnetwork/server/v2026/session"
)

const winbackStripeTestCustomerId = "cus_winback_test"

type winbackStripeTestEnv struct {
	stateLock sync.Mutex
	// the promotion codes Stripe knows, by id
	promotionCodes map[string]map[string]any
	// the forms of POST /v1/subscriptions, /v1/checkout/sessions and
	// /v1/promotion_codes, in order
	subscriptionForms  []url.Values
	checkoutForms      []url.Values
	promotionCodeForms []url.Values
	unexpected         []string
}

func (self *winbackStripeTestEnv) writeJson(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}

func (self *winbackStripeTestEnv) form(t testing.TB, r *http.Request) url.Values {
	if err := r.ParseForm(); err != nil {
		t.Errorf("fake stripe: %s", err)
	}
	return r.PostForm
}

// installWinbackStripeTestEnv stands up the fake Stripe API every call of the
// two purchase paths reaches, and the configured price ids of both tiers.
func installWinbackStripeTestEnv(t testing.TB) *winbackStripeTestEnv {
	env := &winbackStripeTestEnv{promotionCodes: map[string]map[string]any{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/subscriptions", func(w http.ResponseWriter, r *http.Request) {
		env.writeJson(w, http.StatusOK, map[string]any{"object": "list", "url": "/v1/subscriptions", "data": []any{}, "has_more": false})
	})
	mux.HandleFunc("POST /v1/subscriptions", func(w http.ResponseWriter, r *http.Request) {
		form := env.form(t, r)
		env.stateLock.Lock()
		env.subscriptionForms = append(env.subscriptionForms, form)
		n := len(env.subscriptionForms)
		env.stateLock.Unlock()
		body := map[string]any{
			"id":       fmt.Sprintf("sub_winback_%d", n),
			"object":   "subscription",
			"customer": form.Get("customer"),
			"status":   "incomplete",
			"latest_invoice": map[string]any{
				"id":                  fmt.Sprintf("in_winback_%d", n),
				"object":              "invoice",
				"confirmation_secret": map[string]any{"client_secret": fmt.Sprintf("pi_winback_%d_secret", n), "type": "payment_intent"},
			},
		}
		if form.Get("trial_period_days") != "" {
			body["status"] = "trialing"
			body["trial_end"] = time.Now().Add(14 * 24 * time.Hour).Unix()
			body["pending_setup_intent"] = map[string]any{"id": fmt.Sprintf("seti_winback_%d", n), "object": "setup_intent", "client_secret": fmt.Sprintf("seti_winback_%d_secret", n)}
		}
		env.writeJson(w, http.StatusOK, body)
	})
	mux.HandleFunc("POST /v1/ephemeral_keys", func(w http.ResponseWriter, r *http.Request) {
		env.writeJson(w, http.StatusOK, map[string]any{"id": "ephkey_winback", "object": "ephemeral_key", "secret": "ek_winback_secret"})
	})
	mux.HandleFunc("POST /v1/checkout/sessions", func(w http.ResponseWriter, r *http.Request) {
		form := env.form(t, r)
		env.stateLock.Lock()
		env.checkoutForms = append(env.checkoutForms, form)
		n := len(env.checkoutForms)
		env.stateLock.Unlock()
		env.writeJson(w, http.StatusOK, map[string]any{"id": fmt.Sprintf("cs_winback_%d", n), "object": "checkout.session", "client_secret": fmt.Sprintf("cs_winback_%d_secret", n)})
	})
	mux.HandleFunc("GET /v1/promotion_codes/{id}", func(w http.ResponseWriter, r *http.Request) {
		env.stateLock.Lock()
		promo, ok := env.promotionCodes[r.PathValue("id")]
		env.stateLock.Unlock()
		if !ok {
			env.writeJson(w, http.StatusNotFound, map[string]any{"error": map[string]any{"type": "invalid_request_error", "code": "resource_missing", "message": "No such promotion code"}})
			return
		}
		env.writeJson(w, http.StatusOK, promo)
	})
	mux.HandleFunc("POST /v1/promotion_codes", func(w http.ResponseWriter, r *http.Request) {
		form := env.form(t, r)
		env.stateLock.Lock()
		env.promotionCodeForms = append(env.promotionCodeForms, form)
		id := fmt.Sprintf("promo_replacement_%d", len(env.promotionCodeForms))
		env.stateLock.Unlock()
		promo := env.addPromotionCode(id, form.Get("customer"), 0)
		env.writeJson(w, http.StatusOK, promo)
	})
	mux.HandleFunc("GET /v1/coupons/{id}", func(w http.ResponseWriter, r *http.Request) {
		env.writeJson(w, http.StatusOK, map[string]any{"id": r.PathValue("id"), "object": "coupon", "percent_off": 50, "duration": "once", "valid": true})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		env.stateLock.Lock()
		env.unexpected = append(env.unexpected, r.Method+" "+r.URL.Path)
		env.stateLock.Unlock()
		env.writeJson(w, http.StatusNotFound, map[string]any{"error": map[string]any{"type": "invalid_request_error", "message": "unexpected " + r.URL.Path}})
	})
	testServer := httptest.NewServer(mux)

	previousToken := stripeApiToken
	previousPublishableKey := stripePublishableKey
	previousBackend := stripe.GetBackend(stripe.APIBackend)
	stripeApiToken = func() string { return "sk_test_winback" }
	stripePublishableKey = func() string { return "pk_test_winback" }
	stripe.SetBackend(stripe.APIBackend, stripe.GetBackendWithConfig(stripe.APIBackend, &stripe.BackendConfig{
		URL:               stripe.String(testServer.URL),
		MaxNetworkRetries: stripe.Int64(0),
	}))
	// the tiers' price ids, as configured ids or the created ones are cached
	priceIds := map[string]string{
		model.PriceTierStandard + "/" + model.PlanMonthly: "price_standard_monthly",
		model.PriceTierStandard + "/" + model.PlanYearly:  "price_standard_yearly",
		"regional/" + model.PlanMonthly:                   "price_regional_monthly",
		"regional/" + model.PlanYearly:                    "price_regional_yearly",
	}
	for key, id := range priceIds {
		stripeTierPriceCache.Store(key, id)
	}
	t.Cleanup(func() {
		stripeApiToken = previousToken
		stripePublishableKey = previousPublishableKey
		stripe.SetBackend(stripe.APIBackend, previousBackend)
		for key := range priceIds {
			stripeTierPriceCache.Delete(key)
		}
		testServer.Close()
	})
	return env
}

// addPromotionCode makes a promotion code Stripe knows: the comeback coupon,
// for the customer, redeemed timesRedeemed of its one time.
func (self *winbackStripeTestEnv) addPromotionCode(id string, customerId string, timesRedeemed int) map[string]any {
	promo := map[string]any{
		"id":              id,
		"object":          "promotion_code",
		"active":          timesRedeemed < 1,
		"max_redemptions": 1,
		"times_redeemed":  timesRedeemed,
		"expires_at":      time.Now().Add(7 * 24 * time.Hour).Unix(),
		"customer":        customerId,
		"coupon":          map[string]any{"id": "winback_monthly_50", "object": "coupon", "percent_off": 50, "duration": "once", "valid": true},
	}
	self.stateLock.Lock()
	self.promotionCodes[id] = promo
	self.stateLock.Unlock()
	return promo
}

func (self *winbackStripeTestEnv) lastSubscriptionForm() url.Values {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.subscriptionForms[len(self.subscriptionForms)-1]
}

func (self *winbackStripeTestEnv) lastCheckoutForm() url.Values {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.checkoutForms[len(self.checkoutForms)-1]
}

// A network with an active Stripe win-back offer buys the monthly plan
// through the payment sheet: the subscription carries its promotion code and
// names the offer, and the sheet quotes the half month; the yearly plan
// carries no code. A code the customer used up without paying (an abandoned
// sheet's subscription) is replaced with one of the offer's own expiry. At
// the regional tier the half month (0.25 USD) is below Stripe's minimum
// charge, so the purchase goes at the regular price. Web Checkout applies the
// same code, for the customer it was made for.
func TestStripePurchasePathsApplyWinbackOffer(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		installWinbackTestEnv(t)
		stripeEnv := installWinbackStripeTestEnv(t)

		networkId := server.NewId()
		userId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "winback-stripe-"+networkId.String(), userId)
		connect.AssertEqual(t, model.CreateStripeCustomerForNetwork(ctx, networkId, winbackStripeTestCustomerId), nil)
		clientSession := session.Testing_CreateClientSession(ctx, &session.ByJwt{NetworkId: networkId, UserId: userId})

		issuedAt := server.NowUtc().Truncate(time.Second)
		offer, created, err := model.IssueWinbackOffer(ctx, &model.IssueWinbackOfferArgs{
			NetworkId:             networkId,
			Store:                 reengagement.WinbackStoreStripe,
			Plan:                  model.PlanMonthly,
			PercentOff:            50,
			IssuedAt:              issuedAt,
			ExpiresAt:             issuedAt.Add(14 * 24 * time.Hour),
			StripePromotionCodeId: "promo_winback_first",
		})
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, created, true)
		stripeEnv.addPromotionCode("promo_winback_first", winbackStripeTestCustomerId, 0)

		sheet := func(plan string, storefrontCountry string) *StripePaymentSheetResult {
			result, err := StripePaymentSheet(&StripePaymentSheetArgs{Plan: plan, StorefrontCountry: storefrontCountry}, clientSession)
			connect.AssertEqual(t, err, nil)
			if result.Error != nil {
				t.Fatalf("payment sheet %s %s refused: %+v (unexpected stripe calls %v)", plan, storefrontCountry, result.Error, stripeEnv.unexpected)
			}
			return result
		}

		// the monthly plan, standard tier: the offer's code, half of 5 USD
		monthly := sheet(model.PlanMonthly, "US")
		connect.AssertEqual(t, monthly.WinbackOfferApplied, true)
		connect.AssertEqual(t, monthly.OfferApplied, false)
		connect.AssertEqual(t, monthly.AmountFirstPeriodUsd, 2.5)
		connect.AssertEqual(t, monthly.RegularPeriodUsd, 5.0)
		connect.AssertEqual(t, monthly.IntentType, "payment")
		form := stripeEnv.lastSubscriptionForm()
		connect.AssertEqual(t, form.Get("discounts[0][promotion_code]"), "promo_winback_first")
		connect.AssertEqual(t, form.Get("discounts[0][coupon]"), "")
		connect.AssertEqual(t, form.Get("metadata[winback_offer]"), offer.OfferId.String())
		connect.AssertEqual(t, form.Get("customer"), winbackStripeTestCustomerId)
		connect.AssertEqual(t, form.Get("items[0][price]"), "price_standard_monthly")

		// the yearly plan: no win-back code (the welcome offer is not issued)
		yearly := sheet(model.PlanYearly, "US")
		connect.AssertEqual(t, yearly.WinbackOfferApplied, false)
		form = stripeEnv.lastSubscriptionForm()
		connect.AssertEqual(t, form.Get("discounts[0][promotion_code]"), "")
		connect.AssertEqual(t, form.Get("metadata[winback_offer]"), "")

		// the code was used up by a sheet that never paid: a fresh one, for
		// the same customer and the offer's own expiry, and the record names it
		stripeEnv.addPromotionCode("promo_winback_first", winbackStripeTestCustomerId, 1)
		replaced := sheet(model.PlanMonthly, "US")
		connect.AssertEqual(t, replaced.WinbackOfferApplied, true)
		connect.AssertEqual(t, len(stripeEnv.promotionCodeForms), 1)
		promoForm := stripeEnv.promotionCodeForms[0]
		connect.AssertEqual(t, promoForm.Get("coupon"), "winback_monthly_50")
		connect.AssertEqual(t, promoForm.Get("customer"), winbackStripeTestCustomerId)
		connect.AssertEqual(t, promoForm.Get("max_redemptions"), "1")
		connect.AssertEqual(t, promoForm.Get("expires_at"), fmt.Sprintf("%d", offer.ExpiresAt.Unix()))
		connect.AssertEqual(t, stripeEnv.lastSubscriptionForm().Get("discounts[0][promotion_code]"), "promo_replacement_1")
		connect.AssertEqual(t, *model.GetWinbackOffer(ctx, offer.OfferId).StripePromotionCodeId, "promo_replacement_1")

		// the regional tier: half of 0.50 USD is below Stripe's minimum charge,
		// so the regular price, and the plan response does not show the offer
		regionalSheet := sheet(model.PlanMonthly, "BR")
		connect.AssertEqual(t, regionalSheet.WinbackOfferApplied, false)
		connect.AssertEqual(t, regionalSheet.AmountFirstPeriodUsd, 0.5)
		connect.AssertEqual(t, stripeEnv.lastSubscriptionForm().Get("discounts[0][promotion_code]"), "")
		connect.AssertEqual(t, stripeEnv.lastSubscriptionForm().Get("items[0][price]"), "price_regional_monthly")
		regional := model.Pro().PriceTierForCountry("BR")
		connect.AssertEqual(t, winbackPlanOfferResult(model.GetActiveWinbackOffer(ctx, networkId, server.NowUtc()), regional, server.NowUtc()) == nil, true)

		// web Checkout (embedded, inline): the same code, and the session is
		// made for the customer the code is restricted to
		checkout := func(itemId string, storefrontCountry string) url.Values {
			result, err := StripeCreateCheckoutSession(&StripeCreateCheckoutSessionArgs{
				ItemId:               itemId,
				StorefrontCountry:    storefrontCountry,
				UiMode:               StripeUiModeEmbedded,
				RedirectOnCompletion: "never",
			}, clientSession)
			connect.AssertEqual(t, err, nil)
			if result.Error != nil {
				t.Fatalf("checkout %s %s refused: %+v (unexpected stripe calls %v)", itemId, storefrontCountry, result.Error, stripeEnv.unexpected)
			}
			return stripeEnv.lastCheckoutForm()
		}
		checkoutForm := checkout(StripeItemProMonthly, "US")
		connect.AssertEqual(t, checkoutForm.Get("discounts[0][promotion_code]"), "promo_replacement_1")
		connect.AssertEqual(t, checkoutForm.Get("customer"), winbackStripeTestCustomerId)
		connect.AssertEqual(t, checkoutForm.Get("subscription_data[metadata][winback_offer]"), offer.OfferId.String())
		connect.AssertEqual(t, checkoutForm.Get("line_items[0][price]"), "price_standard_monthly")
		checkoutForm = checkout(StripeItemProYearly, "US")
		connect.AssertEqual(t, checkoutForm.Get("discounts[0][promotion_code]"), "")
		connect.AssertEqual(t, checkoutForm.Get("subscription_data[metadata][winback_offer]"), "")
		checkoutForm = checkout(StripeItemProMonthly, "BR")
		connect.AssertEqual(t, checkoutForm.Get("discounts[0][promotion_code]"), "")
		connect.AssertEqual(t, checkoutForm.Get("customer"), "")

		// once redeemed, nothing is applied
		server.Tx(ctx, func(tx server.PgTx) {
			model.RedeemWinbackOfferInTx(tx, ctx, networkId, reengagement.WinbackStoreStripe, &offer.OfferId, server.NowUtc())
		})
		after := sheet(model.PlanMonthly, "US")
		connect.AssertEqual(t, after.WinbackOfferApplied, false)
		connect.AssertEqual(t, stripeEnv.lastSubscriptionForm().Get("discounts[0][promotion_code]"), "")
		connect.AssertEqual(t, len(stripeEnv.unexpected), 0)
	})
}
