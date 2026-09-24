package model

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// Tests of the location seeder and of CreateLocation against the place list:
// seeding, refreshing and backfilling by geoname id, and the adoption of
// stored rows that resolve to the place a lookup names.

// A small place list in the shape server/cli/geolite2export writes
// (connect/GEOMAP.md §4.1). It has a city under a subdivision, a city GeoLite2
// files under no subdivision (sg), two cities of one name in one region (the
// later id suffixed, as the export does), a country GeoLite2 names differently
// from the ISO table (nl), and one the ISO table does not have (xk).
const testPlacesYaml = `
version: 1
source: test
build_epoch: 1790110762
countries:
  gb: {name: United Kingdom, geoname_id: 2635167, continent_code: eu, continent: Europe}
  nl: {name: The Netherlands, geoname_id: 2750405, continent_code: eu, continent: Europe}
  sg: {name: Singapore, geoname_id: 1880251, continent_code: as, continent: Asia}
  us: {name: United States, geoname_id: 6252001, continent_code: na, continent: North America}
  xk: {name: Kosovo, geoname_id: 831053, continent_code: eu, continent: Europe}
places:
  gb:
    England:
      Dorking: {geoname_id: 2651095, region_geoname_id: 6269131, latitude: 51.2344, longitude: -0.3336, spread_km: 9.9, time_zone: Europe/London}
      East Finchley: {geoname_id: 2650444, region_geoname_id: 6269131, latitude: 51.5967, longitude: -0.1593, spread_km: 0, time_zone: Europe/London}
      Forest Hill: {geoname_id: 2649216, region_geoname_id: 6269131, latitude: 51.4504, longitude: -0.0367, spread_km: 0, time_zone: Europe/London}
      Forest Hill (geoname 11593192): {geoname_id: 11593192, region_geoname_id: 6269131, latitude: 51.7561, longitude: -1.1475, spread_km: 0, time_zone: Europe/London}
  nl:
    North Holland:
      Amsterdam: {geoname_id: 2759794, region_geoname_id: 2749879, latitude: 52.3759, longitude: 4.8975, spread_km: 12.1, time_zone: Europe/Amsterdam}
  sg:
    Singapore:
      Bedok New Town: {geoname_id: 1884382, latitude: 1.3264, longitude: 103.9394, spread_km: 0, time_zone: Asia/Singapore}
  us:
    California:
      Palo Alto: {geoname_id: 5380748, region_geoname_id: 5332921, latitude: 37.4419, longitude: -122.143, spread_km: 3.2, time_zone: America/Los_Angeles}
`

// the cities of testPlacesYaml, in the list's order
var testPlacesCityGeonameIds = []uint32{2651095, 2650444, 2649216, 11593192, 2759794, 1884382, 5380748}

// Stands a place list in for the deployment's. The matcher's list is reset on
// both sides, so the test resolves against the pushed list and the tests
// after it against their own.
func pushTestPlaces(placesYaml string) func() {
	pop := server.Config.PushSimpleResource(placesResource, []byte(placesYaml))
	resetLocationPlaceNames()
	return func() {
		pop()
		resetLocationPlaceNames()
	}
}

// The number of location rows a condition matches.
func countLocations(ctx context.Context, where string, args ...any) int {
	var count int
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `SELECT COUNT(*) FROM location WHERE `+where, args...)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&count))
			}
		})
	})
	return count
}

// A row's geoname_id column, read directly; 0 for NULL.
func storedGeonameId(ctx context.Context, locationId server.Id) uint32 {
	var geonameId *int64
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `SELECT geoname_id FROM location WHERE location_id = $1`, locationId)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&geonameId))
			}
		})
	})
	return geonameIdValue(geonameId)
}

// The location with a geoname id, failing the test when there is none.
func requireLocationByGeonameId(t testing.TB, ctx context.Context, geonameId uint32) *Location {
	location := GetLocationByGeonameId(ctx, geonameId)
	if location == nil {
		t.Fatalf("no location has geoname id %d", geonameId)
	}
	return location
}

