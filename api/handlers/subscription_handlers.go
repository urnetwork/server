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

func SubscriptionCheckBalanceCode(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(model.CheckBalanceCode, w, r)
}

func SubscriptionRedeemBalanceCode(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(model.RedeemBalanceCode, w, r)
}

func SubscriptionCreatePaymentId(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(model.SubscriptionCreatePaymentId, w, r)
}
