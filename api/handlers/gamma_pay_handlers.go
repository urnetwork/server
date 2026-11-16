package handlers

import (
	"net/http"

	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/router"
)

// Pay with Gamma (controller/gamma_controller.go, mmm/BTCPAY.md §18): the
// services document set_service publishes, the terms, an intent for the
// signed-in caller's network, an intent's status, and the account's
// automatic renewal and top-up switches. The status call carries the
// intent's status token in the body; it never goes in a url.

// GammaServices is the services document, readable by any web app.
func GammaServices(w http.ResponseWriter, r *http.Request) {
	controller.GammaServicesCors(w)
	router.WrapNoAuth(controller.GammaServices, w, r)
}

// GammaTerms says what gamma buys now and what minting needs.
func GammaTerms(w http.ResponseWriter, r *http.Request) {
	router.WrapNoAuth(controller.GammaTerms, w, r)
}

// GammaPaymentIntent starts an intent for the caller's network.
func GammaPaymentIntent(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(controller.CreateGammaPaymentIntent, w, r)
}

// GammaPaymentStatus is what a script polls while it waits.
func GammaPaymentStatus(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputNoAuth(controller.GammaPaymentStatus, w, r)
}

// GammaSettings reads the account's automatic switches.
func GammaSettings(w http.ResponseWriter, r *http.Request) {
	router.WrapRequireAuth(controller.GetGammaSettings, w, r)
}

// GammaSetSettings sets them.
func GammaSetSettings(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(controller.SetGammaSettings, w, r)
}
