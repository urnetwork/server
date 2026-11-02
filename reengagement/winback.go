// SPDX-License-Identifier: MPL-2.0

package reengagement

// The win-back offer W1 issues (mmm/reengagement/PLAN.md "THE STORES",
// decision 5, accepted 2026-11-01): the monthly plan's first month at half
// price on Google Play and Stripe, Apple's own win-back offer on the monthly
// product, nothing on yearly, nothing for crypto. Pure, like the rest of the
// package: the controller loads the facts, asks the App Store, talks to Stripe
// and records the offer (controller/winback_offer_controller.go).

import (
	"math"
	"strings"
	"time"
)

// Stores an offer is redeemed on (network_winback_offer.store and the plan
// response's winback_offer.store).
const (
	WinbackStoreStripe = "stripe"
	WinbackStorePlay   = "play"
	WinbackStoreApple  = "apple"
)

// The W experiment's arms (reengagement.yml experiment on the
// reengagement.winback surface): the holdout gets no email (and so no offer),
// control is mailed without an offer, offer is mailed with the offer where the
// network's store has one (Google Play and Stripe).
const (
	ArmHoldout = "holdout"
	ArmControl = "control"
	ArmOffer   = "offer"
)

// WinbackOfferInterval is how long after one offer the network may get the
// next: one offer per network per 365 days, whatever became of the first.
const WinbackOfferInterval = 365 * 24 * time.Hour

// WelcomeOfferPromisePercentOff is the welcome offer the onboarding emails
// promise "goes away and doesn't come back": 25% off a first year
// (mmm/onboarding/REVIEW.md finding 10). No later offer may equal or beat it.
const WelcomeOfferPromisePercentOff = 25

// WinbackMaxPercentOff is the accepted shape's discount (PLAN.md decision 5):
// half price on one month. A larger discount is refused, whatever it is worth.
const WinbackMaxPercentOff = 50

// WinbackStoreForMarket is the store an offer may be issued on, from the
// market of the network's last paid supporter window: the store the network
// paid through. Crypto, x402 and manual windows have no monthly plan to
// discount, so "" (W1 without an offer).
func WinbackStoreForMarket(market string) string {
	switch strings.ToLower(strings.TrimSpace(market)) {
	case "stripe":
		return WinbackStoreStripe
	case "google":
		return WinbackStorePlay
	case "apple":
		return WinbackStoreApple
	}
	return ""
}

// Why W1 issues no offer (the result label of
// urnetwork_reengagement_winback_offers_total), or "" when it may.
const (
	WinbackRefusalDisabled = "disabled"
	WinbackRefusalNoStore  = "no_store"
	WinbackRefusalHoldout  = "holdout"
	WinbackRefusalNoArm    = "no_offer_arm"
	// Stripe would not charge the discounted first month (StripeMinimumChargeUsdCents)
	WinbackRefusalBelowMinimumCharge = "below_minimum_charge"
	WinbackRefusalCountry            = "country"
	WinbackRefusalNeverPaid          = "never_paid"
	WinbackRefusalRecent             = "recent_offer"
	// the App Store does not list the offer for the subscription (asked last)
	WinbackRefusalAppleIneligible = "apple_ineligible"
)

// WinbackOfferFacts are what the offer decision reads for one network at its
// W1 send.
type WinbackOfferFacts struct {
	// reengagement.yml winback_offer is on and its shape passes the rule over
	// every offer (WinbackShapeRefusal)
	Enabled bool
	// the network's arm in the W experiment, "" when none runs
	Arm string
	// WinbackStoreForMarket of the network's last paid window
	Store string
	// the mail policy lets an offer go to the network's country; an unknown
	// country may not (onboarding.MailPolicy.OfferAllowed)
	CountryAllowed bool
	// the network paid for at least one period: not a trial alone, and no
	// refund, dispute or store revocation on record
	Paid bool
	// an offer was issued to the network in the WinbackOfferInterval before now
	OfferedWithinInterval bool
	// Stripe only: the discounted first month is below Stripe's minimum charge
	// at a tier the network may be priced on (WinbackStripeChargeable)
	StripeBelowMinimumCharge bool
}