// The seeder stores every country, region and city of the list, keyed by id
// and named as the list names it.
func TestAddDefaultLocationsSeedsThePlaceList(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer pushTestPlaces(testPlacesYaml)()

		AddDefaultLocations(ctx, -1)

		// every country, keyed by id and named as the list names it
		for _, country := range []struct {
			code      string
			name      string
			geonameId uint32
		}{
			{code: "gb", name: "United Kingdom", geonameId: 2635167},
			// not the ISO table's "Netherlands"
			{code: "nl", name: "The Netherlands", geonameId: 2750405},
			{code: "sg", name: "Singapore", geonameId: 1880251},
			{code: "us", name: "United States", geonameId: 6252001},
			// not in the ISO table at all
			{code: "xk", name: "Kosovo", geonameId: 831053},
		} {
			location := requireLocationByGeonameId(t, ctx, country.geonameId)
			connect.AssertEqual(t, location.LocationType, LocationTypeCountry)
			connect.AssertEqual(t, location.CountryCode, country.code)
			connect.AssertEqual(t, location.Country, country.name)
			connect.AssertEqual(t, location.CountryGeonameId, country.geonameId)
			connect.AssertEqual(t, location.CountryLocationId, location.LocationId)
		}

		// a city, with its coordinate and its region's and country's ids
		city := requireLocationByGeonameId(t, ctx, 2650444)
		connect.AssertEqual(t, city.LocationType, LocationTypeCity)
		connect.AssertEqual(t, city.City, "East Finchley")
		connect.AssertEqual(t, city.Region, "England")
		connect.AssertEqual(t, city.Country, "United Kingdom")
		connect.AssertEqual(t, city.CountryCode, "gb")
		connect.AssertEqual(t, city.Latitude, 51.5967)
		connect.AssertEqual(t, city.Longitude, -0.1593)
		connect.AssertEqual(t, city.CityGeonameId, uint32(2650444))
		connect.AssertEqual(t, city.RegionGeonameId, uint32(6269131))
		connect.AssertEqual(t, city.CountryGeonameId, uint32(2635167))
		connect.AssertEqual(t, city.CityLocationId, city.LocationId)

		// the region row, keyed by the subdivision's id
		region := requireLocationByGeonameId(t, ctx, 6269131)
		connect.AssertEqual(t, region.LocationType, LocationTypeRegion)
		connect.AssertEqual(t, region.Region, "England")
		connect.AssertEqual(t, region.LocationId, city.RegionLocationId)
		connect.AssertEqual(t, region.CountryLocationId, city.CountryLocationId)

		// a city under no subdivision is filed under the region named for its
		// country, which has no id of its own
		bedok := requireLocationByGeonameId(t, ctx, 1884382)
		connect.AssertEqual(t, bedok.City, "Bedok New Town")
		connect.AssertEqual(t, bedok.Region, "Singapore")
		connect.AssertEqual(t, bedok.RegionGeonameId, uint32(0))
		connect.AssertEqual(t, locationName(ctx, t, bedok.RegionLocationId), "Singapore")

		// two cities of one name in one region are two rows
		forestHill := requireLocationByGeonameId(t, ctx, 2649216)
		forestHillOxford := requireLocationByGeonameId(t, ctx, 11593192)
		connect.AssertEqual(t, forestHill.City, "Forest Hill")
		connect.AssertEqual(t, forestHillOxford.City, "Forest Hill (geoname 11593192)")
		connect.AssertNotEqual(t, forestHill.LocationId, forestHillOxford.LocationId)
		connect.AssertEqual(t, forestHill.RegionLocationId, forestHillOxford.RegionLocationId)

		for _, geonameId := range testPlacesCityGeonameIds {
			connect.AssertEqual(t, requireLocationByGeonameId(t, ctx, geonameId).LocationType, LocationTypeCity)
		}
		connect.AssertEqual(t, countLocations(ctx, `location_type = $1 AND geoname_id IS NOT NULL`, LocationTypeCity), len(testPlacesCityGeonameIds))
		connect.AssertEqual(t, blankNamedLocationCount(ctx, t), 0)
	})
}

// A second seed of the same list changes no row.
func TestAddDefaultLocationsIsIdempotent(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer pushTestPlaces(testPlacesYaml)()

		AddDefaultLocations(ctx, -1)
		locationCount := countLocations(ctx, `true`)
		geonameIdLocationIds := map[uint32]server.Id{}
		for _, geonameId := range testPlacesCityGeonameIds {
			geonameIdLocationIds[geonameId] = requireLocationByGeonameId(t, ctx, geonameId).LocationId
		}

		AddDefaultLocations(ctx, -1)
		connect.AssertEqual(t, countLocations(ctx, `true`), locationCount)
		for _, geonameId := range testPlacesCityGeonameIds {
			connect.AssertEqual(t, requireLocationByGeonameId(t, ctx, geonameId).LocationId, geonameIdLocationIds[geonameId])
		}
		connect.AssertEqual(t, countLocations(ctx, `location_name LIKE '%(geoname %'`), 1)
	})
}

// A city limit seeds every country and only the first cities in the list's
// order.
func TestAddDefaultLocationsCityLimit(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer pushTestPlaces(testPlacesYaml)()

		// 0 seeds every country and no city
		AddDefaultLocations(ctx, 0)
		connect.AssertEqual(t, countLocations(ctx, `location_type = $1`, LocationTypeCity), 0)
		connect.AssertEqual(t, countLocations(ctx, `location_type = $1 AND geoname_id IS NOT NULL`, LocationTypeCountry), 5)

		// a limit takes the first cities in the list's order
		AddDefaultLocations(ctx, 2)
		connect.AssertEqual(t, countLocations(ctx, `location_type = $1`, LocationTypeCity), 2)
		connect.AssertEqual(t, requireLocationByGeonameId(t, ctx, 2651095).City, "Dorking")
		connect.AssertEqual(t, requireLocationByGeonameId(t, ctx, 2650444).City, "East Finchley")
		connect.AssertEqual(t, GetLocationByGeonameId(ctx, 2649216) == nil, true)
	})
}

