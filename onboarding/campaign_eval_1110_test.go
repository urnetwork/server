// SPDX-License-Identifier: MPL-2.0

package onboarding

// Onboarding/retention evaluation 2026-11-10: the copy and trigger fixes. Each
// test reproduces a defect the evaluation found on Main.

import (
	"testing"
	"time"
)

// T5: E1's footer line ("Your welcome offer, 3 months of Pro free, is
// available until ...") reached path-A networks the offer may not be emailed
// to: onboarding.yml no_offer_countries (IR, CU, KP, SY) and an unknown
// country. The offer steps (E3a, E4b, E5b) stood down there; E1 named the
// offer anyway, because its params set offer_expires_at whenever an offer
// existed. E1 follows the offer steps' rule now.
func TestE1OfferLineFollowsTheOfferRule(t *testing.T) {
	expires := time.Date(2026, 9, 14, 22, 5, 0, 0, time.UTC)
	f := healthy(PathA)
	f.OfferState = OfferStateActive
	f.NoOfferEmail = true
	d := Decide(StepE1, f)
	if d.Action != ActionSend || d.Template != TemplateE1Connect {
		t.Fatalf("e1 where the offer may not be emailed: %+v", d)
	}
	if !d.NoOfferEmail {
		t.Errorf("the e1 decision does not carry the offer rule: %+v", d)
	}
	in := ParamsInput{
		Template: TemplateE1Connect, Variant: VariantDefault, NoOfferEmail: true,
		OfferExpiresAt: &expires, DailyDataGb: 30, Location: time.UTC, Now: expires.Add(-4 * 24 * time.Hour),
		Token: "T", SiteUrl: "https://ur.io",
	}
	if got := BuildParams(in)["offer_expires_at"]; got != "" {
		t.Errorf("e1 states the offer where it may not be emailed: offer_expires_at %q", got)
	}

	// where the offer may be emailed, path A keeps the line
	f.NoOfferEmail = false
	if d := Decide(StepE1, f); d.NoOfferEmail {
		t.Errorf("e1 where the offer may be emailed: %+v", d)
	}
	in.NoOfferEmail = false
	if got := BuildParams(in)["offer_expires_at"]; got != "Sep 14, 2026 at 10:05 PM UTC" {
		t.Errorf("e1 offer line where the offer may be emailed: %q", got)
	}
	// and without an offer there is no line either way
	in.OfferExpiresAt = nil
	if got := BuildParams(in)["offer_expires_at"]; got != "" {
		t.Errorf("e1 offer line without an offer: %q", got)
	}

	// the countries behind the fact: the default no-offer list, and an
	// unknown country
	policy := DefaultMailPolicy()
	for _, country := range []string{"IR", "CU", "KP", "SY", ""} {
		if policy.OfferAllowed(country) {
			t.Errorf("an offer may be emailed to %q", country)
		}
	}
	if !policy.OfferAllowed("US") {
		t.Error("an offer may be emailed to US")
	}
}
