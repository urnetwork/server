package model

import (
	"fmt"
	"net/http"

	"github.com/urnetwork/sidecar"
)

// A sidecar limit's refusal as an error (DESIGN.md 20.7's Refusals paragraph), which the
// router answers through its two seams (router/handler_utils.go): RetryAfterSeconds, for
// the Retry-After header, and HttpErrorResultBody, the QuotaRefusal itself as the body.
// The status follows the refusal:
//
//   - 429 with Retry-After when a window resets (RetryAfterMs set);
//   - 402 when a purchase or a higher tier lifts the limit (Upgrade set). A handler that
//     can quote terms writes them first (controller.WriteX402QuotaRefusal); otherwise the
//     402 carries the QuotaRefusal body alone, as evs's does (plan-QUOTA-small-readings
//     #18);
//   - else 429 with the body and no Retry-After, which is RESOURCE_EXHAUSTED over HTTP
//     (plan-QUOTA-small-readings #13).
//
// D-QUOTA-5: an agent pays only under I-8. No sidecar path pays a 402: §7.4's allowlist
// refuses purchase routes to sidecar tokens, so the refusal surfaces and a person buys in
// an app or a console.
type QuotaRefusalError struct {
	Refusal sidecar.QuotaRefusal
}

// The error for refusal, which it copies.
func NewQuotaRefusalError(refusal *sidecar.QuotaRefusal) *QuotaRefusalError {
	return &QuotaRefusalError{
		Refusal: *refusal,
	}
}

// The HTTP status the refusal answers with.
func (self *QuotaRefusalError) HttpStatus() int {
	switch {
	case 0 < self.Refusal.RetryAfterMs:
		return http.StatusTooManyRequests
	case self.Refusal.Upgrade:
		return http.StatusPaymentRequired
	default:
		return http.StatusTooManyRequests
	}
}

// "<status> <message>", the router's convention for a declared status. It names the
// limit's key and no network.
func (self *QuotaRefusalError) Error() string {
	return fmt.Sprintf("%d Quota limit %s reached.", self.HttpStatus(), self.Refusal.LimitKey)
}

// Whole seconds until the window resets, rounded up; zero for no window.
func (self *QuotaRefusalError) RetryAfterSeconds() int {
	if self.Refusal.RetryAfterMs == 0 {
		return 0
	}
	return int((int64(self.Refusal.RetryAfterMs) + 999) / 1000)
}

// The QuotaRefusal, the body of every quota answer.
func (self *QuotaRefusalError) HttpErrorResultBody() any {
	refusal := self.Refusal
	return &refusal
}

// The tiers above tier, lowest first.
func higherNetworkTiers(tier NetworkTier) []NetworkTier {
	switch tier {
	case NetworkTierFree:
		return []NetworkTier{NetworkTierPro, NetworkTierTeam}
	case NetworkTierPro:
		return []NetworkTier{NetworkTierTeam}
	default:
		return nil
	}
}

// Whether tier's own limit for limitKey, from pro.yml, would admit one more past
// usedCount: `none`, or above it. Only a tier's sidecar: keys grow with the tier; the
// sidecar_abuse: keys are equal on every tier (D-QUOTA-7), so a tier lifts none of them.
func QuotaLimitLiftedByTier(tier NetworkTier, limitKey string, usedCount int64) bool {
	row, ok := proSidecarLimitRowForKey(limitKey)
	if !ok || row.block != proSidecarBlockTier {
		return false
	}
	limit := Pro().TierFor(tier).Sidecar.Limit(limitKey)
	return limit == proSidecarLimitNone || usedCount < limit
}

// Whether a higher tier than tier lifts limitKey past usedCount, which is what
// QuotaRefusal.Upgrade says. A network_sidecar_limit row holds whatever the tier, so a
// key with a row is never lifted by one.
func quotaRefusalUpgrade(tier NetworkTier, limitKey string, usedCount int64, hasRow bool) bool {
	if hasRow {
		return false
	}
	for _, higherTier := range higherNetworkTiers(tier) {
		if QuotaLimitLiftedByTier(higherTier, limitKey, usedCount) {
			return true
		}
	}
	return false
}