// A row keyed by geoname id takes the list's name and coordinates on every
// seed; a lookup never moves it.
func TestAddDefaultLocationsRefreshesByGeonameId(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		popPlaces := pushTestPlaces(testPlacesYaml)
		AddDefaultLocations(ctx, -1)
		popPlaces()
		before := requireLocationByGeonameId(t, ctx, 2650444)

		renamed := strings.NewReplacer(
			"East Finchley: {geoname_id: 2650444, region_geoname_id: 6269131, latitude: 51.5967, longitude: -0.1593,",
			"East Finchley Village: {geoname_id: 2650444, region_geoname_id: 6269131, latitude: 51.5901, longitude: -0.1655,",
			"gb: {name: United Kingdom,",
			"gb: {name: Great Britain,",
		).Replace(testPlacesYaml)
		defer pushTestPlaces(renamed)()
		AddDefaultLocations(ctx, -1)

		after := requireLocationByGeonameId(t, ctx, 2650444)
		connect.AssertEqual(t, after.LocationId, before.LocationId)
		connect.AssertEqual(t, after.City, "East Finchley Village")
		connect.AssertEqual(t, after.Latitude, 51.5901)
		connect.AssertEqual(t, after.Longitude, -0.1655)
		connect.AssertEqual(t, after.Country, "Great Britain")
		connect.AssertEqual(t, after.CountryLocationId, before.CountryLocationId)
		// renamed, not duplicated
		connect.AssertEqual(t, countLocations(ctx, `location_name = $1`, "East Finchley"), 0)

		// a lookup of the city carries one network's coordinate and another
		// spelling; it resolves to the row and changes neither
		lookup := &Location{
			LocationType:     LocationTypeCity,
			City:             "East Finchley",
			Region:           "England",
			Country:          "United Kingdom",
			CountryCode:      "gb",
			Latitude:         51.6,
			Longitude:        -0.17,
			CityGeonameId:    2650444,
			RegionGeonameId:  6269131,
			CountryGeonameId: 2635167,
		}
		CreateLocation(ctx, lookup)
		connect.AssertEqual(t, lookup.LocationId, before.LocationId)
		stored := requireLocationByGeonameId(t, ctx, 2650444)
		connect.AssertEqual(t, stored.City, "East Finchley Village")
		connect.AssertEqual(t, stored.Latitude, 51.5901)
		connect.AssertEqual(t, stored.Longitude, -0.1655)
	})
}

// Rows from before geoname ids -- the old city list, the older ip databases --
// are matched by name and get the ids and the list's coordinates.
func TestAddDefaultLocationsBackfillsLegacyRows(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		legacy := &Location{
			LocationType: LocationTypeCity,
			City:         "East Finchley",
			Region:       "England",
			Country:      "United Kingdom",
			CountryCode:  "gb",
		}
		CreateLocation(ctx, legacy)
		connect.AssertEqual(t, storedGeonameId(ctx, legacy.LocationId), uint32(0))

		defer pushTestPlaces(testPlacesYaml)()
		AddDefaultLocations(ctx, -1)

		city := requireLocationByGeonameId(t, ctx, 2650444)
		connect.AssertEqual(t, city.LocationId, legacy.LocationId)
		connect.AssertEqual(t, city.RegionLocationId, legacy.RegionLocationId)
		connect.AssertEqual(t, city.CountryLocationId, legacy.CountryLocationId)
		connect.AssertEqual(t, city.Latitude, 51.5967)
		connect.AssertEqual(t, city.Longitude, -0.1593)
		connect.AssertEqual(t, storedGeonameId(ctx, legacy.RegionLocationId), uint32(6269131))
		connect.AssertEqual(t, storedGeonameId(ctx, legacy.CountryLocationId), uint32(2635167))
		connect.AssertEqual(t, countLocations(ctx, `location_name = $1`, "East Finchley"), 1)
	})
}

// A lookup with a stored geoname id resolves to that row under any names, and
// one without an id is matched by name.
func TestCreateLocationMatchesGeonameIdBeforeName(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer pushTestPlaces(testPlacesYaml)()

		first := &Location{
			LocationType:     LocationTypeCity,
			City:             "Old Town",
			Region:           "Somewhere",
			Country:          "United Kingdom",
			CountryCode:      "gb",
			CityGeonameId:    4200000001,
			RegionGeonameId:  4200000002,
			CountryGeonameId: 2635167,
		}
		CreateLocation(ctx, first)
		connect.AssertEqual(t, first.CityGeonameId, uint32(4200000001))
		connect.AssertEqual(t, first.RegionGeonameId, uint32(4200000002))
		connect.AssertEqual(t, first.CountryGeonameId, uint32(2635167))

		// the same id under other names is the same city, and resolves to its
		// row without creating the region it now names
		renamed := &Location{
			LocationType:     LocationTypeCity,
			City:             "New Town",
			Region:           "Elsewhere",
			Country:          "United Kingdom",
			CountryCode:      "gb",
			CityGeonameId:    4200000001,
			RegionGeonameId:  4200000003,
			CountryGeonameId: 2635167,
		}
		CreateLocation(ctx, renamed)
		connect.AssertEqual(t, renamed.LocationId, first.LocationId)
		connect.AssertEqual(t, renamed.RegionLocationId, first.RegionLocationId)
		connect.AssertEqual(t, renamed.CountryLocationId, first.CountryLocationId)
		// the stored names, which a lookup does not rename
		connect.AssertEqual(t, renamed.City, "Old Town")
		connect.AssertEqual(t, renamed.Region, "Somewhere")
		connect.AssertEqual(t, countLocations(ctx, `location_name = $1`, "New Town"), 0)
		connect.AssertEqual(t, countLocations(ctx, `location_name = $1`, "Elsewhere"), 0)

		// a location without an id is matched by name, as before
		byName := &Location{
			LocationType: LocationTypeCity,
			City:         "Old Town",
			Region:       "Somewhere",
			Country:      "United Kingdom",
			CountryCode:  "gb",
		}
		CreateLocation(ctx, byName)
		connect.AssertEqual(t, byName.LocationId, first.LocationId)
	})
}

