// SPDX-License-Identifier: MPL-2.0

package onboarding

import (
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
