package controller

import (
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The decision rule on its own, with no database and no mmdb file: a probe may
// CORRECT a location but must never COARSEN it. The end-to-end behaviour is
// covered by the TestSetConnectionLocation* tests; this pins the rule itself so
// each clause is exercised independently of whichever mmdb the environment has.
//
// Every row here fails against at least one plausible wrong rule -- "the probe
// always wins" (the bug), or "keep whichever answer is finer" (the overcorrection
// that would throw away country correction).
func TestProbedLocationPreferred(t *testing.T) {
	probedCity := func(countryCode string) *model.ProviderEgressLocation {
		return &model.ProviderEgressLocation{
			LocationId:    server.NewId(),
			CountryCode:   countryCode,
			CityConfident: true,
		}
	}
	probedCountry := func(countryCode string) *model.ProviderEgressLocation {
		return &model.ProviderEgressLocation{
			LocationId:    server.NewId(),
			CountryCode:   countryCode,
			CityConfident: false,
		}
	}
	mmdb := func(locationType string, countryCode string) *model.Location {
		return &model.Location{
			LocationId:   server.NewId(),
			LocationType: locationType,
			CountryCode:  countryCode,
		}
	}

	tests := []struct {
		name   string
		egress *model.ProviderEgressLocation
		mmdb   *model.Location
		want   bool
	}{
		// the regression: a country-only probe must not replace an mmdb city
		// in the same country. This is the row that fails against "the probe
		// always wins".
		{"country probe vs mmdb city, same country", probedCountry("ca"), mmdb(model.LocationTypeCity, "ca"), false},
		{"country probe vs mmdb region, same country", probedCountry("ca"), mmdb(model.LocationTypeRegion, "ca"), false},
		// casing must not read as a country disagreement
		{"country probe vs mmdb city, same country cased", probedCountry("CA"), mmdb(model.LocationTypeCity, "ca"), false},

		// country correction: these fail against "keep whichever is finer"
		{"country probe vs mmdb city, other country", probedCountry("jp"), mmdb(model.LocationTypeCity, "ca"), true},
		{"country probe vs mmdb region, other country", probedCountry("jp"), mmdb(model.LocationTypeRegion, "ca"), true},

		// nothing to lose
		{"country probe vs mmdb country, same", probedCountry("ca"), mmdb(model.LocationTypeCountry, "ca"), true},
		{"country probe vs mmdb country, other", probedCountry("jp"), mmdb(model.LocationTypeCountry, "ca"), true},
		{"country probe vs no mmdb answer", probedCountry("jp"), nil, true},

		// a city-confident probe is never a downgrade
		{"city probe vs mmdb city, same country", probedCity("ca"), mmdb(model.LocationTypeCity, "ca"), true},
		{"city probe vs mmdb city, other country", probedCity("jp"), mmdb(model.LocationTypeCity, "ca"), true},
		{"city probe vs mmdb region", probedCity("ca"), mmdb(model.LocationTypeRegion, "ca"), true},
		{"city probe vs no mmdb answer", probedCity("jp"), nil, true},
	}
	for _, test := range tests {
		if got := probedLocationPreferred(test.egress, test.mmdb); got != test.want {
			t.Errorf("%s: probedLocationPreferred = %v, want %v", test.name, got, test.want)
		}
	}
}
