package handlers

import (
	"net/http"
	"io"
	"encoding/json"
	"bytes"

    "bringyour.com/bringyour"
	// "bringyour.com/bringyour/router"
)


// https://stripe.com/docs/webhooks
// https://stripe.com/docs/webhooks#verify-official-libraries
// https://github.com/stripe/stripe-go
func StripeWebhook(w http.ResponseWriter, req *http.Request) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
        return
	}

	out := &bytes.Buffer{}
	json.Compact(out, []byte(body))

	bringyour.Logger().Printf("Stripe webhook body: %s\n", out.Bytes())

	w.Header().Set("Content-Type", "application/json")
    w.Write([]byte("{}"))
}


// https://docs.cloud.coinbase.com/commerce/docs/webhooks#subscribing-to-a-webhook
// The signature is included as a X-CC-Webhook-Signature header. This header contains the SHA256 HMAC signature of the raw request payload, computed using your webhook shared secret as the key.
func CoinbaseWebhook(w http.ResponseWriter, req *http.Request) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
        return
	}

	out := &bytes.Buffer{}
	json.Compact(out, []byte(body))

	bringyour.Logger().Printf("Coinbase webhook body: %s\n", out.Bytes())

	w.Header().Set("Content-Type", "application/json")
    w.Write([]byte("{}"))
}


func CircleWebhook(w http.ResponseWriter, req *http.Request) {
	body, err := io.ReadAll(req.Body)
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
