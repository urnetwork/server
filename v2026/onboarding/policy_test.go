// SPDX-License-Identifier: MPL-2.0

package onboarding

import (
	"slices"
	"testing"
	"time"
)

func TestMailPolicyCountries(t *testing.T) {
	p := DefaultMailPolicy()
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	created := now.Add(-10 * 24 * time.Hour)
	cases := []struct {
		name    string
		purpose string
		facts   MailFacts
		want    string
	}{
		{"us default", PurposeOnboarding, MailFacts{Country: "US", ConsentSource: ConsentSourceSignupDefault, Now: now}, ""},
		{"lowercase country", PurposeOnboarding, MailFacts{Country: "us", Now: now}, ""},
		{"china", PurposeOnboarding, MailFacts{Country: "CN", ConsentSource: ConsentSourceOptIn, Now: now}, ExitCountryNoMarketing},
		{"russia", PurposeReengagement, MailFacts{Country: "RU", ConsentSource: ConsentSourceSignupSwitch, Now: now}, ExitCountryNoMarketing},
		{"germany without evidence", PurposeOnboarding, MailFacts{Country: "DE", ConsentSource: ConsentSourceSignupDefault, Now: now}, ExitConsentEvidence},
		{"germany unknown source", PurposeOnboarding, MailFacts{Country: "DE", Now: now}, ExitConsentEvidence},
		{"germany with the switch shown", PurposeOnboarding, MailFacts{Country: "DE", ConsentSource: ConsentSourceSignupSwitch, Now: now}, ""},
		{"uk from settings", PurposeReengagement, MailFacts{Country: "GB", ConsentSource: ConsentSourceSettings, Now: now}, ""},
		{"unknown country without evidence", PurposeOnboarding, MailFacts{Country: "", ConsentSource: ConsentSourceSignupDefault, Now: now}, ExitConsentEvidence},
		{"unknown country with evidence", PurposeOnboarding, MailFacts{Country: "", ConsentSource: ConsentSourceSignupSwitch, Now: now}, ""},
		{"canada onboarding", PurposeOnboarding, MailFacts{Country: "CA", Now: now, CreatedAt: created}, ""},
		{"canada re-engagement inside six months", PurposeReengagement, MailFacts{Country: "CA", Now: now, CreatedAt: created}, ""},
		{"canada re-engagement after six months", PurposeReengagement, MailFacts{Country: "CA", Now: now, CreatedAt: now.Add(-200 * 24 * time.Hour)}, ExitCanadaConsent},
		{"canada opted in", PurposeReengagement, MailFacts{Country: "CA", ConsentSource: ConsentSourceOptIn, Now: now, CreatedAt: now.Add(-900 * 24 * time.Hour)}, ""},
		{"canada win-back inside two years", PurposeWinback, MailFacts{Country: "CA", Now: now, LastPaidEnd: ptrTime(now.Add(-400 * 24 * time.Hour))}, ""},
		{"canada win-back after two years", PurposeWinback, MailFacts{Country: "CA", Now: now, LastPaidEnd: ptrTime(now.Add(-800 * 24 * time.Hour))}, ExitCanadaConsent},
		{"canada win-back never paid", PurposeWinback, MailFacts{Country: "CA", Now: now, CreatedAt: created}, ExitCanadaConsent},
	}
	for _, c := range cases {
		if got := p.Exit(c.purpose, c.facts); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

// The sanctioned countries (Iran, Cuba, North Korea, Syria) get no marketing
// mail of any purpose by default, as China and Russia; before 2026-11-13 only
// their offers were held back, and re-engagement and onboarding mail still
// went to them.
func TestMailPolicyDefaultNoMarketingCoversSanctionedCountries(t *testing.T) {
	p := DefaultMailPolicy()
	now := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	for _, country := range []string{"CN", "RU", "IR", "CU", "KP", "SY", "ir", " sy "} {
		if !p.NoMarketing(country) {
			t.Errorf("%q: marketing allowed", country)
		}
		for _, purpose := range []string{PurposeOnboarding, PurposeReengagement, PurposeWinback} {
			// the strongest consent there is does not reopen it
			facts := MailFacts{Country: country, ConsentSource: ConsentSourceOptIn, CreatedAt: now, LastPaidEnd: &now, Now: now}
			if got := p.Exit(purpose, facts); got != ExitCountryNoMarketing {
				t.Errorf("%q %s: exit %q, want %q", country, purpose, got, ExitCountryNoMarketing)
			}
		}
	}
	for _, country := range []string{"US", "UA", "DE", ""} {
		if p.NoMarketing(country) {
			t.Errorf("%q: no marketing", country)
		}
	}
	// a configured list replaces the default
	if q := (MailPolicy{NoMarketingCountries: []string{"FR"}}); !q.NoMarketing("fr") || q.NoMarketing("IR") {
		t.Error("configured no-marketing list")
	}
	// the codes the product-updates queries compare stored countries with
	if codes := p.NoMarketingCountryCodes(); !slices.Equal(codes, []string{"CN", "RU", "IR", "CU", "KP", "SY"}) {
		t.Error("default codes", codes)
	}
	if codes := (MailPolicy{NoMarketingCountries: []string{" ir", "IR", "", "cu "}}).NoMarketingCountryCodes(); !slices.Equal(codes, []string{"IR", "CU"}) {
		t.Error("normalized codes", codes)
	}
	if codes := (MailPolicy{}).NoMarketingCountryCodes(); codes == nil || len(codes) != 0 {
		t.Error("an empty list", codes)
	}
}

// Each default policy owns its lists: a caller that edits one does not change
// the defaults the next caller gets.
func TestDefaultMailPolicyCopiesTheDefaults(t *testing.T) {
	p := DefaultMailPolicy()
	p.NoMarketingCountries[0] = "US"
	p.NoOfferCountries[0] = "US"
	p.EvidenceCountries[0] = "US"
	q := DefaultMailPolicy()
	if q.NoMarketing("US") || !q.NoMarketing("CN") || !q.OfferAllowed("US") || q.OfferAllowed("IR") {
		t.Error("an edit to one policy's lists reached the defaults")
	}
	if got := q.Exit(PurposeOnboarding, MailFacts{Country: "AT", Now: time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)}); got != ExitConsentEvidence {
		t.Errorf("an edit to one policy's evidence list reached the defaults: %q", got)
	}
}

func TestMailPolicyOffers(t *testing.T) {
	p := DefaultMailPolicy()
	for country, want := range map[string]bool{"US": true, "de": true, "IR": false, "CU": false, "KP": false, "SY": false, "": false} {
		if got := p.OfferAllowed(country); got != want {
			t.Errorf("offer to %q: got %t, want %t", country, got, want)
		}
	}
	// configured lists replace the defaults
	q := MailPolicy{NoOfferCountries: []string{"FR"}}
	if q.OfferAllowed("FR") || !q.OfferAllowed("IR") {
		t.Error("configured no-offer list")
	}
	if HasConsentEvidence(ConsentSourceSignupDefault) || HasConsentEvidence(ConsentSourceUnsubscribe) || !HasConsentEvidence(ConsentSourceOptIn) {
		t.Error("evidence sources")
	}
}

func ptrTime(t time.Time) *time.Time { return &t }
