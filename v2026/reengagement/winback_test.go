// SPDX-License-Identifier: MPL-2.0

package reengagement

import (
	"strings"
	"testing"
	"time"
)

// The store an offer goes out on is the store the network paid through; the
// markets without a monthly plan to discount have none.
func TestWinbackStoreForMarket(t *testing.T) {
	for market, want := range map[string]string{
		"stripe": WinbackStoreStripe,
		"google": WinbackStorePlay,
		"apple":  WinbackStoreApple,
		"Apple ": WinbackStoreApple,
		"solana": "",
		"monero": "",
		"zcash":  "",
		"x402":   "",
		"manual": "",
		"":       "",
	} {
		if got := WinbackStoreForMarket(market); got != want {
			t.Errorf("market %q: store %q, want %q", market, got, want)
		}
	}
}

func eligibleWinbackFacts(store string, arm string) WinbackOfferFacts {
	return WinbackOfferFacts{Enabled: true, Arm: arm, Store: store, CountryAllowed: true, Paid: true}
}

// Every constraint refuses on its own, in the order the metric reports it;
// Google Play and Stripe go to the offer arm only, the App Store's offer to
// every mailed arm.
func TestWinbackOfferRefusal(t *testing.T) {
	for _, store := range []string{WinbackStoreStripe, WinbackStorePlay} {
		if got := WinbackOfferRefusal(eligibleWinbackFacts(store, ArmOffer)); got != "" {
			t.Fatalf("%s offer arm: refused %q", store, got)
		}
		for _, arm := range []string{ArmControl, ""} {
			if got := WinbackOfferRefusal(eligibleWinbackFacts(store, arm)); got != WinbackRefusalNoArm {
				t.Errorf("%s arm %q: %q, want no_offer_arm", store, arm, got)
			}
		}
	}
	for _, arm := range []string{ArmOffer, ArmControl, ""} {
		if got := WinbackOfferRefusal(eligibleWinbackFacts(WinbackStoreApple, arm)); got != "" {
			t.Errorf("apple arm %q: refused %q", arm, got)
		}
	}
	for _, test := range []struct {
		name  string
		facts func(f *WinbackOfferFacts)
		want  string
	}{
		{"disabled", func(f *WinbackOfferFacts) { f.Enabled = false }, WinbackRefusalDisabled},
		{"crypto, x402 or manual", func(f *WinbackOfferFacts) { f.Store = "" }, WinbackRefusalNoStore},
		{"holdout", func(f *WinbackOfferFacts) { f.Arm = ArmHoldout }, WinbackRefusalHoldout},
		{"no-offer or unknown country", func(f *WinbackOfferFacts) { f.CountryAllowed = false }, WinbackRefusalCountry},
		{"trial only or refunded", func(f *WinbackOfferFacts) { f.Paid = false }, WinbackRefusalNeverPaid},
		{"an offer in the last 365 days", func(f *WinbackOfferFacts) { f.OfferedWithinInterval = true }, WinbackRefusalRecent},
	} {
		for _, store := range []string{WinbackStoreStripe, WinbackStorePlay, WinbackStoreApple} {
			f := eligibleWinbackFacts(store, ArmOffer)
			test.facts(&f)
			if got := WinbackOfferRefusal(f); got != test.want {
				t.Errorf("%s on %s: %q, want %q", test.name, store, got, test.want)
			}
		}
	}
	// the holdout is refused on every store, the App Store included
	if got := WinbackOfferRefusal(eligibleWinbackFacts(WinbackStoreApple, ArmHoldout)); got != WinbackRefusalHoldout {
		t.Errorf("apple holdout: %q", got)
	}
	// a first month Stripe would not charge refuses Stripe only
	for _, store := range []string{WinbackStoreStripe, WinbackStorePlay, WinbackStoreApple} {
		f := eligibleWinbackFacts(store, ArmOffer)
		f.StripeBelowMinimumCharge = true
		want := ""
		if store == WinbackStoreStripe {
			want = WinbackRefusalBelowMinimumCharge
		}
		if got := WinbackOfferRefusal(f); got != want {
			t.Errorf("below Stripe's minimum on %s: %q, want %q", store, got, want)
		}
	}
}

// Stripe charges the standard tier's half month (2.50 USD) and not the
// regional tier's (0.25 USD, below its 0.50 USD minimum).
func TestWinbackStripeChargeable(t *testing.T) {
	for _, test := range []struct {
		monthlyUsd float64
		percentOff int
		cents      int64
		chargeable bool
	}{
		{5, 50, 250, true},
		{0.5, 50, 25, false},
		{1, 50, 50, true},
		{0.99, 50, 49, false},
		{0.5, 0, 50, true},
	} {
		if got := WinbackFirstPeriodUsdCents(test.monthlyUsd, test.percentOff); got != test.cents {
			t.Errorf("%g at %d%%: %d cents, want %d", test.monthlyUsd, test.percentOff, got, test.cents)
		}
		if got := WinbackStripeChargeable(test.monthlyUsd, test.percentOff); got != test.chargeable {
			t.Errorf("%g at %d%%: chargeable %t, want %t", test.monthlyUsd, test.percentOff, got, test.chargeable)
		}
	}
}

