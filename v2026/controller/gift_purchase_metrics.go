package controller

// What the gift purchase routes answer (POST /pay/gift/checkout,
// /pay/gift/crypto-intent and /pay/gift/gamma-intent), counted once per
// answer by rail and result. A buyer can be turned away before any order is
// stored, so without this a refusal left no trace in production: the
// per-address limit (which buyers behind one VPN exit share), the friend-email
// daily cap, a validation error, sales closed, a Stripe session that could not
// be made, a coin that cannot be quoted, a Gamma purchase the deposit cannot
// take. Successes count too, so a rail's refusal rate is its refusals over its
// answers.
//
// The result is ok, or the refusal code the route answered when it is one of
// the codes these routes use (giftPurchaseRefusalCodes); any other code
// counts as other, and an answer that is not a 200 (a Go error) as error. The
// labels are closed sets of codes, never message text, an address, an order
// or an amount.

import (
	"github.com/prometheus/client_golang/prometheus"
)

// The rails, as the purchase routes are named for them.
const (
	giftPurchaseRailCard  = "card"
	giftPurchaseRailCoin  = "coin"
	giftPurchaseRailGamma = "gamma"
)

var giftPurchaseRails = []string{
	giftPurchaseRailCard,
	giftPurchaseRailCoin,
	giftPurchaseRailGamma,
}

const (
	// an order stored and its payment started: a Checkout session, a coin
	// invoice or a Gamma intent handed to the buyer
	giftPurchaseResultOk = "ok"
	// a refusal whose code is none of giftPurchaseRefusalCodes
	giftPurchaseResultOther = "other"
	// answered with an error, not a refusal: a non-2xx answer
	giftPurchaseResultError = "error"
)

// The refusal codes the gift purchase routes answer, each a result of its own:
// the gift rules (gift_checkout_controller.go, gift_gamma_controller.go), the
// purchase codes the three rails share (subscription_controller.go), and the
// Gamma purchase checks' (gamma_controller.go).
var giftPurchaseRefusalCodes = []string{
	GiftErrorCodeSalesClosed,
	GiftErrorCodeInvalidQuantity,
	GiftErrorCodeInvalidRecipients,
	GiftErrorCodeInvalidSendOn,
	GiftErrorCodeMessageTooLong,
	GiftErrorCodeNoteHasLink,
	GiftErrorCodeDailyLimit,
	GiftErrorCodePaymentInProgress,
	PurchaseErrorCodeInvalidRequest,
	PurchaseErrorCodeInvalidEmail,
	PurchaseErrorCodeRateLimited,
	PurchaseErrorCodeStartFailed,
	PurchaseErrorCodeCheckoutUnavailable,
	PurchaseErrorCodeAssetUnavailable,
	PurchaseErrorCodeItemUnavailable,
	PurchaseErrorCodeGuestSignInRequired,
	PurchaseErrorCodeAlreadySubscribed,
	PurchaseErrorCodePlanUnavailable,
	PurchaseErrorCodeOfferUnavailable,
	GammaErrorCodeInsufficientGamma,
	GammaErrorCodeDailyCapExceeded,
	GammaErrorCodeDailyCapTooLow,
	GammaErrorCodeNoDeposit,
	GammaErrorCodeInvalidAmount,
	GammaErrorCodeMintTooSmall,
	GammaErrorCodeDepositColdkeyMismatch,
	GammaErrorCodeMintLiquidityExceeded,
	GammaErrorCodeMintSlippage,
}

var giftPurchaseKnownRefusals = func() map[string]bool {
	known := map[string]bool{}
	for _, code := range giftPurchaseRefusalCodes {
		known[code] = true
	}
	return known
}()

// Every result label: ok, other, error and each known refusal code.
func giftPurchaseResults() []string {
	return append(
		[]string{giftPurchaseResultOk, giftPurchaseResultOther, giftPurchaseResultError},
		giftPurchaseRefusalCodes...,
	)
}

var giftPurchases = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "gift",
	Name:      "purchases_total",
	Help:      "Answers of the gift purchase routes (POST /pay/gift/checkout, /pay/gift/crypto-intent, /pay/gift/gamma-intent) by rail (card, coin, gamma) and result: ok (an order stored and its payment started), the refusal code the route answered (gift_sales_closed, invalid_quantity, rate_limited, gift_daily_limit, start_failed, checkout_unavailable, asset_unavailable, insufficient_gamma, ...), other (a refusal code outside the known set), error (a non-2xx answer)",
}, []string{"rail", "result"})

func init() {
	prometheus.MustRegister(giftPurchases)
	// every series from zero, so an increase sees each one's first count
	for _, rail := range giftPurchaseRails {
		for _, result := range giftPurchaseResults() {
			giftPurchases.WithLabelValues(rail, result)
		}
	}
}

// giftPurchaseResultLabel is the result of one answer: error for a Go error or
// no answer, the refusal's code when it is a known one, other for any other
// refusal, else ok.
func giftPurchaseResultLabel(err error, answered bool, refused bool, code string) string {
	switch {
	case err != nil || !answered:
		return giftPurchaseResultError
	case !refused:
		return giftPurchaseResultOk
	case giftPurchaseKnownRefusals[code]:
		return code
	default:
		return giftPurchaseResultOther
	}
}

// countGiftPurchase counts one answer of a gift purchase route.
func countGiftPurchase(rail string, err error, answered bool, refused bool, code string) {
	giftPurchases.WithLabelValues(rail, giftPurchaseResultLabel(err, answered, refused, code)).Inc()
}
