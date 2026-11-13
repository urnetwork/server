// SPDX-License-Identifier: MPL-2.0

package onboarding

// Which language a reader's campaign emails are written in (the campaign
// review of 2026-11-13: the locale was missing for 80% of re-engagement
// episodes and most readers in Japan and Korea got English). The reader's
// languages are kept as an ordered list, the way Stripe keeps
// preferred_locales: what their devices and browsers report, newest first.
// At send time the whole list is matched against the locales the email has
// templates in, so a template added later serves its readers at once.
//
// Matching uses golang.org/x/text/language with three guards the 2026-11-12
// probe of the 28 email locales called for:
//   - a match below High confidence is no match: x/text answers Persian with
//     Arabic and Bulgarian or Serbian with Russian, at No;
//   - a match must keep the reader's language: x/text answers Kazakh,
//     Belarusian, Armenian, Uzbek or Kyrgyz with Russian, and Uyghur with
//     Chinese, at High, which reads as a political choice, not a translation.
//     Only the written languages of LanguageRewrites cross over (Cantonese
//     reads Chinese, Egyptian Arabic reads Arabic, Swiss German reads German);
//   - regional locales are tried before their language, so zh-TW and zh-Hant
//     read zh-HK (Traditional), not zh, and es-AR reads es-419.
//
// A reader known only by country gets the country's language under a strict
// rule (CountryLanguage), on CLDR's population shares; any other gets
// English.

import (
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/text/language"
)

// languageShare is one language of a country in the generated CLDR table.
type languageShare struct {
	// BCP 47, as CLDR names it ("zh-Hant", "gsw")
	language string
	// the share of the country's people who use it, first or second
	// language, at the fluency a computer or phone needs (CLDR
	// populationPercent); a country's shares can sum past 100
	percent float64
	// it has an official standing in the country: official, de facto or
	// regional (CLDR officialStatus)
	official bool
}

// MailLanguageSettings decide which language a reader's campaign emails are
// written in. The owner's choices for single countries go in
// CountryOverrides (onboarding.yml mail_language.country_overrides).
type MailLanguageSettings struct {
	// The least share of a country's people, in percent, its top language
	// needs for readers known only by country to get it (CLDR
	// populationPercent). Default 80: Japanese (95), Korean (100), Brazilian
	// Portuguese (91) and Mexican Spanish (83) pass; Swiss German (76),
	// Ukrainian (65) and Hindi (41) do not.
	CountryLanguageMinPercent float64
	// The most share, in percent, any other official language of the country
	// may have, English and the languages that read the top language's
	// template aside. Default 50: Luxembourg (Luxembourgish 67), Tunisia
	// (French 53) and Aruba are multilingual and stay English. Only a language
	// with an official standing there counts: CLDR's shares include second
	// languages, and German as the Netherlands' second language (71) or
	// Bavarian in Austria (95) does not make either country multilingual.
	CountryOtherLanguageMaxPercent float64
	// Maps an alpha-2 country to the language its readers known only by
	// country get, in place of the rule ("UA": "uk"); "en" pins English.
	// Empty by default: Ukraine, Kazakhstan and Belarus are the owner's call,
	// a political choice as well as a linguistic one, and the rule leaves
	// each in English.
	CountryOverrides map[string]string
	// Maps a language to the written language whose templates serve it,
	// keeping its script and region: Chinese varieties read Chinese (yue to
	// zh, so yue-HK reads zh-HK), Arabic varieties Arabic, Swiss German and
	// Bavarian German. No other cross-language match is taken.
	LanguageRewrites map[string]string
}

// DefaultMailLanguageSettings returns a new instance on each call.
func DefaultMailLanguageSettings() *MailLanguageSettings {
	rewrites := map[string]string{
		"gsw": "de",
		"bar": "de",
	}
	for _, chinese := range []string{"yue", "nan", "hak", "wuu", "gan", "hsn", "cdo", "cpx", "mnp"} {
		rewrites[chinese] = "zh"
	}
	for _, arabic := range []string{"arz", "ary", "arq", "aeb", "apc", "apd", "ars", "acm", "afb", "ayl", "ajp", "acq"} {
		rewrites[arabic] = "ar"
	}
	return &MailLanguageSettings{
		CountryLanguageMinPercent:      80,
		CountryOtherLanguageMaxPercent: 50,
		CountryOverrides:               map[string]string{},
		LanguageRewrites:               rewrites,
	}
}