// A lookup with ids gives a legacy row of the same names its ids and a missing
// coordinate, and never moves a stored one.
func TestCreateLocationBackfillsLegacyGeonameIds(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer pushTestPlaces(testPlacesYaml)()

		legacy := &Location{
			LocationType: LocationTypeCity,
			City:         "Palo Alto",
			Region:       "California",
			Country:      "United States",
			CountryCode:  "us",
		}
		CreateLocation(ctx, legacy)
		for _, locationId := range []server.Id{legacy.LocationId, legacy.RegionLocationId, legacy.CountryLocationId} {
			connect.AssertEqual(t, storedGeonameId(ctx, locationId), uint32(0))
		}

		lookup := &Location{
			LocationType:     LocationTypeCity,
			City:             "Palo Alto",
			Region:           "California",
			Country:          "United States",
			CountryCode:      "us",
			Latitude:         37.4419,
			Longitude:        -122.143,
			CityGeonameId:    5380748,
			RegionGeonameId:  5332921,
			CountryGeonameId: 6252001,
		}
		CreateLocation(ctx, lookup)
		connect.AssertEqual(t, lookup.LocationId, legacy.LocationId)
		connect.AssertEqual(t, lookup.RegionLocationId, legacy.RegionLocationId)
		connect.AssertEqual(t, lookup.CountryLocationId, legacy.CountryLocationId)
		connect.AssertEqual(t, storedGeonameId(ctx, legacy.LocationId), uint32(5380748))
		connect.AssertEqual(t, storedGeonameId(ctx, legacy.RegionLocationId), uint32(5332921))
		connect.AssertEqual(t, storedGeonameId(ctx, legacy.CountryLocationId), uint32(6252001))

		// the legacy row had no coordinate; the lookup filled it
		city := GetLocation(ctx, legacy.LocationId)
		connect.AssertEqual(t, city.Latitude, 37.4419)
		connect.AssertEqual(t, city.Longitude, -122.143)
		connect.AssertEqual(t, city.CityGeonameId, uint32(5380748))
		connect.AssertEqual(t, city.RegionGeonameId, uint32(5332921))
		connect.AssertEqual(t, city.CountryGeonameId, uint32(6252001))
		connect.AssertEqual(t, GetLocationByGeonameId(ctx, 5380748).LocationId, legacy.LocationId)

		// and a stored coordinate is never overwritten by a lookup
		moved := *lookup
		moved.Latitude = 37.5
		moved.Longitude = -122.2
		CreateLocation(ctx, &moved)
		city = GetLocation(ctx, legacy.LocationId)
		connect.AssertEqual(t, city.Latitude, 37.4419)
		connect.AssertEqual(t, city.Longitude, -122.143)
	})
}

// Two places of one name in one region are told apart by id, whichever
// arrives first.
func TestCreateLocationKeepsSameNamedPlacesApart(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer pushTestPlaces(testPlacesYaml)()

		forestHill := func(geonameId uint32) *Location {
			location := &Location{
				LocationType:     LocationTypeCity,
				City:             "Forest Hill",
				Region:           "England",
				Country:          "United Kingdom",
				CountryCode:      "gb",
				CityGeonameId:    geonameId,
				RegionGeonameId:  6269131,
				CountryGeonameId: 2635167,
			}
			CreateLocation(ctx, location)
			return location
		}

		oxford := forestHill(11593192)
		london := forestHill(2649216)
		connect.AssertNotEqual(t, oxford.LocationId, london.LocationId)
		connect.AssertEqual(t, oxford.RegionLocationId, london.RegionLocationId)
		connect.AssertEqual(t, locationName(ctx, t, oxford.LocationId), "Forest Hill")
		connect.AssertEqual(t, locationName(ctx, t, london.LocationId), "Forest Hill (geoname 2649216)")

		connect.AssertEqual(t, forestHill(11593192).LocationId, oxford.LocationId)
		connect.AssertEqual(t, forestHill(2649216).LocationId, london.LocationId)
		connect.AssertEqual(t, countLocations(ctx, `location_name LIKE 'Forest Hill%'`), 2)

		// a lookup without an id cannot tell them apart and takes the row of
		// the plain name, creating nothing
		byName := &Location{
			LocationType: LocationTypeCity,
			City:         "Forest Hill",
			Region:       "England",
			Country:      "United Kingdom",
			CountryCode:  "gb",
		}
		CreateLocation(ctx, byName)
		connect.AssertEqual(t, byName.LocationId, oxford.LocationId)
		connect.AssertEqual(t, countLocations(ctx, `location_name LIKE 'Forest Hill%'`), 2)
	})
}

// GeoLite2 files some cities under no subdivision. With a geoname id the city
// resolves under the region named for its country, as the seeder files it,
// instead of degrading to the country.
func TestCreateLocationRegionlessGeoLite2City(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer pushTestPlaces(testPlacesYaml)()

		lookup := &Location{
			LocationType:     LocationTypeCity,
			City:             "Bedok New Town",
			Country:          "Singapore",
			CountryCode:      "sg",
			Latitude:         1.3264,
			Longitude:        103.9394,
			CityGeonameId:    1884382,
			CountryGeonameId: 1880251,
		}
		CreateLocation(ctx, lookup)
		connect.AssertEqual(t, lookup.LocationType, LocationTypeCity)
		connect.AssertEqual(t, lookup.City, "Bedok New Town")
		connect.AssertEqual(t, lookup.Region, "Singapore")
		connect.AssertEqual(t, lookup.RegionGeonameId, uint32(0))
		connect.AssertEqual(t, locationName(ctx, t, lookup.RegionLocationId), "Singapore")
		connect.AssertEqual(t, storedGeonameId(ctx, lookup.RegionLocationId), uint32(0))

		// and the seeder files the same city on the same row
		AddDefaultLocations(ctx, -1)
		connect.AssertEqual(t, requireLocationByGeonameId(t, ctx, 1884382).LocationId, lookup.LocationId)
		connect.AssertEqual(t, blankNamedLocationCount(ctx, t), 0)
	})
}

