// SPDX-License-Identifier: MPL-2.0

package onboarding

import (
	"testing"
)

// The redeem link is Apple's documented form for our app's id, with the code
// as the last query value, escaped. The landing page accepts exactly this
// form (mmm ur.io landingPlan.js appleRedeemUrl).
func TestAppleOfferCodeRedeemUrl(t *testing.T) {
	if got := AppleOfferCodeRedeemUrl("ABCDEFGHJKLMNPQR23"); got != "https://apps.apple.com/redeem?ctx=offercodes&id=6741000606&code=ABCDEFGHJKLMNPQR23" {
		t.Errorf("one-time code: %s", got)
	}
	if got := AppleOfferCodeRedeemUrl("a b&c"); got != "https://apps.apple.com/redeem?ctx=offercodes&id=6741000606&code=a+b%26c" {
		t.Errorf("escaping: %s", got)
	}
}