// WinbackOfferRefusal is why W1 may not issue an offer to the network, or ""
// when it may, on f.Store. Google Play and Stripe offers go to the offer arm
// only; the App Store's is Apple's own win-back offer, so every mailed arm may
// get it, and only when Apple lists it for the subscription, which the
// caller asks after this passes.
func WinbackOfferRefusal(f WinbackOfferFacts) string {
	switch {
	case !f.Enabled:
		return WinbackRefusalDisabled
	case f.Store == "":
		return WinbackRefusalNoStore
	case f.Arm == ArmHoldout:
		return WinbackRefusalHoldout
	case f.Store != WinbackStoreApple && f.Arm != ArmOffer:
		return WinbackRefusalNoArm
	case f.Store == WinbackStoreStripe && f.StripeBelowMinimumCharge:
		return WinbackRefusalBelowMinimumCharge
	case !f.CountryAllowed:
		return WinbackRefusalCountry
	case !f.Paid:
		return WinbackRefusalNeverPaid
	case f.OfferedWithinInterval:
		return WinbackRefusalRecent
	}
	return ""
}

// WinbackTierPrices are one price tier's regular prices.
type WinbackTierPrices struct {
	Name       string
	MonthlyUsd float64
	YearlyUsd  float64
}

func usdCents(usd float64) int64 {
	return int64(math.Round(usd * 100))
}

// WinbackShapeRefusal is why a configured offer shape breaks the rule over
// every offer (mmm/onboarding/STORES.md "The rule every store offer must
// meet"), or "" when it keeps it: the monthly plan only, at most the accepted
// WinbackMaxPercentOff off its first month, and worth less than the welcome
// offer at every price tier. The welcome offer is the configured one, never
// read above the promised WelcomeOfferPromisePercentOff: the promise binds
// the networks that got it whatever the configuration says later.
func WinbackShapeRefusal(plan string, percentOff int, welcomePercentOff int, tiers []WinbackTierPrices) string {
	if plan != "monthly" {
		return "plan " + plan + " is not the monthly plan"
	}
	if percentOff <= 0 || WinbackMaxPercentOff < percentOff {
		return "percent_off is outside (0, 50]"
	}
	welcome := WelcomeOfferPromisePercentOff
	if 0 < welcomePercentOff && welcomePercentOff < welcome {
		welcome = welcomePercentOff
	}
	if len(tiers) == 0 {
		return "no price tiers to check the offer against"
	}
	for _, tier := range tiers {
		monthly, yearly := usdCents(tier.MonthlyUsd), usdCents(tier.YearlyUsd)
		if monthly <= 0 || yearly <= 0 {
			return "tier " + tier.Name + " has no monthly or yearly price"
		}
		// percent-cents on both sides: one month at percentOff against the
		// welcome offer's percent off a first year
		if int64(welcome)*yearly <= int64(percentOff)*monthly {
			return "tier " + tier.Name + ": the offer equals or beats the welcome offer"
		}
	}
	return ""
}

// StripeMinimumChargeUsdCents is Stripe's minimum charge in USD, the currency
// every tier is priced in. Stripe does not charge a non-zero invoice below it:
// it marks the invoice paid and moves the amount to the customer's balance,
// to be added to the next invoice (customer balance type invoice_too_small).
// A discounted first month below it would be neither charged at half price
// nor paid for when the subscription starts (the regional tier's monthly
// plan is 0.50 USD, so half of it is 0.25 USD), so the Stripe offer is not
// made there.
const StripeMinimumChargeUsdCents = 50

// WinbackFirstPeriodUsdCents is what the first month costs at the offer:
// the monthly price less percentOff of it, in cents.
func WinbackFirstPeriodUsdCents(monthlyUsd float64, percentOff int) int64 {
	monthly := usdCents(monthlyUsd)
	return monthly - int64(math.Round(float64(monthly)*float64(percentOff)/100))
}

// WinbackStripeChargeable reports whether Stripe charges the first month at
// the offer: at least its minimum charge. (Stripe also charges nothing on a
// zero invoice, but no offer of the accepted shape is free.)
func WinbackStripeChargeable(monthlyUsd float64, percentOff int) bool {
	return StripeMinimumChargeUsdCents <= WinbackFirstPeriodUsdCents(monthlyUsd, percentOff)
}

// The Play purchase that used a free phase alone spans at most the trial and
// the store's day of slack (controller SubscriptionGracePeriod); a window
// longer than this by a day was charged at least once.
const PlayTrialWindowDays = 14 + 1 + 1