// A code-only location resolves to the seeded country row and its name, even
// for a code the ISO table does not have.
func TestCreateLocationResolvesSeededCountryNames(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		// before the seeder, nothing names xk
		raised := func() (raised bool) {
			defer func() {
				if err := recover(); err != nil {
					raised = true
				}
			}()
			CreateLocation(ctx, &Location{
				LocationType: LocationTypeCountry,
				CountryCode:  "xk",
			})
			return
		}()
		connect.AssertEqual(t, raised, true)
		connect.AssertEqual(t, locationCount(ctx, t, "xk"), 0)

		defer pushTestPlaces(testPlacesYaml)()
		AddDefaultLocations(ctx, 0)

		for countryCode, name := range map[string]string{
			"xk": "Kosovo",
			"nl": "The Netherlands",
		} {
			location := &Location{
				LocationType: LocationTypeCountry,
				CountryCode:  countryCode,
			}
			CreateLocation(ctx, location)
			connect.AssertEqual(t, location.Country, name)
			connect.AssertEqual(t, locationName(ctx, t, location.LocationId), name)
			connect.AssertEqual(t, location.LocationId, requireLocationByGeonameId(t, ctx, location.CountryGeonameId).LocationId)
		}
	})
}

// Every level of a created location is found by its geoname id, and an unknown
// id finds nothing.
func TestGetLocationByGeonameId(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer pushTestPlaces(testPlacesYaml)()

		connect.AssertEqual(t, GetLocationByGeonameId(ctx, 0) == nil, true)
		connect.AssertEqual(t, GetLocationByGeonameId(ctx, 2650444) == nil, true)

		created := &Location{
			LocationType:     LocationTypeCity,
			City:             "East Finchley",
			Region:           "England",
			Country:          "United Kingdom",
			CountryCode:      "gb",
			Latitude:         51.5967,
			Longitude:        -0.1593,
			CityGeonameId:    2650444,
			RegionGeonameId:  6269131,
			CountryGeonameId: 2635167,
		}
		CreateLocation(ctx, created)

		country := requireLocationByGeonameId(t, ctx, 2635167)
		connect.AssertEqual(t, country.LocationType, LocationTypeCountry)
		connect.AssertEqual(t, country.LocationId, created.CountryLocationId)
		connect.AssertEqual(t, country.CityLocationId, server.Id{})
		connect.AssertEqual(t, country.RegionLocationId, server.Id{})
		connect.AssertEqual(t, country.CityGeonameId, uint32(0))
		connect.AssertEqual(t, country.CountryGeonameId, uint32(2635167))

		region := GetLocation(ctx, created.RegionLocationId)
		connect.AssertEqual(t, region.LocationType, LocationTypeRegion)
		connect.AssertEqual(t, region.Region, "England")
		connect.AssertEqual(t, region.Country, "United Kingdom")
		connect.AssertEqual(t, region.CityLocationId, server.Id{})
		connect.AssertEqual(t, region.RegionGeonameId, uint32(6269131))
		connect.AssertEqual(t, region.CountryGeonameId, uint32(2635167))
	})
}

// Real GeoLite2 entries for the lookups below: the stored rows they meet are
// spelled other ways, or are other places.
const looseTestPlacesYaml = `
version: 1
source: test
build_epoch: 1
countries:
  br: {name: Brazil, geoname_id: 3469034, continent_code: sa, continent: South America}
  fr: {name: France, geoname_id: 3017382, continent_code: eu, continent: Europe}
  gb: {name: United Kingdom, geoname_id: 2635167, continent_code: eu, continent: Europe}
  ua: {name: Ukraine, geoname_id: 690791, continent_code: eu, continent: Europe}
  us: {name: United States, geoname_id: 6252001, continent_code: na, continent: North America}
places:
  br:
    São Paulo:
      São Paulo: {geoname_id: 3448439, region_geoname_id: 3448433, latitude: -23.6293, longitude: -46.6351}
  fr:
    Île-de-France:
      Paris: {geoname_id: 2988507, region_geoname_id: 3012874, latitude: 48.8534, longitude: 2.3488}
      Saint-Denis: {geoname_id: 2980916, region_geoname_id: 3012874, latitude: 48.9356, longitude: 2.3539}
  gb:
    England:
      Abridge: {geoname_id: 9072588, region_geoname_id: 6269131, latitude: 51.6473, longitude: 0.1909}
      Cambridge: {geoname_id: 2653941, region_geoname_id: 6269131, latitude: 52.198, longitude: 0.118}
  ua:
    Kyiv City:
      Kyiv: {geoname_id: 703448, region_geoname_id: 703447, latitude: 50.458, longitude: 30.5303}
  us:
    Illinois:
      Springfield: {geoname_id: 4250542, region_geoname_id: 4896861, latitude: 39.8017, longitude: -89.6437}
      Springfeld: {geoname_id: 4200000009, region_geoname_id: 4896861, latitude: 39.8, longitude: -89.6}
    Texas:
      Paris: {geoname_id: 4717560, region_geoname_id: 4736286, latitude: 33.6609, longitude: -95.5555}
`

