package handlers

import (
	"net/http"

	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/router"
)

// The crypto payment rail (controller/crypto_pay_controller.go,
// docs/CRYPTO-PAY-API.md): the coins on offer, an invoice for the signed-in
// caller's network, a data pack without signing in, an invoice's status and a
// fresh quote. The status and requote calls carry the invoice's status token
// in the body; it never goes in a url.

// CryptoPaymentAssets lists the coins that are on and what each sells.
func CryptoPaymentAssets(w http.ResponseWriter, r *http.Request) {
	router.WrapNoAuth(controller.CryptoPaymentAssets, w, r)
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
