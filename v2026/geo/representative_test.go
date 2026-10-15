package geo

import (
	"math"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// Tests of the region and country representatives, on a hand-built list and
// on the deployment's place list when the checkout has one.

// A region of three cities along one parallel, a region of two cities placed
// symmetrically, a region of one city, and a country across the antimeridian,
// each to pin one rule of the representative.
const representativePlaces = `
version: 1
source: test
build_epoch: 1
countries:
  ga: {name: Gabon, geoname_id: 2400553, continent_code: af, continent: Africa}
  fj: {name: Fiji, geoname_id: 2205218, continent_code: oc, continent: Oceania}
places:
  ga:
    Estuaire:
      West: {geoname_id: 201, region_geoname_id: 21, latitude: 0.5, longitude: 10.0, spread_km: 0, time_zone: Africa/Libreville}
      Middle: {geoname_id: 202, region_geoname_id: 21, latitude: 0.5, longitude: 11.0, spread_km: 0, time_zone: Africa/Libreville}
      East: {geoname_id: 203, region_geoname_id: 21, latitude: 0.5, longitude: 12.0, spread_km: 0, time_zone: Africa/Libreville}
    Ogooue:
      Upper: {geoname_id: 205, region_geoname_id: 22, latitude: -1.0, longitude: 13.0, spread_km: 0, time_zone: Africa/Libreville}
      Lower: {geoname_id: 204, region_geoname_id: 22, latitude: -3.0, longitude: 13.0, spread_km: 0, time_zone: Africa/Libreville}
    Gabon:
      Alone: {geoname_id: 206, latitude: 2.0, longitude: 9.5, spread_km: 0, time_zone: Africa/Libreville}
  fj:
    Northern:
      Westedge: {geoname_id: 207, region_geoname_id: 23, latitude: -16.5, longitude: 179.8, spread_km: 0, time_zone: Pacific/Fiji}
      Eastedge: {geoname_id: 208, region_geoname_id: 23, latitude: -16.5, longitude: -179.6, spread_km: 0, time_zone: Pacific/Fiji}
      Centre: {geoname_id: 209, region_geoname_id: 23, latitude: -16.5, longitude: 179.95, spread_km: 0, time_zone: Pacific/Fiji}
`

// A representative is the member city nearest the mean, by id or name, with
// the RMS spread of its members, across the antimeridian too.
func TestRepresentatives(t *testing.T) {
	places, err := LoadPlaces([]byte(representativePlaces))
	if err != nil {
		t.Fatal(err)
	}
	representatives := NewRepresentatives(places)

	assertRepresentative := func(name string, representative Representative, ok bool, wantCity string, wantSpreadKm float64) {
		t.Helper()
		if !ok {
			t.Fatalf("%s: no representative", name)
		}
		if representative.Place.City != wantCity {
			t.Fatalf("%s: represented by %s, want %s", name, representative.Place.City, wantCity)
		}
		if 1e-6 < math.Abs(representative.SpreadKm-wantSpreadKm) {
			t.Fatalf("%s: spread %.6f km, want %.6f km", name, representative.SpreadKm, wantSpreadKm)
		}
	}

	// three cities a degree apart: the middle one, and the RMS of 0 and two
	// one-degree distances
	degreeKm := DistanceKm(0.5, 10, 0.5, 11)
	estuaire, ok := representatives.Region("ga", 21, "Estuaire")
	assertRepresentative("Estuaire by id", estuaire, ok, "Middle", math.Sqrt(2*degreeKm*degreeKm/3))
	// the same region by its name, in a row that has no id, and in either case
	// of the country code
	byName, ok := representatives.Region("GA", 0, "Estuaire")
	assertRepresentative("Estuaire by name", byName, ok, "Middle", estuaire.SpreadKm)
	// an id the list does not know falls back to the name
	byStaleId, ok := representatives.Region("ga", 99, "Estuaire")
	assertRepresentative("Estuaire by a stale id", byStaleId, ok, "Middle", estuaire.SpreadKm)
	// an id of another country's region is not this country's region
	if _, ok := representatives.Region("fj", 21, "Nowhere"); ok {
		t.Fatal("a region id resolved across countries")
	}

	// two cities are equally near their mean, up to rounding, so either may
	// stand for the region; the spread is measured from the one chosen, the
	// RMS of 0 and the whole distance, which is the same either way
	pairKm := DistanceKm(-1, 13, -3, 13)
	ogooue, ok := representatives.Region("ga", 22, "Ogooue")
	if !ok || (ogooue.Place.City != "Upper" && ogooue.Place.City != "Lower") {
		t.Fatalf("Ogooue: %+v, %v", ogooue, ok)
	}
	assertRepresentative("Ogooue", ogooue, ok, ogooue.Place.City, math.Sqrt(pairKm*pairKm/2))
	if again := NewRepresentatives(places); func() bool {
		representative, _ := again.Region("ga", 22, "Ogooue")
		return representative.Place != ogooue.Place
	}() {
		t.Fatal("the representative of a tied region depends on the run")
	}

	// a region named for its country, with one city and no id
	alone, ok := representatives.Region("ga", 0, "Gabon")
	assertRepresentative("the region named for the country", alone, ok, "Alone", 0)

	// the mean of cities on both sides of the antimeridian is near it, not on
	// the far side of the earth
	northern, ok := representatives.Region("fj", 23, "Northern")
	wantSpreadKm := math.Sqrt((math.Pow(DistanceKm(-16.5, 179.95, -16.5, 179.8), 2) + math.Pow(DistanceKm(-16.5, 179.95, -16.5, -179.6), 2)) / 3)
	assertRepresentative("across the antimeridian", northern, ok, "Centre", wantSpreadKm)

	// a country is represented by one of its own cities
	gabon, ok := representatives.Country("GA")
	if !ok || gabon.Place.CountryCode != "ga" {
		t.Fatalf("Gabon: %+v, %v", gabon, ok)
	}
	if !(estuaire.SpreadKm < gabon.SpreadKm) {
		t.Fatalf("Gabon's spread %.3f km is not wider than one of its regions' (%.3f km)", gabon.SpreadKm, estuaire.SpreadKm)
	}
	if _, ok := representatives.Country("zz"); ok {
		t.Fatal("an unknown country has a representative")
	}
}

// On the deployment's place list, when this checkout has one: every
// representative is a city of its own country (and region), and a large
// country is far wider than a city's accuracy radius, which is why a genesis
// placed only in its country cannot be anchored at a city's width.
func TestRepresentativesOfThePlaceList(t *testing.T) {
	paths, _ := filepath.Glob(filepath.Join("..", "..", "config", "all", "mmdb", "*", "places.yml"))
	if len(paths) == 0 {
		t.Skip("no places.yml in this checkout")
	}
	slices.Sort(paths)
	placesBytes, err := os.ReadFile(paths[len(paths)-1])
	if err != nil {
		t.Fatal(err)
	}
	places, err := LoadPlaces(placesBytes)
	if err != nil {
		t.Fatal(err)
	}
	representatives := NewRepresentatives(places)
	for _, country := range places.Countries() {
		representative, ok := representatives.Country(country.Code)
		if !ok {
			// a country of the list with no city
			continue
		}
		if representative.Place.CountryCode != country.Code {
			t.Fatalf("%s is represented by %s, %s", country.Code, representative.Place.City, representative.Place.CountryCode)
		}
	}
	for place := range places.Cities() {
		representative, ok := representatives.Region(place.CountryCode, place.RegionGeonameId, place.Region)
		if !ok || representative.Place.CountryCode != place.CountryCode || representative.Place.Region != place.Region {
			t.Fatalf("%s, %s, %s: region represented by %+v, %v", place.City, place.Region, place.CountryCode, representative.Place, ok)
		}
	}
	for _, countryCode := range []string{"us", "de", "sg"} {
		if representative, ok := representatives.Country(countryCode); ok {
			t.Logf("%s: represented by %s, %s; spread %.0f km", countryCode, representative.Place.City, representative.Place.Region, representative.SpreadKm)
		}
	}
	if us, ok := representatives.Country("us"); ok && !(500 < us.SpreadKm) {
		t.Fatalf("the United States spread only %.0f km", us.SpreadKm)
	}
}
