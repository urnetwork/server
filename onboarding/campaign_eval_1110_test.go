// SPDX-License-Identifier: MPL-2.0

package onboarding

// Onboarding/retention evaluation 2026-11-10: the copy and trigger fixes. Each
// test reproduces a defect the evaluation found on Main.

import (
	"testing"
	"time"

	"golang.org/x/text/language"
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

// T6: which Chinese locales read Traditional characters. The script decides
// where there is one; without one, Hong Kong, Macau and Taiwan do.
func TestTraditionalChinese(t *testing.T) {
	for locale, want := range map[string]bool{
		"zh-TW": true, "zh-HK": true, "zh-MO": true, "zh_TW": true, "zh-tw": true,
		"zh-Hant": true, "zh-hant": true, "zh-Hant-TW": true, "zh-Hant-US": true, "zh-Hant-CN": true,
		"zh": false, "zh-CN": false, "zh-SG": false, "zh-US": false,
		"zh-Hans": false, "zh-Hans-HK": false, "zh-Hans-TW": false,
		"": false, "en-TW": false, "ja-Hant": false,
	} {
		if got := TraditionalChinese(locale); got != want {
			t.Errorf("TraditionalChinese(%q) = %t, want %t", locale, got, want)
		}
	}
	// the date and device-word styles follow the same rule
	for _, locale := range []string{"zh-TW", "zh-MO", "zh-Hant", "zh-Hant-US", "zh-Hans-HK", "zh-CN", "zh-SG", "zh"} {
		want := "zh"
		if TraditionalChinese(locale) {
			want = "zh-HK"
		}
		if got := EmailLocale(locale); got != want {
			t.Errorf("EmailLocale(%q) = %q, want %q", locale, got, want)
		}
	}
}

// T6: the store locale a regional tag reads before its bare language. The
// Spanish and Portuguese region lists are CLDR's parent locales: every listed
// region's tag has es-419 or pt-PT for its parent in golang.org/x/text's CLDR
// data.
func TestRegionalStoreLocale(t *testing.T) {
	for locale, want := range map[string]string{
		"zh-TW": "zh-HK", "zh-MO": "zh-HK", "zh-HK": "zh-HK", "zh-Hant": "zh-HK", "zh-Hant-US": "zh-HK", "zh_hant_tw": "zh-HK",
		"zh": "", "zh-CN": "", "zh-SG": "", "zh-Hans-HK": "",
		"es-AR": "es-419", "es-us": "es-419", "es_CO": "es-419", "es-MX": "es-419", "es-ES": "", "es-419": "", "es": "",
		"pt-AO": "pt-PT", "pt-MZ": "pt-PT", "pt-BR": "", "pt-PT": "", "pt": "",
		"de-AT": "", "en-US": "", "fr-CA": "", "": "",
	} {
		if got := RegionalStoreLocale(locale); got != want {
			t.Errorf("RegionalStoreLocale(%q) = %q, want %q", locale, got, want)
		}
	}
	for region := range latinAmericanSpanishRegions {
		if parent := language.MustParse("es-" + region).Parent().String(); parent != "es-419" {
			t.Errorf("es-%s: the CLDR parent is %s, not es-419", region, parent)
		}
	}
	for region := range europeanPortugueseRegions {
		if parent := language.MustParse("pt-" + region).Parent().String(); parent != "pt-PT" {
			t.Errorf("pt-%s: the CLDR parent is %s, not pt-PT", region, parent)
		}
	}
	// the regions the evaluation found reading es instead of es-419
	for _, region := range []string{"AR", "CO", "CL", "PE", "VE", "UY", "EC", "BO", "PY", "CR", "GT", "HN", "NI", "PA", "SV", "DO", "PR", "CU", "US"} {
		if got := RegionalStoreLocale("es-" + region); got != "es-419" {
			t.Errorf("es-%s reads %q", region, got)
		}
	}
}
