// SPDX-License-Identifier: MPL-2.0

package reengagement

// Where each part of a network's mail context came from
// (mmm/reengagement/PLAN.md "Server" 7 and 10; controller/mail_context.go).
// The country tries the onboarding row first. A network older than the
// onboarding program has none, so its country falls back to its purchases
// (the App Store storefront, then the Google Play region, then the Stripe
// card's billing country). The time zone and the locale try first what the
// network's own apps and browsers last reported (its durable mail context,
// written on every auth-client, sign-up and sign-in since 2026-11-13), then
// the onboarding row, then the reader's country, the purchase countries
// before the sign-up IP's; the time zone last the time of day the reader was
// last active, the locale the newest client event's. The platform falls back
// to the newer of the newest client event's and the device spec of the newest
// client. No source is an IP lookup made for this program; the onboarding
// row's zone and country are the sign-up IP's, recorded at enrollment.
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
	// the network's mail context: the zone or languages its apps and
	// browsers last reported on auth-client, sign-up or sign-in
	ContextSourceClient = "client"
	// the reader's country: its zone (onboarding.CountrySendZone) or its
	// language (onboarding.CountryLanguage)
	ContextSourceCountry = "country"
	// the time of day the reader was last active, the last connection or the
	// sign-up (onboarding.ActivitySendZone)
	ContextSourceActivity = "activity"
	// no source had it
	ContextSourceUnknown = "unknown"
)

// CountrySources are the country's sources in the order they are tried.
var CountrySources = []string{ContextSourceOnboarding, ContextSourceAppStore, ContextSourcePlay, ContextSourceStripe}
