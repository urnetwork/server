// SPDX-License-Identifier: MPL-2.0

package onboarding

import (
	"testing"
	"time"
)

// The offer emails' trial line said "Nothing is charged until {trial_end_at}",
// with trial_end_at the send time plus 14 days: false for a reader who starts
// the trial later, which the offer's own deadline invites, and for one who is
// not eligible for a trial (2026-11-13 review). The copy is relative now, so
// no offer email carries a trial date, in either branch or opening.
func TestOfferParamsCarryNoTrialDate(t *testing.T) {
	expires := time.Date(2026, 11, 20, 18, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		template string
		opening  string
	}{
		{TemplateE3LastChance, OpeningJoined},
		{TemplateE3LastChance, OpeningLastNote},
		{TemplateE4bOffer, ""},
	} {
		for _, storeBilled := range []bool{false, true} {
			p := BuildParams(ParamsInput{
				Template: c.template, Variant: VariantDefault, Opening: c.opening, StoreBilled: storeBilled,
				OfferExpiresAt: &expires, OfferPriceUsd: 29.99, RegularYearUsd: 39.99, DailyDataGb: 30,
				Location: time.UTC, Token: "T", SiteUrl: "https://ur.io",
			})
			if _, ok := p["trial_end_at"]; ok {
				t.Errorf("%s/%s (store billed %t) carries trial_end_at %q", c.template, c.opening, storeBilled, p["trial_end_at"])
			}
			// the deadline stays: the offer does end then
			if p["offer_expires_at"] == "" {
				t.Errorf("%s/%s (store billed %t) lost the offer's deadline", c.template, c.opening, storeBilled)
			}
		}
	}
}
