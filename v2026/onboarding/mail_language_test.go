// SPDX-License-Identifier: MPL-2.0

package onboarding

import (
	"slices"
	"testing"
)

func candidates(source string, tags ...string) []LanguageCandidate {
	out := []LanguageCandidate{}
	for _, tag := range tags {
		out = append(out, LanguageCandidate{Tag: tag, Source: source})
	}
	return out
}

// Tags normalize to language, script and region as given; junk, "*" and und
// are no tag.
func TestNormalizeLanguageTag(t *testing.T) {
	for _, c := range []struct {
		tag  string
		want string
	}{
		{"pt_br", "pt-BR"},
		{" de-DE ", "de-DE"},
		{"zh-Hant-TW", "zh-Hant-TW"},
		{"iw-IL", "he-IL"},
		{"in", "id"},
		{"en-US-u-ca-gregory", "en-US"},
		{"es-419", "es-419"},
		{"yue-Hant-HK", "yue-Hant-HK"},
		{"*", ""},
		{"und", ""},
		{"1x", ""},
		{"", ""},
		{"x-klingon", ""},
	} {
		if got := NormalizeLanguageTag(c.tag); got != c.want {
			t.Errorf("%q: %q, want %q", c.tag, got, c.want)
		}
	}
	if got := NormalizeLanguages([]string{"ja-JP", "ja-jp", "en", "junk!", "ko", "fr"}, 3); !slices.Equal(got, []string{"ja-JP", "en", "ko"}) {
		t.Errorf("normalized list %v", got)
	}
}

// The whole Accept-Language list is kept by q, not just its first tag: a
// Catalan browser that also lists Spanish keeps Spanish.
func TestAcceptLanguages(t *testing.T) {
	got := AcceptLanguages([]string{"ca-ES,es;q=0.9,en;q=0.8"}, 8)
	if !slices.Equal(got, []string{"ca-ES", "es", "en"}) {
		t.Fatalf("list %v", got)
	}
	if got := AcceptLanguages([]string{"en;q=0.2, ja-JP", "*;q=0.1, fr;q=0"}, 8); !slices.Equal(got, []string{"ja-JP", "en"}) {
		t.Errorf("by q, without * and q=0: %v", got)
	}
	if got := AcceptLanguages(nil, 8); len(got) != 0 {
		t.Errorf("no header: %v", got)
	}
	catalan := AcceptLanguages([]string{"ca-ES,es;q=0.9"}, 8)
	if locale, i := MatchTemplateLocale(candidates("client", catalan...), EmailLocales, DefaultMailLanguageSettings()); locale != "es" || i != 1 {
		t.Errorf("a Catalan reader who lists Spanish reads %q (candidate %d)", locale, i)
	}
}

// The traps of the 2026-11-12 probe: a match below High confidence is none
// (Persian is not Arabic, Bulgarian not Russian); a High match into another
// language is none (Kazakh, Belarusian, Armenian and Uyghur are not Russian or
// Chinese); Traditional Chinese reads zh-HK whatever the order of the list;
// Latin American Spanish reads es-419; an exact locale is itself.
func TestMatchTemplateLocaleTraps(t *testing.T) {
	settings := DefaultMailLanguageSettings()
	shuffled := []string{"zh", "pt", "es", "zh-HK", "en", "es-419", "pt-PT", "pt-BR", "es-MX", "ru", "ar", "de"}
	for _, c := range []struct {
		tag  string
		want string
	}{
		{"zh-TW", "zh-HK"},
		{"zh-Hant", "zh-HK"},
		{"zh-Hant-TW", "zh-HK"},
		{"zh-MO", "zh-HK"},
		{"zh-HK", "zh-HK"},
		{"zh-CN", "zh"},
		{"zh-Hans-HK", "zh"},
		{"zh", "zh"},
		{"cmn", "zh"},
		{"yue-HK", "zh-HK"},
		{"yue", "zh-HK"},
		{"es-AR", "es-419"},
		{"es-US", "es-419"},
		{"es-ES", "es"},
		{"es", "es"},
		{"es-MX", "es-MX"},
		{"pt", "pt"},
		{"pt-BR", "pt-BR"},
		{"pt-AO", "pt-PT"},
		{"de-CH", "de"},
		{"gsw-CH", "de"},
		{"arz", "ar"},
		{"en-GB", "en"},
		{"fa-IR", ""},
		{"bg-BG", ""},
		{"sr-RS", ""},
		{"kk-KZ", ""},
		{"be-BY", ""},
		{"hy", ""},
		{"az", ""},
		{"uz", ""},
		{"ky", ""},
		{"ug", ""},
		{"ca-ES", ""},
		{"fil-PH", ""},
		{"tr-TR", ""},
	} {
		got, _ := MatchTemplateLocale(candidates("client", c.tag), shuffled, settings)
		if got != c.want {
			t.Errorf("%s reads %q, want %q", c.tag, got, c.want)
		}
	}
	// the first candidate a template serves wins: Filipino has none, English
	// does
	if got, i := MatchTemplateLocale(candidates("client", "fil-PH", "en-US"), EmailLocales, settings); got != "en" || i != 1 {
		t.Errorf("[fil-PH en-US] reads %q (%d)", got, i)
	}
	if got, i := MatchTemplateLocale(candidates("client", "uk-UA", "ru-RU"), EmailLocales, settings); got != "uk" || i != 0 {
		t.Errorf("[uk-UA ru-RU] reads %q (%d)", got, i)
	}
}

