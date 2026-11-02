// SPDX-License-Identifier: MPL-2.0

package reengagement

// Where each part of a network's mail context came from
// (mmm/reengagement/PLAN.md "Server" 7 and 10). The onboarding row is tried
// first. A network older than the onboarding program has none, so its country
// falls back to its purchases (the App Store storefront, then the Google Play
// region, then the Stripe card's billing country), and its locale and
// platform to what its own clients last reported (their newest client event,
// and for the platform also the device spec of the newest client, whichever is
// more recent). No source is an IP lookup made for this program; the
// onboarding row's country is the sign-up IP's, recorded at enrollment. The
// time zone has no source but the onboarding row: clients report it only on
// auth-client, and only the onboarding row keeps it.
const (
	// the network's onboarding row (networks enrolled since 2026-09-15)
	ContextSourceOnboarding = "onboarding"
	// the storefront of the network's latest App Store subscription row
	ContextSourceAppStore = "app_store"
	// the region of the network's latest Google Play subscription row
	ContextSourcePlay = "play"
	// the billing country of the network's Stripe card
	ContextSourceStripe = "stripe"
	// the network's newest client event (POST /client/events)
	ContextSourceClientEvent = "client_event"
	// the device spec of the network's newest client
	ContextSourceDevice = "device"
	// no source had it
	ContextSourceUnknown = "unknown"
)

// CountrySources are the country's sources in the order they are tried.
var CountrySources = []string{ContextSourceOnboarding, ContextSourceAppStore, ContextSourcePlay, ContextSourceStripe}