// MailContextSettings bound what the server keeps of a network's mail
// context (model.NetworkMailContext) and how often an unchanged report
// rewrites it.
type MailContextSettings struct {
	// How many of a reader's languages are kept, most preferred first.
	// Default 8: a browser lists a handful, an OS a few more. Non-positive
	// keeps none.
	MaxLanguages int
	// How many app builds are kept, one per platform and distribution, the
	// newest first. Default 6. Non-positive keeps none.
	MaxClients int
	// An unchanged report renews a stored value's time only once that time
	// is this old, so a device that authenticates many times a day writes
	// the row at most this often. Default 24 h. Non-positive renews on every
	// report.
	RefreshInterval time.Duration
}

// DefaultMailContextSettings returns a new instance on each call.
func DefaultMailContextSettings() *MailContextSettings {
	return &MailContextSettings{
		MaxLanguages:    8,
		MaxClients:      6,
		RefreshInterval: 24 * time.Hour,
	}
}

// NormalizeLanguageTag is a BCP 47 tag in its canonical form, language,
// script and region as given ("pt_br" is "pt-BR", "iw" is "he", "zh-Hant-TW"
// stays), extensions and variants dropped; "" for junk, "*", und and mul.
func NormalizeLanguageTag(tag string) string {
	tag = strings.TrimSpace(strings.ReplaceAll(tag, "_", "-"))
	if tag == "" || tag == "*" || 64 < len(tag) {
		return ""
	}
	parsed, err := language.Parse(tag)
	if err != nil {
		return ""
	}
	base, baseConfidence := parsed.Base()
	if baseConfidence != language.Exact {
		return ""
	}
	switch base.String() {
	// no language, several (x/text reads Accept-Language's * as mul), no
	// linguistic content, one ISO 639 has no code for
	case "und", "mul", "zxx", "mis":
		return ""
	}
	normalized := base.String()
	if script, confidence := parsed.Script(); confidence == language.Exact {
		normalized += "-" + script.String()
	}
	if region, confidence := parsed.Region(); confidence == language.Exact {
		normalized += "-" + region.String()
	}
	return normalized
}

// NormalizeLanguages normalizes a reported list, most preferred first: junk
// dropped, each tag once, at most max.
func NormalizeLanguages(tags []string, max int) []string {
	languages := []string{}
	for _, tag := range tags {
		if len(languages) >= max {
			break
		}
		if normalized := NormalizeLanguageTag(tag); normalized != "" && !slices.Contains(languages, normalized) {
			languages = append(languages, normalized)
		}
	}
	return languages
}

// AcceptLanguages is the whole list of Accept-Language headers, by q, most
// preferred first ("ca-ES,es;q=0.9" is [ca-ES es], so a Catalan reader gets
// the Spanish template rather than English), normalized; q=0 and "*" are
// dropped. Empty for a request without the header, as from the apps.
func AcceptLanguages(headers []string, max int) []string {
	joined := strings.TrimSpace(strings.Join(headers, ","))
	if joined == "" || 4096 < len(joined) {
		return []string{}
	}
	tags, qs, err := language.ParseAcceptLanguage(joined)
	if err != nil {
		return []string{}
	}
	ordered := []string{}
	for i, tag := range tags {
		if qs[i] <= 0 {
			continue
		}
		ordered = append(ordered, tag.String())
	}
	return NormalizeLanguages(ordered, max)
}

// emailLocaleMatcher matches against one email's template locales.
type emailLocaleMatcher struct {
	locales []string
	matcher language.Matcher
	bases   []string
}

