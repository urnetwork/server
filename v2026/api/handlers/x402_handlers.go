package handlers

import (
	"net/http"

	"github.com/urnetwork/server/v2026/controller"
)

// x402 is a header protocol: the signed payment arrives in `X-PAYMENT` and the
// "not paid yet" answer is a 402 status carrying the payment terms. So these are
// raw handlers rather than the usual router.Wrap* impls, which own the status code
// and cannot see request headers.

// X402Skus lists what an agent can buy and for how much.
func X402Skus(w http.ResponseWriter, r *http.Request) {
	controller.X402SkusHandler(w, r)
}

// X402Purchase quotes payment terms (402) or settles a signed payment and grants.
func X402Purchase(w http.ResponseWriter, r *http.Request) {
	controller.X402PurchaseHandler(w, r)
}
