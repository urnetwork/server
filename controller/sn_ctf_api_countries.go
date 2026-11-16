package controller

// sn_ctf_api_countries.go — the 249 countries of capture the flag as the API
// names them (sn CAPTURETHEFLAG.md §4.1, design/ctf/design.html): L, package
// ctf's list of the ISO 3166-1 alpha-2 codes of model/iso_country_names.go in
// code order, with the English name, the name a sentence uses, and whether the
// territory has a permanent population.

import (
	"regexp"
	"strings"
	"sync"

	"github.com/urfoundation/sn/ctf"
)

// The number of countries in L.
const snCtfCountryCount = 249

// snCtfCountry is one country of L.
type snCtfCountry struct {
	Code string
	Name string
	// the name as an English sentence uses it, with "the" where it takes one
	SentenceName string
	// false for the seven territories without a permanent population
	Inhabited bool
}

// The territories without a permanent population. They keep their slots and
// pots, and /sn/ctf/top leaves them out unless asked.
var snCtfUninhabited = map[string]bool{"aq": true, "bv": true, "gs": true, "hm": true, "io": true, "tf": true, "um": true}

// The names a sentence gives "the" (design.html THE), and those a sentence
// says otherwise (design.html SAY).
var (
	snCtfSentenceThe = map[string]bool{
		"us": true, "gb": true, "nl": true, "ph": true, "ae": true, "bs": true, "cf": true, "do": true, "gm": true, "ky": true, "mh": true,
		"mv": true, "sb": true, "sc": true, "tc": true, "cc": true, "ck": true, "fk": true, "mp": true, "km": true, "fo": true,
	}
	snCtfSentenceSay = map[string]string{
		"cd": "the DR Congo", "vg": "the British Virgin Islands", "vi": "the U.S. Virgin Islands", "um": "the U.S. Outlying Islands",
		"io": "the British Indian Ocean Territory", "bq": "the Caribbean Netherlands", "fm": "Micronesia", "ps": "Palestine",
		"va": "Vatican City", "sh": "Saint Helena", "gs": "South Georgia", "hm": "Heard Island", "ru": "Russia", "bn": "Brunei",
	}
)

var snCtfTrailingParenthesis = regexp.MustCompile(` \(.*\)$`)

// snCtfSentenceName is how a sentence names a country: the short form of its
// name (before any comma, without a trailing parenthesis), with "the" where
// it takes one, or a form of its own.
func snCtfSentenceName(code string, name string) string {
	if say, ok := snCtfSentenceSay[code]; ok {
		return say
	}
	short, _, _ := strings.Cut(name, ",")
	short = snCtfTrailingParenthesis.ReplaceAllString(short, "")
	if snCtfSentenceThe[code] {
		return "the " + short
	}
	return short
}

// snCtfCountries is L in code order, as package ctf holds it.
var snCtfCountries = sync.OnceValue(func() []*snCtfCountry {
	countries := []*snCtfCountry{}
	for _, code := range ctf.Countries() {
		country := &snCtfCountry{Code: string(code[:]), Name: code.Name()}
		country.SentenceName = snCtfSentenceName(country.Code, country.Name)
		country.Inhabited = !snCtfUninhabited[country.Code]
		countries = append(countries, country)
	}
	return countries
})

// snCtfCountryIndex is a code's position in L, by lowercase code.
var snCtfCountryIndex = sync.OnceValue(func() map[string]int {
	index := map[string]int{}
	for i, country := range snCtfCountries() {
		index[country.Code] = i
	}
	return index
})

// snCtfParseCountry reads a code in either case: false when it is not one of
// L.
func snCtfParseCountry(code string) (*snCtfCountry, bool) {
	i, ok := snCtfCountryIndex()[strings.ToLower(code)]
	if !ok {
		return nil, false
	}
	return snCtfCountries()[i], true
}

// snCtfCountriesVersion names the countries object, L's codes concatenated (498
// ASCII bytes), as census and ledger headers name it.
func snCtfCountriesVersion() string {
	return ctf.CountriesRef.String()
}
