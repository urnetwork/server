// SPDX-License-Identifier: MPL-2.0

package controller

// Onboarding/retention evaluation 2026-11-10: the copy and trigger fixes that
// live in the controller. Pure: none of these needs a database.

import (
	"testing"

	"github.com/urnetwork/server/model"
)

// T6: normalizeLocale keeps the language and the region, so a bare "zh-Hant"
// was stored as "zh" and "zh-Hant-US" (Traditional characters, read in the
// United States) as "zh-US": both then got the Simplified zh templates. The
// stored form keeps the characters now; the region still stands in for the
// script where it implies it (zh-Hant-TW -> zh-TW, as before).
func TestNormalizeLocaleKeepsTheChineseCharacters(t *testing.T) {
	cases := map[string]string{
		"zh-Hant": "zh-Hant", "zh_Hant": "zh-Hant", "zh-hant": "zh-Hant",
		"zh-Hant-US": "zh-Hant", "zh-Hant-MY": "zh-Hant",
		"zh-Hans-HK": "zh", "zh-Hans-TW": "zh",
		// the region implies the script: as before
		"zh-Hant-TW": "zh-TW", "zh-Hant-HK": "zh-HK", "zh-Hant-MO": "zh-MO",
		"zh-Hans-CN": "zh-CN", "zh-Hans": "zh", "zh-TW": "zh-TW", "zh-CN": "zh-CN", "zh": "zh",
		// other languages: as before
		"sr-Latn-RS": "sr-RS", "es-419": "es-419", "pt-br": "pt-BR",
	}
	for in, want := range cases {
		got := normalizeLocale(in)
		if got != want {
			t.Errorf("normalizeLocale(%q) = %q, want %q", in, got, want)
		}
		// re-engagement normalizes the stored value again
		if again := normalizeLocale(got); again != got {
			t.Errorf("normalizeLocale(%q) = %q is not stable: %q", in, got, again)
		}
	}

	// and the stored form reaches the store's Traditional templates
	b := &model.OnboardingBrevoConfig{Templates: map[string]map[string]map[string]int{
		"e1_connect": {"default": {"en": 1, "zh": 2, "zh-HK": 3}},
	}}
	for in, want := range map[string]string{
		"zh-Hant": "zh-HK", "zh-Hant-US": "zh-HK", "zh-Hant-TW": "zh-HK", "zh-TW": "zh-HK", "zh-MO": "zh-HK",
		"zh-Hans-HK": "zh", "zh-Hans": "zh", "zh-CN": "zh",
	} {
		if _, matched, _ := b.TemplateIdLocale("e1_connect", "default", normalizeLocale(in)); matched != want {
			t.Errorf("%q (stored %q) reads %q, want %q", in, normalizeLocale(in), matched, want)
		}
	}
}
