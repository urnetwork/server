package geo

import (
	"strings"
	"testing"
)

// Tests of the place name index: its lookups by name, normalized name and id,
// and the region named for its country.

// A list with twin city names, names that normalize alike, and regions named
// for their countries.
const namesPlaces = `
version: 1
source: test
build_epoch: 1
countries:
  gb: {name: United Kingdom, geoname_id: 2635167, continent_code: eu, continent: Europe}
  gt: {name: Guatemala, geoname_id: 3595528, continent_code: na, continent: North America}
  sg: {name: Singapore, geoname_id: 1880251, continent_code: as, continent: Asia}
places:
  gb:
    England:
      Forest Hill: {geoname_id: 2649216, region_geoname_id: 6269131, latitude: 51.4504, longitude: -0.0367}
      Forest Hill (geoname 11593192): {geoname_id: 11593192, region_geoname_id: 6269131, latitude: 51.7561, longitude: -1.1475}
      Saint-Denis: {geoname_id: 900001, region_geoname_id: 6269131, latitude: 51.0, longitude: 0.0}
      SAINT DENIS: {geoname_id: 900002, region_geoname_id: 6269131, latitude: 51.1, longitude: 0.1}
    Scotland:
      Forest Hill: {geoname_id: 900003, region_geoname_id: 2638360, latitude: 56.0, longitude: -3.0}
  gt:
    Guatemala:
      Guatemala City: {geoname_id: 3598132, region_geoname_id: 3595530, latitude: 14.6407, longitude: -90.5133}
      Villa Nueva: {geoname_id: 3587902, latitude: 14.5269, longitude: -90.5875}
  sg:
    Singapore:
      Bedok New Town: {geoname_id: 1884382, latitude: 1.3264, longitude: 103.9394}
`

// The name index of namesPlaces.
func loadNames(t *testing.T) *PlaceNames {
	t.Helper()
	places, err := LoadPlaces([]byte(namesPlaces))
	if err != nil {
		t.Fatal(err)
	}
	// a stand-in normalization: the index applies whatever it is given
	normalize := func(name string) string {
		return strings.Join(strings.FieldsFunc(strings.ToLower(name), func(r rune) bool {
			return r == ' ' || r == '-' || r == '(' || r == ')'
		}), " ")
	}
	return NewPlaceNames(places, normalize)
}

// The geoname ids of named places, in their order.
func cityGeonameIds(namedPlaces []*NamedPlace) []uint32 {
	geonameIds := []uint32{}
	for _, namedPlace := range namedPlaces {
		geonameIds = append(geonameIds, namedPlace.Place.GeonameId)
	}
	return geonameIds
}

// Regions and cities are found by exact name, normalized name and id, in each
// scope, and listed in the list's order.
func TestPlaceNamesIndexesRegionsAndCities(t *testing.T) {
	placeNames := loadNames(t)

	gb := placeNames.Country("GB")
	if gb == nil || gb.Code != "gb" || len(gb.Regions) != 2 || len(gb.Cities) != 5 {
		t.Fatalf("gb = %+v", gb)
	}
	if placeNames.Country("fr") != nil {
		t.Fatal("a country with no city has no names")
	}

	england := gb.RegionByGeonameId(6269131)
	if england == nil || england.Name != "England" || england.Normalized != "england" || len(england.Cities) != 4 {
		t.Fatalf("England = %+v", england)
	}
	if gb.Region("England") != england || gb.RegionByGeonameId(0) != nil || gb.RegionByGeonameId(1) != nil {
		t.Fatal("region lookups disagree")
	}
	if regions := gb.RegionsNamed("england"); len(regions) != 1 || regions[0] != england {
		t.Fatalf("RegionsNamed(england) = %v", regions)
	}

	// the normalized name of a place, the suffixed twin keeping its suffix
	if got := cityGeonameIds(england.CitiesNamed("forest hill")); len(got) != 1 || got[0] != 2649216 {
		t.Fatalf("England CitiesNamed(forest hill) = %v", got)
	}
	if got := cityGeonameIds(england.CitiesNamed("forest hill geoname 11593192")); len(got) != 1 || got[0] != 11593192 {
		t.Fatalf("England CitiesNamed(suffixed) = %v", got)
	}
	// two places whose names normalize alike are both returned
	if got := cityGeonameIds(england.CitiesNamed("saint denis")); len(got) != 2 {
		t.Fatalf("England CitiesNamed(saint denis) = %v", got)
	}
	// the country scope spans its regions
	if got := cityGeonameIds(gb.CitiesNamed("forest hill")); len(got) != 2 {
		t.Fatalf("gb CitiesNamed(forest hill) = %v", got)
	}
	if got := england.CitiesNamed("nowhere"); len(got) != 0 {
		t.Fatalf("CitiesNamed(nowhere) = %v", got)
	}

	// in the list's order
	cityNames := []string{}
	for _, namedPlace := range england.Cities {
		cityNames = append(cityNames, namedPlace.Place.City)
	}
	if got, want := strings.Join(cityNames, "|"), "Forest Hill|Forest Hill (geoname 11593192)|SAINT DENIS|Saint-Denis"; got != want {
		t.Fatalf("England order = %s, want %s", got, want)
	}
}

// The region named for its country has no id, and a real subdivision of the
// same name shares it and lends it its id.
func TestPlaceNamesRegionNamedForTheCountry(t *testing.T) {
	placeNames := loadNames(t)

	// GeoLite2 files Singapore's cities under no subdivision: the region named
	// for the country has no id
	singapore := placeNames.Country("sg").Region("Singapore")
	if singapore == nil || singapore.GeonameId != 0 || len(singapore.Cities) != 1 {
		t.Fatalf("Singapore = %+v", singapore)
	}

	// a real subdivision named like its country shares that region, which
	// takes the subdivision's id, as the rows share one region row
	guatemala := placeNames.Country("gt").Region("Guatemala")
	if guatemala == nil || guatemala.GeonameId != 3595530 || len(guatemala.Cities) != 2 {
		t.Fatalf("Guatemala = %+v", guatemala)
	}
	if placeNames.Country("gt").RegionByGeonameId(3595530) != guatemala {
		t.Fatal("the shared region is not found by the subdivision's id")
	}
}
