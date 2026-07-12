package handlers

import (
	"net/http"

	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
)

func SubscriptionBalance(w http.ResponseWriter, r *http.Request) {
	router.WrapRequireAuth(controller.SubscriptionBalance, w, r)
}

func StripeWebhook(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputBodyFormatterNoAuth(
		controller.VerifyStripeBody,
		controller.StripeWebhook,
		w,
		r,
	)
}

func CoinbaseWebhook(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputBodyFormatterNoAuth(
		controller.VerifyCoinbaseBody,
		controller.CoinbaseWebhook,
		w,
		r,
	)
}

func PlayWebhook(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputBodyFormatterNoAuth(
		controller.VerifyPlayBody,
		controller.PlayWebhook,
		w,
		r,
	)
}

func HeliusWebhook(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputBodyFormatterNoAuth(
		controller.VerifyHeliusBody,
		controller.HeliusWebhook,
		w,
		r,
	)
}

func SubscriptionCheckBalanceCode(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(model.CheckBalanceCode, w, r)
}

func SubscriptionRedeemBalanceCode(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(controller.RedeemBalanceCode, w, r)
}

func SubscriptionCreatePaymentId(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(model.SubscriptionCreatePaymentId, w, r)
}

func CreateSolanaPaymentIntent(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(controller.CreateSolanaPaymentIntent, w, r)
}

func CreateStripePaymentIntent(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(controller.StripeCreatePaymentIntent, w, r)
}

// StripeCreateCheckoutSession starts a Stripe Checkout Session for the caller's network.
//
// ui_mode "hosted" (the default) returns Stripe's hosted checkout_url -- the client just
// navigates there. ui_mode "embedded" instead returns a client_secret + publishable_key,
// which Stripe.js mounts as Embedded Checkout on our own /checkout page (that is what the
// desktop apps render in a webview). A session is one or the other, never both.
func StripeCreateCheckoutSession(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(controller.StripeCreateCheckoutSession, w, r)
}

func StripeCreateCustomerPortal(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(controller.StripeCreateCustomerPortal, w, r)
}
