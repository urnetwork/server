package handlers

import (
	"net/http"

	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/router"
)

// The crypto payment rail (controller/crypto_pay_controller.go,
// docs/CRYPTO-PAY-API.md): the coins on offer, an invoice for the signed-in
// caller's network, a data pack without signing in, an invoice's status and a
// fresh quote. The status and requote calls carry the invoice's status token
// in the body; it never goes in a url.

// CryptoPaymentAssets lists the coins that can be paid with now and what each
// sells, and apart from them the coins that are on but temporarily unavailable.
func CryptoPaymentAssets(w http.ResponseWriter, r *http.Request) {
	router.WrapNoAuth(controller.CryptoPaymentAssets, w, r)
}

// CryptoPaymentQuote prices a plan or a data pack in a coin now, without an
// invoice. The bearer token is optional (the network's tier and offer).
func CryptoPaymentQuote(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputOptionalAuth(controller.CryptoPaymentQuote, w, r)
}

// AlchemyWebhook is the Alchemy webhook's delivery (ETH Address Activity;
// Bitcoin has none): verified by the webhook's own signing key, it only brings
// ETH's next chain read forward.
func AlchemyWebhook(w http.ResponseWriter, r *http.Request) {
	if !webhookVerifiable(w, "alchemy", controller.AlchemyWebhookVerifiable) {
		return
	}
	router.WrapWithInputBodyFormatterNoAuth(
		controller.VerifyAlchemyBody,
		controller.AlchemyWebhook,
		w,
		r,
	)
}

// CryptoPaymentIntent starts an invoice for the caller's network.
func CryptoPaymentIntent(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(controller.CreateCryptoPaymentIntent, w, r)
}

// PayDataCryptoIntent starts an invoice for a data pack without signing in:
// for a named network, or as a balance code.
func PayDataCryptoIntent(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputNoAuth(controller.CreateDataCryptoPaymentIntent, w, r)
}

// CryptoPaymentStatus is what a page or an app polls while it waits.
func CryptoPaymentStatus(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputNoAuth(controller.CryptoPaymentStatus, w, r)
}

// CryptoPaymentRequote gives an invoice nothing was paid to a fresh quote.
func CryptoPaymentRequote(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputNoAuth(controller.CryptoPaymentRequote, w, r)
}
