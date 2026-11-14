package handlers

import (
	"fmt"
	"net/http"

	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/router"
	"github.com/urnetwork/server/v2026/session"
)

// Selling URnetwork Pro gift codes (controller/gift_checkout_controller.go,
// mmm/GIFTOFPRIVACY.md §10.2). Every route is public; the two purchases take
// an optional sign-in, which only `referral_credit` reads.

// GiftConfig is GET /pay/gift/config: whether gifts are sold, the tiers and
// the coins.
func GiftConfig(w http.ResponseWriter, r *http.Request) {
	router.WrapNoAuth(controller.GiftConfig, w, r)
}

// GiftCheckout is POST /pay/gift/checkout: an order paid by card through
// Stripe Checkout, embedded or hosted.
func GiftCheckout(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputOptionalAuth(controller.GiftCheckout, w, r)
}

// GiftCryptoIntent is POST /pay/gift/crypto-intent: an order paid with a
// coin, through the crypto invoice the site's CryptoPayPanel renders.
func GiftCryptoIntent(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputOptionalAuth(controller.GiftCryptoIntent, w, r)
}

// GiftOrder is POST /pay/gift/order {order_id, token}: the order's status,
// 404 for a token that does not open it. The token travels in the body, never
// in the url, where it would land in access logs.
func GiftOrder(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputNoAuth(controller.GiftOrderStatus, w, r, func(_ *session.ClientSession, _ *controller.GiftOrderStatusResult) bool {
		// it carries the buyer's masked address and changes as the order is
		// paid: never cached
		w.Header().Set("Cache-Control", "no-store")
		return false
	})
}

// GiftCardsPdf is POST /pay/gift/cards.pdf {order_id, token}: every card of
// the order, one per page, as a download (the dialog saves it as a Blob). 404
// until the cards are rendered.
func GiftCardsPdf(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputNoAuth(controller.GiftCardsPdf, w, r, func(_ *session.ClientSession, result *controller.GiftCardsPdfResult) bool {
		writeGiftCardsPdf(w, result)
		return true
	})
}

// writeGiftCardsPdf writes the cards as a download. The cards carry the
// codes, so nothing may keep a copy.
func writeGiftCardsPdf(w http.ResponseWriter, result *controller.GiftCardsPdfResult) {
	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", controller.GiftCardsPdfFileName))
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(result.Pdf)))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Write(result.Pdf)
}

// GiftCheckCode is POST /pay/gift/check-code: a code's kind and status
// without a sign-in, for ur.io/redeem.
func GiftCheckCode(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputNoAuth(controller.GiftCheckCode, w, r)
}
