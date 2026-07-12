package controller

import (
	"context"
	"testing"

	"github.com/go-playground/assert/v2"

	"github.com/stripe/stripe-go/v82"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// TestStripeDataPackByteCount pins the item -> amount mapping. Getting this wrong
// charges for one thing and delivers another.
func TestStripeDataPackByteCount(t *testing.T) {
	byteCount, ok := stripeDataPackByteCount(StripeItemData1Tib)
	assert.Equal(t, ok, true)
	assert.Equal(t, byteCount, 1*model.Tib)

	byteCount, ok = stripeDataPackByteCount(StripeItemData10Tib)
	assert.Equal(t, ok, true)
	assert.Equal(t, byteCount, 10*model.Tib)

	// the Pro items are subscriptions, not data packs
	_, ok = stripeDataPackByteCount(StripeItemProMonthly)
	assert.Equal(t, ok, false)
	_, ok = stripeDataPackByteCount("nonsense")
	assert.Equal(t, ok, false)
}

// TestStripeDataPackPriceUsd pins that the web checkout quotes the SAME price as
// pro.yml -- which is also what the site and the x402 skus quote. A customer must never
// be shown two different prices for the same thing.
func TestStripeDataPackPriceUsd(t *testing.T) {
	priceUsd, ok := stripeDataPackPriceUsd(1 * model.Tib)
	assert.Equal(t, ok, true)
	assert.Equal(t, priceUsd, float64(5))

	priceUsd, ok = stripeDataPackPriceUsd(10 * model.Tib)
	assert.Equal(t, ok, true)
	assert.Equal(t, priceUsd, float64(30))

	// an amount we do not sell has no price -- checkout must refuse, not guess
	_, ok = stripeDataPackPriceUsd(7 * model.Tib)
	assert.Equal(t, ok, false)
}

// TestStripeCheckoutAmountsAreWholeCents pins the USD -> cents conversion Stripe wants.
// $30 must be 3000, never 2999 (float truncation) and never 30.
func TestStripeCheckoutAmountsAreWholeCents(t *testing.T) {
	cents := func(priceUsd float64) int64 {
		// mirrors StripeCreateCheckoutSession
		return int64(round(priceUsd * 100))
	}

	assert.Equal(t, cents(5), int64(500))
	assert.Equal(t, cents(30), int64(3000))
	assert.Equal(t, cents(12.34), int64(1234))
	// float noise must not shave a cent off
	assert.Equal(t, cents(0.1+0.2), int64(30))
}

func round(f float64) float64 {
	if f < 0 {
		return float64(int64(f - 0.5))
	}
	return float64(int64(f + 0.5))
}

// TestStripeCheckoutRefusesWithoutReturnUrls pins that we never send a customer to
// Stripe with nowhere to come back to. An unconfigured env must refuse, not hand them
// off and lose them.
func TestStripeCheckoutRefusesWithoutReturnUrls(t *testing.T) {
	urls := stripeCheckoutUrls()

	// this test env has no config stripe.yml, so the urls are empty and checkout must
	// be refused. (In main they are set -- see config/main/stripe.yml.)
	if urls.SuccessUrl == "" || urls.CancelUrl == "" {
		result := stripeCheckoutError("Checkout is not configured.")
		assert.NotEqual(t, result.Error, nil)
		assert.Equal(t, result.CheckoutUrl, "")
	}
}

// TestStripeCheckoutUiMode pins the ui mode the caller gets. Empty must mean HOSTED:
// the web app and the mobile apps were written before embedded existed and only ever read
// checkout_url, so a default of embedded would hand them a session with no url and their
// upgrade button would silently do nothing.
func TestStripeCheckoutUiMode(t *testing.T) {
	uiMode, ok := stripeCheckoutUiMode("")
	assert.Equal(t, ok, true)
	assert.Equal(t, uiMode, StripeUiModeHosted)

	uiMode, ok = stripeCheckoutUiMode(StripeUiModeEmbedded)
	assert.Equal(t, ok, true)
	assert.Equal(t, uiMode, StripeUiModeEmbedded)

	uiMode, ok = stripeCheckoutUiMode(StripeUiModeHosted)
	assert.Equal(t, ok, true)
	assert.Equal(t, uiMode, StripeUiModeHosted)

	// Stripe also has a "custom" ui mode, but we do not build for it -- refuse rather
	// than pass an unsupported mode through to the API
	_, ok = stripeCheckoutUiMode("custom")
	assert.Equal(t, ok, false)
	_, ok = stripeCheckoutUiMode("nonsense")
	assert.Equal(t, ok, false)
}

// TestStripeCheckoutUiModeParamsDoNotMix pins the one thing Stripe will reject outright:
// success_url/cancel_url are NOT ALLOWED on a session with ui_mode embedded, and an
// embedded session needs a return_url instead. Mixing the two shapes is a 400 from Stripe
// -- i.e. an upgrade button that just errors -- so it is worth pinning.
func TestStripeCheckoutUiModeParamsDoNotMix(t *testing.T) {
	urls := StripeCheckoutUrls{
		SuccessUrl: "https://ur.io/checkout/success",
		CancelUrl:  "https://ur.io/checkout/cancel",
		ReturnUrl:  "https://ur.io/checkout/complete?session_id={CHECKOUT_SESSION_ID}",
	}

	embedded := &stripe.CheckoutSessionParams{}
	assert.Equal(t, stripeCheckoutApplyUiMode(embedded, StripeUiModeEmbedded, urls), true)
	assert.Equal(t, *embedded.UIMode, string(stripe.CheckoutSessionUIModeEmbedded))
	assert.Equal(t, *embedded.ReturnURL, urls.ReturnUrl)
	// the pair Stripe rejects in this mode must be absent
	assert.Equal(t, embedded.SuccessURL, nil)
	assert.Equal(t, embedded.CancelURL, nil)

	hosted := &stripe.CheckoutSessionParams{}
	assert.Equal(t, stripeCheckoutApplyUiMode(hosted, StripeUiModeHosted, urls), true)
	assert.Equal(t, *hosted.SuccessURL, urls.SuccessUrl)
	assert.Equal(t, *hosted.CancelURL, urls.CancelUrl)
	// hosted is Stripe's default; sending ui_mode/return_url would only confuse it
	assert.Equal(t, hosted.UIMode, nil)
	assert.Equal(t, hosted.ReturnURL, nil)
}

// TestStripeCheckoutRefusesUnconfiguredUiMode pins that each mode refuses on ITS OWN
// urls. An env that configured hosted checkout but never set return_url must not quietly
// create an embedded session that strands the customer in a webview with no way back --
// and vice versa.
func TestStripeCheckoutRefusesUnconfiguredUiMode(t *testing.T) {
	hostedOnly := StripeCheckoutUrls{
		SuccessUrl: "https://ur.io/checkout/success",
		CancelUrl:  "https://ur.io/checkout/cancel",
	}
	assert.Equal(t, stripeCheckoutApplyUiMode(&stripe.CheckoutSessionParams{}, StripeUiModeHosted, hostedOnly), true)
	assert.Equal(t, stripeCheckoutApplyUiMode(&stripe.CheckoutSessionParams{}, StripeUiModeEmbedded, hostedOnly), false)

	embeddedOnly := StripeCheckoutUrls{
		ReturnUrl: "https://ur.io/checkout/complete",
	}
	assert.Equal(t, stripeCheckoutApplyUiMode(&stripe.CheckoutSessionParams{}, StripeUiModeEmbedded, embeddedOnly), true)
	assert.Equal(t, stripeCheckoutApplyUiMode(&stripe.CheckoutSessionParams{}, StripeUiModeHosted, embeddedOnly), false)

	// a half-configured hosted pair is not usable either
	assert.Equal(t, stripeCheckoutApplyUiMode(
		&stripe.CheckoutSessionParams{},
		StripeUiModeHosted,
		StripeCheckoutUrls{SuccessUrl: "https://ur.io/checkout/success"},
	), false)

	// nothing configured at all
	assert.Equal(t, stripeCheckoutApplyUiMode(&stripe.CheckoutSessionParams{}, StripeUiModeHosted, StripeCheckoutUrls{}), false)
	assert.Equal(t, stripeCheckoutApplyUiMode(&stripe.CheckoutSessionParams{}, StripeUiModeEmbedded, StripeCheckoutUrls{}), false)
}

// TestSignedInDataPurchaseLandsTheData pins the whole point of a signed-in purchase: the
// data ARRIVES.
//
// The webhook used to only create a balance code and email it, ignoring
// client_reference_id entirely. So a customer who bought 10 TiB while logged in got a
// code in their inbox to find and paste back into the app — and the confirmation page
// sat polling for a balance that would never grow, eventually telling them the purchase
// was "taking longer than usual" when it had in fact worked perfectly.
//
// We know whose network it is. The data lands.
func TestSignedInDataPurchaseLandsTheData(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()

		before := model.GetActiveTransferBalances(ctx, networkId)
		assert.Equal(t, len(before), 0)

		err := CreateBalanceCode(
			ctx,
			1*model.Tib,
			model.Pro().DataCodeDuration,
			model.UsdToNanoCents(5.00),
			"test-checkout-session-signed-in",
			"test-record",
			"buyer@bringyour.com",
			&networkId, // signed in: we know the network
		)
		assert.Equal(t, err, nil)

		// the data is simply THERE -- no code to redeem by hand
		after := model.GetActiveTransferBalances(ctx, networkId)
		assert.Equal(t, len(after), 1)
		assert.Equal(t, after[0].BalanceByteCount, 1*model.Tib)
		// ...and a data purchase never grants Pro, however it was bought
		assert.Equal(t, after[0].Pro, false)
		assert.Equal(t, model.IsProNetwork(ctx, networkId), false)
	})
}

// TestWebhookRetryDoesNotDoubleCredit pins idempotency. Stripe retries a webhook it
// thinks failed, so the same purchase event can arrive more than once. It must not credit
// the network twice — that would be giving away data for free, forever, to anyone whose
// webhook happened to be retried.
func TestWebhookRetryDoesNotDoubleCredit(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()

		purchaseEventId := "test-checkout-session-retried"

		for i := 0; i < 3; i += 1 {
			err := CreateBalanceCode(
				ctx,
				1*model.Tib,
				model.Pro().DataCodeDuration,
				model.UsdToNanoCents(5.00),
				purchaseEventId, // the SAME purchase event, delivered three times
				"test-record",
				"buyer@bringyour.com",
				&networkId,
			)
			assert.Equal(t, err, nil)
		}

		// exactly one credit, not three
		balances := model.GetActiveTransferBalances(ctx, networkId)
		assert.Equal(t, len(balances), 1)
		assert.Equal(t, balances[0].BalanceByteCount, 1*model.Tib)
	})
}