// A lookup of a place adopts a stored row without an id only when that row
// resolves to the place (location_match.go): spelled another way, or within
// reach of it and of no other place. A row that is another place, however
// alike, is never adopted.
func TestCreateLocationAdoptsRowsThatResolveToThePlace(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer pushTestPlaces(looseTestPlacesYaml)()

		// a stored row spelled without the diacritics anchors to the place
		legacy := &Location{
			LocationType: LocationTypeCity,
			City:         "Sao Paulo",
			Region:       "Sao Paulo",
			Country:      "Brazil",
			CountryCode:  "br",
		}
		CreateLocation(ctx, legacy)
		lookup := &Location{
			LocationType:     LocationTypeCity,
			City:             "São Paulo",
			Region:           "São Paulo",
			Country:          "Brazil",
			CountryCode:      "br",
			Latitude:         -23.5475,
			Longitude:        -46.6361,
			CityGeonameId:    3448439,
			RegionGeonameId:  3448433,
			CountryGeonameId: 3469034,
		}
		CreateLocation(ctx, lookup)
		connect.AssertEqual(t, lookup.LocationId, legacy.LocationId)
		connect.AssertEqual(t, lookup.RegionLocationId, legacy.RegionLocationId)
		connect.AssertEqual(t, storedGeonameId(ctx, legacy.LocationId), uint32(3448439))
		connect.AssertEqual(t, storedGeonameId(ctx, legacy.RegionLocationId), uint32(3448433))
		city := GetLocation(ctx, legacy.LocationId)
		connect.AssertEqual(t, city.Latitude, -23.5475)
		// a lookup adopts the row as it is named; the seeder renames it
		connect.AssertEqual(t, city.City, "Sao Paulo")

		// a stored row named for the place, looked up under another spelling:
		// the row is found by what it resolves to, not by the lookup's name
		saintDenis := &Location{
			LocationType: LocationTypeCity,
			City:         "Saint-Denis",
			Region:       "Ile-de-France",
			Country:      "France",
			CountryCode:  "fr",
		}
		CreateLocation(ctx, saintDenis)
		stDenis := &Location{
			LocationType:     LocationTypeCity,
			City:             "St. Denis",
			Region:           "Île-de-France",
			Country:          "France",
			CountryCode:      "fr",
			CityGeonameId:    2980916,
			RegionGeonameId:  3012874,
			CountryGeonameId: 3017382,
		}
		CreateLocation(ctx, stDenis)
		connect.AssertEqual(t, stDenis.LocationId, saintDenis.LocationId)

		// a loose match: "Kiev" is two edits from Kyiv and in reach of nothing else
		kiev := &Location{
			LocationType: LocationTypeCity,
			City:         "Kiev",
			Region:       "Kyiv City",
			Country:      "Ukraine",
			CountryCode:  "ua",
		}
		CreateLocation(ctx, kiev)
		kyiv := &Location{
			LocationType:     LocationTypeCity,
			City:             "Kyiv",
			Region:           "Kyiv City",
			Country:          "Ukraine",
			CountryCode:      "ua",
			CityGeonameId:    703448,
			RegionGeonameId:  703447,
			CountryGeonameId: 690791,
		}
		CreateLocation(ctx, kyiv)
		connect.AssertEqual(t, kyiv.LocationId, kiev.LocationId)

		// another place, however alike, is never adopted: Cambridge anchors to
		// Cambridge, so a lookup of Abridge makes its own row
		cambridge := &Location{
			LocationType: LocationTypeCity,
			City:         "Cambridge",
			Region:       "England",
			Country:      "United Kingdom",
			CountryCode:  "gb",
		}
		CreateLocation(ctx, cambridge)
		abridge := &Location{
			LocationType:     LocationTypeCity,
			City:             "Abridge",
			Region:           "England",
			Country:          "United Kingdom",
			CountryCode:      "gb",
			CityGeonameId:    9072588,
			RegionGeonameId:  6269131,
			CountryGeonameId: 2635167,
		}
		CreateLocation(ctx, abridge)
		connect.AssertNotEqual(t, abridge.LocationId, cambridge.LocationId)
		connect.AssertEqual(t, abridge.RegionLocationId, cambridge.RegionLocationId)
		connect.AssertEqual(t, storedGeonameId(ctx, cambridge.LocationId), uint32(0))

		// never across countries
		paris := &Location{
			LocationType: LocationTypeCity,
			City:         "Paris",
			Region:       "Texas",
			Country:      "United States",
			CountryCode:  "us",
		}
		CreateLocation(ctx, paris)
		parisFrance := &Location{
			LocationType:     LocationTypeCity,
			City:             "Paris",
			Region:           "Île-de-France",
			Country:          "France",
			CountryCode:      "fr",
			CityGeonameId:    2988507,
			RegionGeonameId:  3012874,
			CountryGeonameId: 3017382,
		}
		CreateLocation(ctx, parisFrance)
		connect.AssertNotEqual(t, parisFrance.LocationId, paris.LocationId)
		connect.AssertEqual(t, parisFrance.RegionLocationId, saintDenis.RegionLocationId)

		// a row that carries another geoname id is another place
		springfield := &Location{
			LocationType:     LocationTypeCity,
			City:             "Springfield",
			Region:           "Illinois",
			Country:          "United States",
			CountryCode:      "us",
			CityGeonameId:    4250542,
			RegionGeonameId:  4896861,
			CountryGeonameId: 6252001,
		}
		CreateLocation(ctx, springfield)
		springfeld := &Location{
			LocationType:     LocationTypeCity,
			City:             "Springfeld",
			Region:           "Illinois",
			Country:          "United States",
			CountryCode:      "us",
			CityGeonameId:    4200000009,
			RegionGeonameId:  4896861,
			CountryGeonameId: 6252001,
		}
		CreateLocation(ctx, springfeld)
		connect.AssertNotEqual(t, springfeld.LocationId, springfield.LocationId)
		connect.AssertEqual(t, springfeld.RegionLocationId, springfield.RegionLocationId)
	})
}

