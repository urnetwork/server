package controller

import (
	"encoding/json"
	"net/http"

	"github.com/urnetwork/glog"
	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server/model"
)

// A refused quota's 402 with x402 terms (DESIGN.md 20.7): WriteX402UpgradeRequired's
// counterpart for a sidecar limit, beside it because that one takes only an
// AuthNetworkClientResult. x402 sells a Pro month and data, never Team, so it quotes
// only a Community network's refusal that Pro's own limit lifts. Every other refusal,
// and every one while x402 is off or cannot quote, takes the plain QuotaRefusal body
// through the error path (model.QuotaRefusalError): a person buys in an app or a
// console. D-QUOTA-5: agents pay only under I-8, and no sidecar path pays, since §7.4's
// allowlist refuses X-PAYMENT to a sidecar token (X402SettleInlineUpgrade).

// The body of a quoted refusal: x402's PaymentRequired, with the QuotaRefusal's fields
// beside its own, so a reader of either finds what it reads. The two share no name.
type x402QuotaRefusalBody struct {
	*X402PaymentRequired
	sidecar.QuotaRefusal
}

// Writes a 402 with x402 terms for the refusal and returns true, for a Community
// network's refusal that a Pro month lifts, with x402 on and able to quote. resource is
// the refused request's path, which the terms name. False writes nothing, and the caller
// answers the refusal's own body.
func WriteX402QuotaRefusal(w http.ResponseWriter, resource string, networkTier model.NetworkTier, refusal *sidecar.QuotaRefusal) bool {
	if refusal == nil || !refusal.Upgrade || 0 < refusal.RetryAfterMs {
		return false
	}
	if networkTier != model.NetworkTierFree || !model.QuotaLimitLiftedByTier(model.NetworkTierPro, refusal.LimitKey, refusal.Used) {
		return false
	}
	if !X402Enabled() {
		return false
	}
	paymentRequired, err := X402PaymentRequiredFor(resource, X402SkuProMonth, "quota limit "+refusal.LimitKey+" reached")
	if err != nil {
		// cannot quote, such as with no Pro price: the plain body rather than broken terms
		glog.Errorf("[x402]could not quote quota terms: %s\n", err)
		return false
	}
	body, err := json.Marshal(&x402QuotaRefusalBody{
		X402PaymentRequired: paymentRequired,
		QuotaRefusal:        *refusal,
	})
	if err != nil {
		return false
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusPaymentRequired)
	w.Write(body)
	return true
}
