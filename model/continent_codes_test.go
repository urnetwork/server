package model

import (
	"slices"
	"testing"

	"github.com/urnetwork/connect"
)

// The country to continent table behind the geo dns sets (connect/
// EXTENDER.md C5).

// A country that is not on a continent this operator can publish to is worse
// than useless: it would put an extender in a set Route 53 never serves. A
// handful of known countries per continent pin the shape of the table, and the
// two tests below pin that it is complete and that it says nothing else.
func TestContinentCodeForCountry(t *testing.T) {
	cases := []struct {
		countryCode   string
		continentCode string
	}{
		{countryCode: "NG", continentCode: "AF"},
		{countryCode: "ZA", continentCode: "AF"},
		{countryCode: "EG", continentCode: "AF"},
		{countryCode: "AQ", continentCode: "AN"},
		{countryCode: "BV", continentCode: "AN"},
		{countryCode: "JP", continentCode: "AS"},
		{countryCode: "IN", continentCode: "AS"},
		{countryCode: "TR", continentCode: "AS"},
		{countryCode: "DE", continentCode: "EU"},
		{countryCode: "GB", continentCode: "EU"},
		{countryCode: "RU", continentCode: "EU"},
		{countryCode: "US", continentCode: "NA"},
		{countryCode: "MX", continentCode: "NA"},
		{countryCode: "GL", continentCode: "NA"},
		{countryCode: "AU", continentCode: "OC"},
		{countryCode: "NZ", continentCode: "OC"},
		{countryCode: "FJ", continentCode: "OC"},
		{countryCode: "BR", continentCode: "SA"},
		{countryCode: "AR", continentCode: "SA"},
		{countryCode: "CO", continentCode: "SA"},
		// the ip database reports lower case, the phase 2 fixtures upper case
		{countryCode: "us", continentCode: "NA"},
		{countryCode: " de ", continentCode: "EU"},
		// nothing is guessed: an unknown, empty or malformed code has no
		// continent, and its extenders are published in the global pool only
		{countryCode: "", continentCode: ""},
		{countryCode: "ZZ", continentCode: ""},
		{countryCode: "USA", continentCode: ""},
		{countryCode: "5", continentCode: ""},
	}
	for _, c := range cases {
		continentCode := ContinentCodeForCountry(c.countryCode)
		if continentCode != c.continentCode {
			t.Errorf(
				"ContinentCodeForCountry(%q) = %q, want %q",
				c.countryCode,
				continentCode,
				c.continentCode,
			)
		}
	}
}

// Route 53 knows seven continents. A value that is not one of them would be
// rejected by the api for every set it appears in, which is a whole
// continent's addresses lost on a typo.
func TestContinentCodesAreRoute53Continents(t *testing.T) {
	connect.AssertEqual(t, len(ContinentCodes), 7)
	for _, continentCode := range []string{"AF", "AN", "AS", "EU", "NA", "OC", "SA"} {
		if !slices.Contains(ContinentCodes, continentCode) {
			t.Errorf("the continent %s is not published", continentCode)
		}
	}
	for countryCode, continentCode := range countryContinentCodes {
		if !slices.Contains(ContinentCodes, continentCode) {
			t.Errorf("the country %s maps to %q, which is not a continent", countryCode, continentCode)
		}
	}
}

// Every assigned country code has a continent. A missing one is silent -- the
// extender simply never appears in a continent set -- so the table is pinned
// against the generated ISO list rather than trusted.
func TestContinentCodesCoverEveryCountry(t *testing.T) {
	for countryCode := range isoCountryNames {
		if ContinentCodeForCountry(countryCode) == "" {
			t.Errorf("the country %s has no continent", countryCode)
		}
	}
	// and the reverse: a code that is not assigned is a typo, with the one
	// exception of Kosovo, which is user-assigned and is what the ip
	// databases report
	for countryCode := range countryContinentCodes {
		if _, ok := isoCountryNames[countryCode]; !ok && countryCode != "xk" {
			t.Errorf("the country %s is not an assigned code", countryCode)
		}
	}
}