// The seeder takes every exact match before any loose one: "Selbe", which
// sorts first, would otherwise adopt the legacy row of "Selby" one edit away.
func TestAddDefaultLocationsMatchesExactlyBeforeLoosely(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		legacy := &Location{
			LocationType: LocationTypeCity,
			City:         "Selby",
			Region:       "England",
			Country:      "United Kingdom",
			CountryCode:  "gb",
		}
		CreateLocation(ctx, legacy)

		defer pushTestPlaces(`
version: 1
source: test
build_epoch: 1
countries:
  gb: {name: United Kingdom, geoname_id: 2635167, continent_code: eu, continent: Europe}
places:
  gb:
    England:
      Selbe: {geoname_id: 900004, region_geoname_id: 6269131, latitude: 53.7, longitude: -1.0, spread_km: 0, time_zone: Europe/London}
      Selby: {geoname_id: 2638419, region_geoname_id: 6269131, latitude: 53.7837, longitude: -1.0678, spread_km: 0, time_zone: Europe/London}
`)()
		AddDefaultLocations(ctx, -1)

		connect.AssertEqual(t, requireLocationByGeonameId(t, ctx, 2638419).LocationId, legacy.LocationId)
		selbe := requireLocationByGeonameId(t, ctx, 900004)
		connect.AssertNotEqual(t, selbe.LocationId, legacy.LocationId)
		connect.AssertEqual(t, selbe.City, "Selbe")
	})
}

// The seeder resolves against the process's one copy of the place list: a
// list a use before it loaded is the list it seeds from and leaves in place,
// not a second parse that replaces the first.
func TestAddDefaultLocationsSharesTheLoadedPlaceList(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer pushTestPlaces(testPlacesYaml)()

		places := CurrentPlaces()
		if places == nil {
			t.Fatal("the pushed place list did not load")
		}
		AddDefaultLocations(ctx, 0)
		if CurrentPlaces() != places {
			t.Fatal("the seeder replaced the loaded place list with a second copy")
		}
	})
}

// A lookup whose place is stored under its geoname id resolves without the
// place list, so a process whose lookups all find their place never reads it;
// the first lookup of a place not yet stored reads it, and resolves against it
// as before.
func TestCreateLocationReadsThePlaceListOnlyForAPlaceNotStoredById(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer pushTestPlaces(testPlacesYaml)()

		// Dorking and East Finchley are seeded; then the process's list is
		// dropped, as a process that did not seed has it
		AddDefaultLocations(ctx, 2)
		resetLocationPlaceNames()
		placeListLoaded := func() bool {
			_, loaded := currentLocationPlaceNamesLoad().peek()
			return loaded
		}

		eastFinchley := &Location{
			LocationType:     LocationTypeCity,
			City:             "East Finchley",
			Region:           "England",
			Country:          "United Kingdom",
			CountryCode:      "gb",
			CityGeonameId:    2650444,
			RegionGeonameId:  6269131,
			CountryGeonameId: 2635167,
		}
		CreateLocation(ctx, eastFinchley)
		connect.AssertEqual(t, eastFinchley.LocationId, requireLocationByGeonameId(t, ctx, 2650444).LocationId)
		england := &Location{
			LocationType:     LocationTypeRegion,
			Region:           "England",
			Country:          "United Kingdom",
			CountryCode:      "gb",
			RegionGeonameId:  6269131,
			CountryGeonameId: 2635167,
		}
		CreateLocation(ctx, england)
		connect.AssertEqual(t, england.LocationId, eastFinchley.RegionLocationId)
		unitedKingdom := &Location{
			LocationType:     LocationTypeCountry,
			Country:          "United Kingdom",
			CountryCode:      "gb",
			CountryGeonameId: 2635167,
		}
		CreateLocation(ctx, unitedKingdom)
		connect.AssertEqual(t, unitedKingdom.LocationId, eastFinchley.CountryLocationId)
		connect.AssertEqual(t, placeListLoaded(), false)

		// Forest Hill was not seeded: its lookup reads the list, and files the
		// new row under the stored region
		forestHill := &Location{
			LocationType:     LocationTypeCity,
			City:             "Forest Hill",
			Region:           "England",
			Country:          "United Kingdom",
			CountryCode:      "gb",
			CityGeonameId:    2649216,
			RegionGeonameId:  6269131,
			CountryGeonameId: 2635167,
		}
		CreateLocation(ctx, forestHill)
		connect.AssertEqual(t, placeListLoaded(), true)
		connect.AssertEqual(t, forestHill.RegionLocationId, eastFinchley.RegionLocationId)
		connect.AssertEqual(t, storedGeonameId(ctx, forestHill.LocationId), uint32(2649216))
	})
}