// A reader known only by country gets its language under the strict rule:
// Japan and Korea their own; Taiwan and Hong Kong Traditional Chinese;
// Brazil, Mexico and Argentina their regional Portuguese or Spanish; the
// Netherlands and Austria theirs, second languages aside; Switzerland,
// Belgium, Canada, India, Luxembourg, the Philippines and every country
// without a qualifying language English; Ukraine, Kazakhstan and Belarus
// English until the owner sets them.
func TestCountryLanguage(t *testing.T) {
	settings := DefaultMailLanguageSettings()
	for _, c := range []struct {
		country string
		want    string
	}{
		{"JP", "ja"},
		{"jp", "ja"},
		{"KR", "ko"},
		{"TW", "zh-HK"},
		{"HK", "zh-HK"},
		{"MO", "zh-HK"},
		{"CN", "zh"},
		{"BR", "pt-BR"},
		{"PT", "pt-PT"},
		{"MX", "es-MX"},
		{"AR", "es-419"},
		{"ES", "es"},
		{"DE", "de"},
		{"AT", "de"},
		{"FR", "fr"},
		{"IT", "it"},
		{"NL", "nl"},
		{"IL", "he"},
		{"EG", "ar"},
		{"SA", "ar"},
		{"RU", "ru"},
		{"PL", "pl"},
		{"CZ", "cs"},
		{"GR", "el"},
		{"SE", "sv"},
		{"VN", "vi"},
		{"TH", "th"},
		{"TZ", "sw"},
		{"US", "en"},
		{"GB", "en"},
		{"CH", ""},
		{"BE", ""},
		{"CA", "en"},
		{"IN", ""},
		{"PH", ""},
		{"LU", ""},
		{"SG", ""},
		{"MY", ""},
		{"KE", ""},
		{"NG", ""},
		{"ZA", ""},
		{"UA", ""},
		{"KZ", ""},
		{"BY", ""},
		{"TR", ""},
		{"IR", ""},
		{"ZZ", ""},
		{"", ""},
	} {
		got, ok := CountryLanguage(c.country, EmailLocales, settings)
		if got != c.want || ok != (c.want != "") {
			t.Errorf("%q: %q (%t), want %q", c.country, got, ok, c.want)
		}
	}

	// the owner's override replaces the rule; a template must still serve it
	settings.CountryOverrides["UA"] = "uk"
	settings.CountryOverrides["CH"] = "en"
	settings.CountryOverrides["IR"] = "fa"
	for country, want := range map[string]string{"UA": "uk", "CH": "en", "IR": ""} {
		if got, _ := CountryLanguage(country, EmailLocales, settings); got != want {
			t.Errorf("override %s: %q, want %q", country, got, want)
		}
	}

	// a template set without the language falls to English
	if got, ok := CountryLanguage("JP", []string{"en", "de"}, DefaultMailLanguageSettings()); ok {
		t.Errorf("JP without a ja template: %q", got)
	}
}

// The candidates decide before the country, and English is the default.
func TestChooseTemplateLocale(t *testing.T) {
	settings := DefaultMailLanguageSettings()
	for _, c := range []struct {
		name      string
		languages MailLanguages
		locale    string
		source    string
	}{
		{"a reported language", MailLanguages{Candidates: candidates("client", "fr-CA"), Country: "JP", CountrySource: "country"}, "fr", "client"},
		{"a list without a template falls to the country", MailLanguages{Candidates: candidates("client", "ca-ES"), Country: "JP", CountrySource: "country"}, "ja", "country"},
		{"Japan by country", MailLanguages{Country: "JP", CountrySource: "country"}, "ja", "country"},
		{"Korea by country", MailLanguages{Country: "KR", CountrySource: "country"}, "ko", "country"},
		{"Switzerland stays English", MailLanguages{Country: "CH", CountrySource: "country"}, "en", ""},
		{"nothing known", MailLanguages{}, "en", ""},
	} {
		locale, source := ChooseTemplateLocale(c.languages, EmailLocales, settings)
		if locale != c.locale || source != c.source {
			t.Errorf("%s: %q from %q, want %q from %q", c.name, locale, source, c.locale, c.source)
		}
	}
	if locale, _ := ChooseTemplateLocale(MailLanguages{Country: "JP"}, []string{"de"}, settings); locale != "" {
		t.Errorf("no English template: %q", locale)
	}
}

// ServesAnyLanguage tells a list that needs the reader's country.
func TestServesAnyLanguage(t *testing.T) {
	settings := DefaultMailLanguageSettings()
	if ServesAnyLanguage([]string{"ca-ES", "fil"}, settings) {
		t.Error("Catalan and Filipino have templates")
	}
	if !ServesAnyLanguage([]string{"ca-ES", "es"}, settings) {
		t.Error("Spanish has none")
	}
	if ServesAnyLanguage(nil, settings) {
		t.Error("an empty list serves")
	}
}

// Each default settings call is a new instance.
func TestMailLanguageSettingsAreFresh(t *testing.T) {
	a := DefaultMailLanguageSettings()
	a.CountryOverrides["UA"] = "uk"
	a.LanguageRewrites["xx"] = "en"
	b := DefaultMailLanguageSettings()
	if _, ok := b.CountryOverrides["UA"]; ok {
		t.Error("overrides are shared")
	}
	if _, ok := b.LanguageRewrites["xx"]; ok {
		t.Error("rewrites are shared")
	}
	if c := DefaultMailContextSettings(); c.MaxLanguages <= 0 || c.MaxClients <= 0 || c.RefreshInterval <= 0 {
		t.Errorf("context settings %+v", c)
	}
}