// emailLocaleMatchers caches a matcher per locale set: a few emails share a
// handful of sets, and a matcher costs a few allocations to build.
var emailLocaleMatchers sync.Map

// newEmailLocaleMatcher orders the locales English first, then by
// language, each language's regional locales before the language itself
// (zh-HK before zh, es-419 and es-MX before es): x/text answers zh-TW with
// whichever Chinese comes first.
func newEmailLocaleMatcher(templateLocales []string) *emailLocaleMatcher {
	locales := []string{}
	for _, locale := range templateLocales {
		if NormalizeLanguageTag(locale) != "" && !slices.Contains(locales, locale) {
			locales = append(locales, locale)
		}
	}
	baseOf := func(locale string) string {
		base, _, _ := strings.Cut(locale, "-")
		return strings.ToLower(base)
	}
	slices.SortStableFunc(locales, func(a string, b string) int {
		if (a == TemplateLocaleEnglish) != (b == TemplateLocaleEnglish) {
			if a == TemplateLocaleEnglish {
				return -1
			}
			return 1
		}
		if c := strings.Compare(baseOf(a), baseOf(b)); c != 0 {
			return c
		}
		// within a language, the regional locales first
		if aRegional, bRegional := strings.Contains(a, "-"), strings.Contains(b, "-"); aRegional != bRegional {
			if aRegional {
				return -1
			}
			return 1
		}
		return strings.Compare(a, b)
	})
	key := strings.Join(locales, ",")
	if cached, ok := emailLocaleMatchers.Load(key); ok {
		return cached.(*emailLocaleMatcher)
	}
	tags := []language.Tag{}
	bases := []string{}
	for _, locale := range locales {
		tag := language.Make(locale)
		tags = append(tags, tag)
		base, _ := tag.Base()
		bases = append(bases, base.String())
	}
	m := &emailLocaleMatcher{locales: locales, matcher: language.NewMatcher(tags), bases: bases}
	emailLocaleMatchers.Store(key, m)
	return m
}

// TemplateLocaleEnglish is the locale every email has a template in.
const TemplateLocaleEnglish = "en"

// match is the template locale that serves one language, or "": the locale
// the tag names exactly (pt is pt, not pt-BR, which x/text calls the same),
// else x/text's match at Exact confidence (cmn is zh, arb is ar), or at High
// confidence when it keeps the language, after the language's rewrite (a
// Cantonese reader, yue-HK, reads zh-Hant-HK and so zh-HK).
func (self *emailLocaleMatcher) match(tag string, settings *MailLanguageSettings) string {
	if len(self.locales) == 0 {
		return ""
	}
	normalized := NormalizeLanguageTag(tag)
	if normalized == "" {
		return ""
	}
	for _, locale := range self.locales {
		if strings.EqualFold(locale, normalized) {
			return locale
		}
	}
	desired := language.Make(normalized)
	base, _ := desired.Base()
	if target, ok := settings.LanguageRewrites[base.String()]; ok {
		script, _ := desired.Script()
		region, _ := desired.Region()
		targetBase, err := language.ParseBase(target)
		if err != nil {
			return ""
		}
		rewritten, err := language.Compose(targetBase, script, region)
		if err != nil {
			return ""
		}
		desired = rewritten
		base = targetBase
	}
	_, index, confidence := self.matcher.Match(desired)
	if confidence < language.High || index < 0 || len(self.locales) <= index {
		return ""
	}
	if confidence != language.Exact && self.bases[index] != base.String() {
		return ""
	}
	return self.locales[index]
}

// LanguageCandidate is one of a reader's languages with the source it came
// from (the controller's context source names).
type LanguageCandidate struct {
	Tag    string
	Source string
}

// MailLanguages is what a send's language is chosen from: the reader's
// languages, most trusted first, and the country to fall back to.
type MailLanguages struct {
	Candidates []LanguageCandidate
	// alpha-2, the store or billing country before the sign-up IP's; "" when
	// unknown
	Country       string
	CountrySource string
}