// A new GeoLite2 build that renames a country (Turkey to Türkiye, Czech
// Republic to Czechia) renames the stored row on the next seed: GeoLite2 is
// the source of truth, and the row is keyed by the country's geoname id, so it
// keeps its id and its code, its cities stay filed under it, and the search
// index follows the new name. A country row's full name is its code, so that
// column does not change. A lookup is not a seed and never renames: one that
// carries the old name resolves to the renamed row and leaves it as it is.
func TestAddDefaultLocationsFollowsARenamedCountry(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		// one build of the list, with the countries named as given
		placesYaml := func(czechName string, turkishName string) string {
			return fmt.Sprintf(`
version: 1
source: test
build_epoch: 1
countries:
  cz: {name: %s, geoname_id: 3077311, continent_code: eu, continent: Europe}
  tr: {name: %s, geoname_id: 298795, continent_code: as, continent: Asia}
places:
  cz:
    Prague:
      Prague: {geoname_id: 3067696, region_geoname_id: 3067695, latitude: 50.088, longitude: 14.4208, spread_km: 0, time_zone: Europe/Prague}
  tr:
    Istanbul:
      Istanbul: {geoname_id: 745044, region_geoname_id: 745042, latitude: 41.0138, longitude: 28.9497, spread_km: 0, time_zone: Europe/Istanbul}
`, czechName, turkishName)
		}
		// the stored columns a country row is named by
		storedCountryRow := func(locationId server.Id) (name string, fullName string, countryCode string) {
			server.Db(ctx, func(conn server.PgConn) {
				result, err := conn.Query(
					ctx,
					`SELECT location_name, location_full_name, country_code FROM location WHERE location_id = $1`,
					locationId,
				)
				server.WithPgResult(result, err, func() {
					if result.Next() {
						server.Raise(result.Scan(&name, &fullName, &countryCode))
					}
				})
			})
			return
		}
		// whether the location search finds a location by an exact search string
		searchFinds := func(query string, locationId server.Id) bool {
			_, ok := locationSearch().AroundIds(ctx, query, 0)[locationId]
			return ok
		}

		countries := []struct {
			code              string
			geonameId         uint32
			oldName           string
			newName           string
			city              string
			cityGeonameId     uint32
			regionGeonameId   uint32
			countryLocationId server.Id
			cityLocationId    server.Id
			regionLocationId  server.Id
		}{
			{code: "tr", geonameId: 298795, oldName: "Turkey", newName: "Türkiye", city: "Istanbul", cityGeonameId: 745044, regionGeonameId: 745042},
			{code: "cz", geonameId: 3077311, oldName: "Czech Republic", newName: "Czechia", city: "Prague", cityGeonameId: 3067696, regionGeonameId: 3067695},
		}

		// the build before the rename
		popPlaces := pushTestPlaces(placesYaml("Czech Republic", "Turkey"))
		AddDefaultLocations(ctx, -1)
		popPlaces()
		for i := range countries {
			country := &countries[i]
			countryLocation := requireLocationByGeonameId(t, ctx, country.geonameId)
			country.countryLocationId = countryLocation.LocationId
			name, fullName, countryCode := storedCountryRow(country.countryLocationId)
			connect.AssertEqual(t, name, country.oldName)
			connect.AssertEqual(t, fullName, country.code)
			connect.AssertEqual(t, countryCode, country.code)
			connect.AssertEqual(t, searchFinds(fmt.Sprintf("%s (%s)", country.oldName, country.code), country.countryLocationId), true)
			city := requireLocationByGeonameId(t, ctx, country.cityGeonameId)
			connect.AssertEqual(t, city.CountryLocationId, country.countryLocationId)
			country.cityLocationId = city.LocationId
			country.regionLocationId = city.RegionLocationId
		}

		// the build that renames them
		defer pushTestPlaces(placesYaml("Czechia", "Türkiye"))()
		AddDefaultLocations(ctx, -1)
		for _, country := range countries {
			// the same row, under the new name, with its code; its full name is
			// the code and stays so
			countryLocation := requireLocationByGeonameId(t, ctx, country.geonameId)
			connect.AssertEqual(t, countryLocation.LocationId, country.countryLocationId)
			connect.AssertEqual(t, countryLocation.Country, country.newName)
			name, fullName, countryCode := storedCountryRow(country.countryLocationId)
			connect.AssertEqual(t, name, country.newName)
			connect.AssertEqual(t, fullName, country.code)
			connect.AssertEqual(t, countryCode, country.code)
			connect.AssertEqual(t, countLocations(ctx, `location_type = $1 AND country_code = $2`, LocationTypeCountry, country.code), 1)

			// the city is still filed under it, and read through it
			city := requireLocationByGeonameId(t, ctx, country.cityGeonameId)
			connect.AssertEqual(t, city.LocationId, country.cityLocationId)
			connect.AssertEqual(t, city.CountryLocationId, country.countryLocationId)
			connect.AssertEqual(t, city.Country, country.newName)

			// the search finds the new name, and no longer the old one
			connect.AssertEqual(t, searchFinds(fmt.Sprintf("%s (%s)", country.newName, country.code), country.countryLocationId), true)
			connect.AssertEqual(t, searchFinds(fmt.Sprintf("%s (%s)", country.oldName, country.code), country.countryLocationId), false)

			// lookups that still carry the old name resolve to the same rows and
			// do not rename the country back
			countryLookup := &Location{
				LocationType:     LocationTypeCountry,
				Country:          country.oldName,
				CountryCode:      country.code,
				CountryGeonameId: country.geonameId,
			}
			CreateLocation(ctx, countryLookup)
			connect.AssertEqual(t, countryLookup.LocationId, country.countryLocationId)
			cityLookup := &Location{
				LocationType:     LocationTypeCity,
				City:             country.city,
				Region:           country.city,
				Country:          country.oldName,
				CountryCode:      country.code,
				CityGeonameId:    country.cityGeonameId,
				RegionGeonameId:  country.regionGeonameId,
				CountryGeonameId: country.geonameId,
			}
			CreateLocation(ctx, cityLookup)
			connect.AssertEqual(t, cityLookup.LocationId, country.cityLocationId)
			connect.AssertEqual(t, cityLookup.RegionLocationId, country.regionLocationId)
			connect.AssertEqual(t, cityLookup.CountryLocationId, country.countryLocationId)
			name, _, _ = storedCountryRow(country.countryLocationId)
			connect.AssertEqual(t, name, country.newName)
			connect.AssertEqual(t, searchFinds(fmt.Sprintf("%s (%s)", country.newName, country.code), country.countryLocationId), true)
			connect.AssertEqual(t, searchFinds(fmt.Sprintf("%s (%s)", country.oldName, country.code), country.countryLocationId), false)
		}
	})
}