var winbackTestTiers = []WinbackTierPrices{
	{Name: "standard", MonthlyUsd: 5, YearlyUsd: 40},
	{Name: "regional", MonthlyUsd: 0.5, YearlyUsd: 4},
}

// The accepted shape keeps the rule at both tiers: half of one month ($2.50,
// $0.25) is worth less than a quarter of a first year ($10, $1). Anything
// yearly, larger than half, or worth as much as the welcome offer is refused.
func TestWinbackShapeRefusal(t *testing.T) {
	if got := WinbackShapeRefusal("monthly", 50, 25, winbackTestTiers); got != "" {
		t.Fatalf("the accepted shape is refused: %s", got)
	}
	// a welcome offer configured off still binds at the promised 25%
	if got := WinbackShapeRefusal("monthly", 50, 0, winbackTestTiers); got != "" {
		t.Fatalf("the accepted shape against the promise: %s", got)
	}
	for _, test := range []struct {
		name       string
		plan       string
		percentOff int
		welcome    int
		tiers      []WinbackTierPrices
		want       string
	}{
		{"nothing on yearly", "yearly", 50, 25, winbackTestTiers, "not the monthly plan"},
		{"no discount", "monthly", 0, 25, winbackTestTiers, "outside"},
		{"more than half", "monthly", 51, 25, winbackTestTiers, "outside"},
		{"a free month", "monthly", 100, 25, winbackTestTiers, "outside"},
		// half a $20 month equals a quarter of a $40 year: equal is refused
		{"equal to the welcome offer", "monthly", 50, 25, []WinbackTierPrices{{Name: "dear", MonthlyUsd: 20, YearlyUsd: 40}}, "equals or beats"},
		{"better than the welcome offer", "monthly", 50, 25, []WinbackTierPrices{{Name: "dearer", MonthlyUsd: 25, YearlyUsd: 40}}, "equals or beats"},
		// a smaller configured welcome offer is the stricter reference
		{"against a smaller welcome offer", "monthly", 50, 5, winbackTestTiers, "equals or beats"},
		{"an unpriced tier", "monthly", 50, 25, []WinbackTierPrices{{Name: "free", MonthlyUsd: 0, YearlyUsd: 40}}, "no monthly or yearly price"},
		{"no tiers", "monthly", 50, 25, nil, "no price tiers"},
	} {
		got := WinbackShapeRefusal(test.plan, test.percentOff, test.welcome, test.tiers)
		if got == "" || !strings.Contains(got, test.want) {
			t.Errorf("%s: %q, want a refusal containing %q", test.name, got, test.want)
		}
	}
	// a larger configured welcome offer is read at the promised 25%
	if got := WinbackShapeRefusal("monthly", 50, 90, []WinbackTierPrices{{Name: "dear", MonthlyUsd: 20, YearlyUsd: 40}}); got == "" {
		t.Error("a welcome offer configured above the promise loosened the rule")
	}
}

// W1's offer variant carries the offer's real expiry, formatted for the reader;
// the default variant carries the App Store line instead, and never a date.
func TestWinbackParams(t *testing.T) {
	now := time.Date(2026, 12, 1, 9, 0, 0, 0, time.UTC)
	expires := time.Date(2026, 12, 15, 10, 0, 0, 0, time.UTC)
	ended := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	offer := BuildParams(ParamsInput{
		Template: TemplateW1Winback, Variant: VariantOffer, Locale: "en", Location: time.UTC, Now: now,
		SiteUrl: "https://ur.io", Token: "T", ProEndedAt: &ended, OfferExpiresAt: &expires, AppleOfferHint: true,
	})
	if offer["offer_expires_at"] == "" || !strings.Contains(offer["offer_expires_at"], "15") {
		t.Errorf("offer expiry: %q", offer["offer_expires_at"])
	}
	if _, ok := offer["apple_offer_hint"]; ok {
		t.Error("the offer variant carries the App Store line")
	}
	if offer["cta_url"] != "https://ur.io/o/winback?t=T" {
		t.Errorf("offer cta: %q", offer["cta_url"])
	}
	apple := BuildParams(ParamsInput{
		Template: TemplateW1Winback, Variant: VariantDefault, Locale: "en", Location: time.UTC, Now: now,
		SiteUrl: "https://ur.io", Token: "T", ProEndedAt: &ended, AppleOfferHint: true,
	})
	if apple["apple_offer_hint"] != "1" {
		t.Errorf("apple hint: %v", apple)
	}
	if _, ok := apple["offer_expires_at"]; ok {
		t.Error("the default variant carries an expiry")
	}
	plain := BuildParams(ParamsInput{Template: TemplateW1Winback, Variant: VariantDefault, SiteUrl: "https://ur.io", Token: "T", Now: now})
	if plain["apple_offer_hint"] != "" {
		t.Errorf("default without Apple's offer: %v", plain)
	}
}