// ChooseTemplateLocale is the locale a send to the reader is written in, of
// the email's template locales, and the source that decided it: the first
// candidate a locale serves (MatchTemplateLocale), else the country's
// language (CountryLanguage, whose source is the country's), else English
// with no source. "" when the email has no English template either.
func ChooseTemplateLocale(languages MailLanguages, templateLocales []string, settings *MailLanguageSettings) (locale string, source string) {
	if locale, i := MatchTemplateLocale(languages.Candidates, templateLocales, settings); locale != "" {
		return locale, languages.Candidates[i].Source
	}
	if locale, ok := CountryLanguage(languages.Country, templateLocales, settings); ok {
		return locale, languages.CountrySource
	}
	if slices.Contains(templateLocales, TemplateLocaleEnglish) {
		return TemplateLocaleEnglish, ""
	}
	return "", ""
}

// MatchTemplateLocale is the template locale of the first candidate one of
// the email's locales serves, with that candidate's index; "" and -1 when
// none does.
func MatchTemplateLocale(candidates []LanguageCandidate, templateLocales []string, settings *MailLanguageSettings) (locale string, index int) {
	m := newEmailLocaleMatcher(templateLocales)
	for i, candidate := range candidates {
		if locale := m.match(candidate.Tag, settings); locale != "" {
			return locale, i
		}
	}
	return "", -1
}

// ServesAnyLanguage reports whether any of the languages has one of the
// email locales: a list that does not (Catalan alone) needs the reader's
// country.
func ServesAnyLanguage(languages []string, settings *MailLanguageSettings) bool {
	m := newEmailLocaleMatcher(EmailLocales)
	for _, tag := range languages {
		if m.match(tag, settings) != "" {
			return true
		}
	}
	return false
}

// EmailLocales are the locales the campaign templates ship in
// (localizations keys/email_*.yaml).
var EmailLocales = []string{
	"en", "ar", "cs", "de", "el", "es", "es-419", "es-MX", "fr", "he", "hi", "id", "it", "ja",
	"ko", "nl", "pl", "pt", "pt-BR", "pt-PT", "ru", "sv", "sw", "th", "uk", "vi", "zh", "zh-HK",
}

// CountryLanguage is the template locale for a reader known only by
// country: the owner's override for the country, else the country's top
// language when all of these hold, else nothing (the caller sends English):
//   - one of the email's locales serves it;
//   - at least CountryLanguageMinPercent of the people use it;
//   - no other official language but English is used by more than
//     CountryOtherLanguageMaxPercent, unless the same template serves it
//     (Min Nan in Taiwan, which reads Traditional Chinese).
//
// The language is read in the country (pt in BR is pt-BR, es in AR is
// es-419, zh-Hant in TW is zh-HK). country is alpha-2, in any case.
func CountryLanguage(country string, templateLocales []string, settings *MailLanguageSettings) (locale string, ok bool) {
	country = strings.ToUpper(strings.TrimSpace(country))
	if country == "" {
		return "", false
	}
	m := newEmailLocaleMatcher(templateLocales)
	inCountry := func(tag string) string {
		normalized := NormalizeLanguageTag(tag)
		if normalized == "" {
			return ""
		}
		parsed := language.Make(normalized)
		if _, confidence := parsed.Region(); confidence == language.Exact {
			return m.match(normalized, settings)
		}
		return m.match(normalized+"-"+country, settings)
	}
	if override, ok := settings.CountryOverrides[country]; ok {
		if locale := inCountry(override); locale != "" {
			return locale, true
		}
		return "", false
	}
	shares := countryCodeLanguageShares[country]
	if len(shares) == 0 || shares[0].percent < settings.CountryLanguageMinPercent {
		return "", false
	}
	top := inCountry(shares[0].language)
	if top == "" {
		return "", false
	}
	for _, share := range shares[1:] {
		if share.percent <= settings.CountryOtherLanguageMaxPercent {
			break
		}
		if !share.official || share.language == "en" || strings.HasPrefix(share.language, "en-") {
			continue
		}
		if inCountry(share.language) != top {
			return "", false
		}
	}
	return top, true
}
