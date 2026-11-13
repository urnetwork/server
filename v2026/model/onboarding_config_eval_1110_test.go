package model

// Onboarding/retention evaluation 2026-11-10: the template locale fallbacks.

import (
	"testing"
)

// storeLocales are the 28 locales mmm/onboarding/build.sh and
// mmm/reengagement/build.mjs upload every template in.
var storeLocales = []string{
	"ar", "cs", "de", "el", "en", "es", "es-419", "es-MX", "fr", "he", "hi", "id", "it", "ja",
	"ko", "nl", "pl", "pt", "pt-BR", "pt-PT", "ru", "sv", "sw", "th", "uk", "vi", "zh", "zh-HK",
}

// T6: the lookup went exact tag -> language -> en, so zh-TW, zh-MO and
// zh-Hant(-*) readers got the Simplified zh templates although the store has
// zh-HK, and Latin American Spanish (es-AR, es-CO, ..., es-US) got es (Spain)
// although the store has es-419. Both programs select templates here
// (OnboardingBrevoConfig is also reengagement.yml's brevo block).
func TestTemplateLocaleRegionalFallbacks(t *testing.T) {
	ids := map[string]int{}
	for i, locale := range storeLocales {
		ids[locale] = 100 + i
	}
	b := &OnboardingBrevoConfig{Templates: map[string]map[string]map[string]int{
		"e1_connect": {"default": ids},
	}}
	cases := map[string]string{
		// Traditional Chinese reads the store's Traditional locale
		"zh-TW": "zh-HK", "zh-MO": "zh-HK", "zh_TW": "zh-HK", "zh-HK": "zh-HK",
		"zh-Hant": "zh-HK", "zh-Hant-TW": "zh-HK", "zh-Hant-HK": "zh-HK", "zh-Hant-MO": "zh-HK", "zh-Hant-US": "zh-HK",
		// Simplified stays Simplified, whatever the region
		"zh": "zh", "zh-CN": "zh", "zh-SG": "zh", "zh-Hans": "zh", "zh-Hans-CN": "zh", "zh-Hans-HK": "zh",
		// Spanish: the region's own template, else es-419 for Latin America
		// and the United States (CLDR parent locales), else es
		"es-MX": "es-MX", "es-419": "es-419", "es-ES": "es", "es": "es", "es-GQ": "es",
		// Portuguese: Brazil and Portugal have their own; Portuguese-speaking
		// Africa and Asia read pt-PT (CLDR parent locales), the rest pt
		"pt-BR": "pt-BR", "pt-PT": "pt-PT", "pt-AO": "pt-PT", "pt-MZ": "pt-PT", "pt-CV": "pt-PT", "pt": "pt", "pt-US": "pt",
		// everything else as before
		"de": "de", "de-AT": "de", "en-GB": "en", "fr-CA": "fr", "xx": "en", "": "en",
	}
	for _, region := range []string{"AR", "BO", "BR", "BZ", "CL", "CO", "CR", "CU", "DO", "EC", "GT", "HN", "NI", "PA", "PE", "PR", "PY", "SV", "US", "UY", "VE"} {
		cases["es-"+region] = "es-419"
	}
	for in, want := range cases {
		id, matched, ok := b.TemplateIdLocale("e1_connect", "default", in)
		if !ok || matched != want || id != ids[want] {
			t.Errorf("%q: template %d in %q, want %d in %q", in, id, matched, ids[want], want)
		}
	}

	// a store without the regional locale falls back to the language, as
	// before
	sparse := &OnboardingBrevoConfig{Templates: map[string]map[string]map[string]int{
		"e1_connect": {"default": {"en": 1, "es": 2, "pt": 3, "zh": 4}},
	}}
	for in, want := range map[string]string{"es-AR": "es", "pt-AO": "pt", "zh-TW": "zh", "zh-Hant": "zh"} {
		if _, matched, _ := sparse.TemplateIdLocale("e1_connect", "default", in); matched != want {
			t.Errorf("without the regional locale %q: %q, want %q", in, matched, want)
		}
	}

	// the order of the candidates, without repeats
	for in, want := range map[string][]string{
		"zh-TW": {"zh-TW", "zh-HK", "zh", "en"},
		"es-AR": {"es-AR", "es-419", "es", "en"},
		"es":    {"es", "en"},
		"zh-HK": {"zh-HK", "zh", "en"},
		"":      {"en"},
	} {
		got := TemplateLocaleCandidates(in)
		if len(got) != len(want) {
			t.Errorf("candidates %q: %v, want %v", in, got, want)
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("candidates %q: %v, want %v", in, got, want)
				break
			}
		}
	}
}
