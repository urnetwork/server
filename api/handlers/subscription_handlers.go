package handlers

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"

	"bringyour.com/bringyour"
	"bringyour.com/bringyour/controller"
	"bringyour.com/bringyour/model"
	"bringyour.com/bringyour/router"
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

// TODO this is not used currently
func CircleWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	out := &bytes.Buffer{}
	json.Compact(out, []byte(body))

	bringyour.Logger().Printf("Circle webhook body: %s\n", out.Bytes())

	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte("{}"))
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
