// SPDX-License-Identifier: MPL-2.0

package onboarding

// Who the campaign emails may go to, by country and by the evidence behind the
// product-updates preference (mmm/onboarding/REVIEW.md finding 12,
// mmm/reengagement/PLAN.md "Audiences" and RESEARCH.md §5). Pure: the
// controllers pass the facts and the configured lists, so every rule has a
// unit test. Counsel reviews the lists; they are data in onboarding.yml
// `policy`, and these are only the defaults.

import (
	"strings"
	"time"
)

// How a network's product-updates preference came to be what it is
// (account_preferences.product_updates_source).
const (
	// a sign-up whose form showed the "Periodic product updates" switch: the
	// create request carried the field
	ConsentSourceSignupSwitch = "signup_switch"
	// a sign-up from a client that never showed the switch: the server's default
	ConsentSourceSignupDefault = "signup_default"
	// the user changed it in settings
	ConsentSourceSettings = "settings"
	// an unsubscribe link or a mailbox's one-click unsubscribe
	ConsentSourceUnsubscribe = "unsubscribe"
	// an explicit opt-in (a control that starts off)
	ConsentSourceOptIn = "opt_in"
	// recorded before the source was tracked
	ConsentSourceUnknown = ""
)

// What a mail is for: the rules differ by purpose (Canada's implied consent
// covers a fresh sign-up but not a free account months later).
const (
	PurposeOnboarding   = "onboarding"
	PurposeReengagement = "reengagement"
	PurposeWinback      = "winback"
)

// Policy exit reasons (recorded as campaign exits).
const (
	ExitCountryNoMarketing = "country_no_marketing"
	ExitConsentEvidence    = "consent_evidence"
	ExitCanadaConsent      = "canada_consent"
)

// MailPolicy is the configured rule set.
type MailPolicy struct {
	// countries that get no campaign mail at all (explicit-consent regimes,
	// advertising bans), ISO 3166-1 alpha-2
	NoMarketingCountries []string
	// countries that never get an offer (prices, discounts) by email
	NoOfferCountries []string
	// countries where mail needs evidence that the reader saw the opt-out at
	// collection or chose to receive it (EU/EEA/UK soft opt-in)
	EvidenceCountries []string
	// treat an unknown country as an evidence country (the strictest reading)
	EvidenceWhenCountryUnknown bool
}

// Defaults, used when onboarding.yml has no `policy` block.
var (
	DefaultNoMarketingCountries = []string{"CN", "RU"}
	DefaultNoOfferCountries     = []string{"IR", "CU", "KP", "SY"}
	// the EU, the EEA, the United Kingdom and Switzerland
	DefaultEvidenceCountries = []string{
		"AT", "BE", "BG", "HR", "CY", "CZ", "DK", "EE", "FI", "FR", "DE", "GR", "HU", "IE",
		"IT", "LV", "LT", "LU", "MT", "NL", "PL", "PT", "RO", "SK", "SI", "ES", "SE",
		"IS", "LI", "NO", "GB", "CH",
	}
)

// DefaultMailPolicy is the rule set without configuration.
func DefaultMailPolicy() MailPolicy {
	return MailPolicy{
		NoMarketingCountries:       DefaultNoMarketingCountries,
		NoOfferCountries:           DefaultNoOfferCountries,
		EvidenceCountries:          DefaultEvidenceCountries,
		EvidenceWhenCountryUnknown: true,
	}
}

// Canada's implied consent (CASL s.10(9), 10(14)): an inquiry within 6 months,
// a purchase within 2 years of the subscription's end.
const (
	CanadaInquiryWindow  = 183 * 24 * time.Hour
	CanadaPurchaseWindow = 2 * 365 * 24 * time.Hour
)

// MailFacts are the facts the policy reads for one network and send.
type MailFacts struct {
	// ISO 3166-1 alpha-2, "" when unknown
	Country       string
	ConsentSource string
	// when the network was created (the "inquiry")
	CreatedAt time.Time
	// the end of the network's latest paid subscription window, if any
	LastPaidEnd *time.Time
	Now         time.Time
}

func normalizeCountry(country string) string {
	return strings.ToUpper(strings.TrimSpace(country))
}

func containsCountry(list []string, country string) bool {
	if country == "" {
		return false
	}
	for _, c := range list {
		if normalizeCountry(c) == country {
			return true
		}
	}
	return false
}

// HasConsentEvidence reports whether the preference was set where the reader
// saw it: the sign-up switch, settings, or an explicit opt-in.
func HasConsentEvidence(source string) bool {
	switch source {
	case ConsentSourceSignupSwitch, ConsentSourceSettings, ConsentSourceOptIn:
		return true
	default:
		return false
	}
}

// Exit is the policy's exit reason for a campaign mail, or "" when it may go.
// It does not look at the product-updates value itself (the campaigns check
// that first).
func (p MailPolicy) Exit(purpose string, f MailFacts) string {
	country := normalizeCountry(f.Country)
	if containsCountry(p.NoMarketingCountries, country) {
		return ExitCountryNoMarketing
	}
	if country == "CA" && purpose != PurposeOnboarding {
		// past the sign-up: an explicit opt-in, or a purchase within two
		// years of the subscription's end
		if f.ConsentSource == ConsentSourceOptIn {
			return ""
		}
		if f.LastPaidEnd != nil && f.Now.Sub(*f.LastPaidEnd) <= CanadaPurchaseWindow {
			return ""
		}
		if purpose == PurposeReengagement && !f.CreatedAt.IsZero() && f.Now.Sub(f.CreatedAt) <= CanadaInquiryWindow {
			return ""
		}
		return ExitCanadaConsent
	}
	needsEvidence := containsCountry(p.EvidenceCountries, country) || (country == "" && p.EvidenceWhenCountryUnknown)
	if needsEvidence && !HasConsentEvidence(f.ConsentSource) {
		return ExitConsentEvidence
	}
	return ""
}

// OfferAllowed reports whether a mail may carry an offer (a price or a
// discount) to this country. An unknown country may not.
func (p MailPolicy) OfferAllowed(country string) bool {
	country = normalizeCountry(country)
	if country == "" {
		return false
	}
	return !containsCountry(p.NoOfferCountries, country)
}
