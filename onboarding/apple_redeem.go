// SPDX-License-Identifier: MPL-2.0

package onboarding

import (
	"net/url"
)

// AppStoreAppId is URnetwork's Apple ID in the App Store: the App Store
// Connect app "URnetwork", bundle network.ur (read 2026-11-15), the id in
// the listing https://apps.apple.com/us/app/urnetwork/id6741000606.
const AppStoreAppId = "6741000606"

// AppleOfferCodeRedeemUrl is Apple's redeem link for an offer code of our
// app: it opens the App Store's redeem sheet with the code filled in, for a
// one-time code and a custom code alike
// (https://apps.apple.com/redeem?ctx=offercodes&id=<Apple ID>&code=<code>).
// The offer landing opens it for an App Store reader (owner, 2026-11-15:
// the welcome offer by email to iPhone readers).
func AppleOfferCodeRedeemUrl(code string) string {
	return "https://apps.apple.com/redeem?ctx=offercodes&id=" + AppStoreAppId + "&code=" + url.QueryEscape(code)
}
