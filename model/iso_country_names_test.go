package model

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// liveBlankCountryCodes is the exact set of country codes that were observed
// with an empty `location_name` on the live beta deployment -- the rows the
// unnamed location-group member path created. Every one of them has to resolve
// from the built-in table, because none of them is in `iso-country-list.yml`
// (that is precisely why they were blank).
//
// Real data, do not trim. Pulled with:
//
//	SELECT country_code FROM location
//	WHERE location_type = 'country' AND location_name = '' ORDER BY 1;
var liveBlankCountryCodes = strings.Fields(`
	ad af ag ai al am ao aq aw ax az ba bb bd bf bh bi bj bl bm bn bo bq bs bt
	bv bw by bz cd cf cg ci cm cn cr cu cv cw dj dm do dz ec eh er et fk fo ga
	gd ge gf gh gi gl gm gn gp gq gs gt gw gy hn ht im io iq ir jm jo ke kg kh
	km kn kp kw ky kz la lb lc li lk lr ls ly ma mc md me mf mg mk ml mm mn mo
	mq mr ms mu mv mw mz na ne ni np om pa pe pk pm pr ps py qa re rs ru rw sc
	sd sh sj sl sm sn so sr ss st sv sx sy sz tc td tf tg tj tk tl tm tn to tt
	tv tw tz ua ug um uy uz va vc ve vg vi vn vu wf ws ye yt zm zw
`)

// TestISOCountryNameCoversLiveBlankCodes is the coverage gate: if the generated
// table ever loses an entry, the code that lost it is named in the failure.
func TestISOCountryNameCoversLiveBlankCodes(t *testing.T) {
	missing := []string{}
	for _, countryCode := range liveBlankCountryCodes {
		name, ok := ISOCountryName(countryCode)
		if !ok || name == "" {
			missing = append(missing, countryCode)
		}
	}
	if 0 < len(missing) {
		t.Fatalf(
			"%d of the %d country codes observed blank on beta do not resolve: %s",
			len(missing),
			len(liveBlankCountryCodes),
			strings.Join(missing, " "),
		)
	}
}

func TestISOCountryName(t *testing.T) {
	// the full ISO 3166-1 alpha-2 assignment
	connect.AssertEqual(t, len(isoCountryNames), 249)

	name, ok := ISOCountryName("cn")
	connect.AssertEqual(t, ok, true)
	connect.AssertEqual(t, name, "China")

	// `do` is a Go keyword lookalike that a naive generator can eat
	name, ok = ISOCountryName("do")
	connect.AssertEqual(t, ok, true)
	connect.AssertEqual(t, name, "Dominican Republic")

	// case insensitive, since callers hold codes in either case
	// (`iso-country-list.yml` is upper case, the `location` table is lower)
	name, ok = ISOCountryName("DO")
	connect.AssertEqual(t, ok, true)
	connect.AssertEqual(t, name, "Dominican Republic")

	// the short form where the standard has one
	name, ok = ISOCountryName("kr")
	connect.AssertEqual(t, ok, true)
	connect.AssertEqual(t, name, "South Korea")

	// no entry may be blank -- a blank entry would reintroduce the bug
	for countryCode, countryName := range isoCountryNames {
		if countryName == "" {
			t.Fatalf("blank name for country code \"%s\"", countryCode)
		}
		if countryCode != strings.ToLower(countryCode) || len(countryCode) != 2 {
			t.Fatalf("country code \"%s\" is not a lower case alpha-2 code", countryCode)
		}
	}

	// codes that are not assigned must not resolve, and must not resolve to
	// themselves
	for _, countryCode := range []string{"", "x", "zz", "xx", "q1", "united states"} {
		name, ok = ISOCountryName(countryCode)
		connect.AssertEqual(t, ok, false)
		connect.AssertEqual(t, name, "")
	}
}

// blankNamedLocationCount is the invariant every case below asserts: whatever
// route a location is created by, and whether that route succeeds, degrades or
// refuses, the `location` table must never gain a row with an empty name.
func blankNamedLocationCount(ctx context.Context, t testing.TB) int {
	var count int
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT COUNT(*) FROM location WHERE location_name = ''`,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&count))
			}
		})
	})
	return count
}

func locationCount(ctx context.Context, t testing.TB, countryCode string) int {
	var count int
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT COUNT(*) FROM location WHERE country_code = $1`,
			countryCode,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&count))
			}
		})
	})
	return count
}

func locationName(ctx context.Context, t testing.TB, locationId server.Id) string {
	var name string
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT location_name FROM location WHERE location_id = $1`,
			locationId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&name))
			}
		})
	})
	return name
}

// TestCreateLocationResolvesCountryName is the bug itself: a country code with
// no name attached, which is exactly what the location-group member path hands
// `CreateLocation`.
func TestCreateLocationResolvesCountryName(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		// `cn` is not in iso-country-list.yml, and had 272 providers behind a
		// blank name on the live deployment
		location := &Location{
			LocationType: LocationTypeCountry,
			CountryCode:  "cn",
		}
		CreateLocation(ctx, location)

		connect.AssertEqual(t, locationName(ctx, t, location.LocationId), "China")
		connect.AssertEqual(t, blankNamedLocationCount(ctx, t), 0)
	})
}

// TestCreateLocationConfigCountryNameWins pins the precedence in the plan's
// global constraints: a deployment that names a country its own way keeps that
// name, even though the Go table also has the code.
func TestCreateLocationConfigCountryNameWins(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		// both sides have `kp`: the table says "North Korea"
		name, ok := ISOCountryName("kp")
		connect.AssertEqual(t, ok, true)
		connect.AssertEqual(t, name, "North Korea")

		pop := server.Config.PushSimpleResource(
			"iso-country-list.yml",
			[]byte("KP: Deployment Naming For Korea\n"),
		)
		defer pop()

		location := &Location{
			LocationType: LocationTypeCountry,
			CountryCode:  "kp",
		}
		CreateLocation(ctx, location)

		connect.AssertEqual(
			t,
			locationName(ctx, t, location.LocationId),
			"Deployment Naming For Korea",
		)
		connect.AssertEqual(t, blankNamedLocationCount(ctx, t), 0)
	})
}

// TestCreateLocationUnknownCountryCodeCreatesNoRow: a code that is in neither
// source is not a country. It must not be stored blank, and it must not be
// stored under its own code as a name either.
func TestCreateLocationUnknownCountryCodeCreatesNoRow(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		for _, countryCode := range []string{"zz", "q1", ""} {
			raised := func() (raised bool) {
				defer func() {
					if err := recover(); err != nil {
						raised = true
					}
				}()
				location := &Location{
					LocationType: LocationTypeCountry,
					CountryCode:  countryCode,
				}
				CreateLocation(ctx, location)
				return
			}()

			if !raised {
				t.Fatalf("country code \"%s\" was accepted", countryCode)
			}
			connect.AssertEqual(t, locationCount(ctx, t, countryCode), 0)
		}

		connect.AssertEqual(t, blankNamedLocationCount(ctx, t), 0)
	})
}

// TestCreateLocationUnnamedRegionCreatesNoRow covers the 2 blank region rows on
// beta (`hk`, `sg`, `location_full_name` ", hk" / ", sg"). mmdb has no
// subdivision for a subdivision-less country, so this arrives on the ordinary
// connect-announce path: the region must not be written, and the location must
// still resolve -- at country granularity -- rather than failing the caller.
func TestCreateLocationUnnamedRegionCreatesNoRow(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		// a region with no name
		region := &Location{
			LocationType: LocationTypeRegion,
			Region:       "",
			CountryCode:  "hk",
		}
		CreateLocation(ctx, region)

		connect.AssertEqual(t, region.LocationType, LocationTypeCountry)
		connect.AssertEqual(t, locationName(ctx, t, region.LocationId), "Hong Kong")

		// the shape mmdb actually returns for hk/sg: a named city with no
		// region at all, which `GuessLocationType` classifies as a city
		city := &Location{
			LocationType: LocationTypeCity,
			City:         "Kowloon",
			Region:       "",
			CountryCode:  "hk",
		}
		CreateLocation(ctx, city)

		connect.AssertEqual(t, city.LocationType, LocationTypeCountry)
		connect.AssertEqual(t, city.LocationId, region.LocationId)

		// and a city with no name of its own
		unnamedCity := &Location{
			LocationType: LocationTypeCity,
			City:         "",
			Region:       "Kowloon City",
			CountryCode:  "hk",
		}
		CreateLocation(ctx, unnamedCity)

		connect.AssertEqual(t, unnamedCity.LocationType, LocationTypeRegion)
		connect.AssertEqual(t, locationName(ctx, t, unnamedCity.LocationId), "Kowloon City")

		connect.AssertEqual(t, blankNamedLocationCount(ctx, t), 0)

		// the ", hk"-shaped full name is a symptom of the same empty field
		var badFullNameCount int
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(
				ctx,
				`SELECT COUNT(*) FROM location WHERE location_full_name LIKE ', %'`,
			)
			server.WithPgResult(result, err, func() {
				if result.Next() {
					server.Raise(result.Scan(&badFullNameCount))
				}
			})
		})
		connect.AssertEqual(t, badFullNameCount, 0)
	})
}

// A migration can find both an older canonical region/city and a later legacy
// row whose name was blank. The backfill gives the legacy row a display name
// but preserves its old full name when the canonical key is already occupied.
// Lookups must prefer the canonical hierarchy and must also recognize a
// normalized city that still belongs to the legacy region id.
func TestCreateLocationHandlesBackfilledCanonicalCollisions(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		canonical := &Location{
			LocationType: LocationTypeCity,
			City:         "Bedok",
			Region:       "Singapore",
			Country:      "Singapore",
			CountryCode:  "sg",
		}
		CreateLocation(ctx, canonical)

		legacyRegionId := server.RequireParseId("00000000-0000-0000-0000-000000000001")
		legacyDuplicateCityId := server.RequireParseId("00000000-0000-0000-0000-000000000002")
		legacyNormalizedCityId := server.RequireParseId("00000000-0000-0000-0000-000000000003")
		server.Db(ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(ctx, `
				INSERT INTO location (
					location_id,
					location_type,
					location_name,
					city_location_id,
					region_location_id,
					country_location_id,
					country_code,
					location_full_name
				)
				VALUES
					($1, 'region', 'Singapore', NULL, $1, $4, 'sg', ', sg'),
					($2, 'city', 'Bedok', $2, $1, $4, 'sg', 'Bedok, , sg'),
					($3, 'city', 'Jurong', $3, $1, $4, 'sg', 'Jurong, Singapore, sg')
			`,
				legacyRegionId,
				legacyDuplicateCityId,
				legacyNormalizedCityId,
				canonical.CountryLocationId,
			))
		})

		duplicate := &Location{
			LocationType: LocationTypeCity,
			City:         "Bedok",
			Region:       "Singapore",
			Country:      "Singapore",
			CountryCode:  "sg",
		}
		CreateLocation(ctx, duplicate)
		connect.AssertEqual(t, duplicate.LocationId, canonical.LocationId)
		connect.AssertEqual(t, duplicate.RegionLocationId, canonical.RegionLocationId)

		normalizedLegacy := &Location{
			LocationType: LocationTypeCity,
			City:         "Jurong",
			Region:       "Singapore",
			Country:      "Singapore",
			CountryCode:  "sg",
		}
		CreateLocation(ctx, normalizedLegacy)
		connect.AssertEqual(t, normalizedLegacy.LocationId, legacyNormalizedCityId)
		connect.AssertEqual(t, normalizedLegacy.RegionLocationId, legacyRegionId)
	})
}

// TestCreateLocationUnknownLocationTypeCreatesNoRow closes the last route to a
// blank name: `LocationType` is what selects which insert runs, so a value that
// is none of the three (the zero value included, from a `Location` built without
// the field) falls through every early return and reaches the city insert with
// an empty `City`.
func TestCreateLocationUnknownLocationTypeCreatesNoRow(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		for _, locationType := range []LocationType{"", "continent", "planet"} {
			raised := func() (raised bool) {
				defer func() {
					if err := recover(); err != nil {
						raised = true
					}
				}()
				location := &Location{
					LocationType: locationType,
					Region:       "Testland",
					CountryCode:  "us",
				}
				CreateLocation(ctx, location)
				return
			}()

			if !raised {
				t.Fatalf("location type \"%s\" was accepted", locationType)
			}
		}

		connect.AssertEqual(t, blankNamedLocationCount(ctx, t), 0)
		connect.AssertEqual(t, locationCount(ctx, t, "us"), 0)
	})
}

// TestAddDefaultLocationsHasNoBlankNames is the regression gate for the
// function that produced all 161 blank rows: every hardcoded location-group
// member goes through the fixed `case string` branch here.
func TestAddDefaultLocationsHasNoBlankNames(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		AddDefaultLocations(ctx, 10)

		connect.AssertEqual(t, blankNamedLocationCount(ctx, t), 0)

		// and the country rows that iso-country-list.yml does name keep that
		// name rather than the table's
		var countryCount int
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(
				ctx,
				`SELECT COUNT(*) FROM location WHERE location_type = $1`,
				LocationTypeCountry,
			)
			server.WithPgResult(result, err, func() {
				if result.Next() {
					server.Raise(result.Scan(&countryCount))
				}
			})
		})
		if countryCount < 58 {
			t.Fatalf("expected at least the 58 configured countries, found %d", countryCount)
		}

		// Where both sources name a code and disagree, the config's name is the
		// one that lands. The code is taken from the config this deployment
		// actually ships rather than hardcoded: which names differ is config
		// data, so naming one here pins the test to one deployment's file and
		// makes it assert nothing the day that row changes to agree.
		countryCode, configName, ok := disagreeingCountryCode(t)
		if !ok {
			return
		}
		location := &Location{
			LocationType: LocationTypeCountry,
			CountryCode:  countryCode,
		}
		CreateLocation(ctx, location)
		connect.AssertEqual(t, locationName(ctx, t, location.LocationId), configName)
	})
}

// disagreeingCountryCode returns one country code that iso-country-list.yml and
// the built-in table both name, and name differently, with the config's name.
func disagreeingCountryCode(t testing.TB) (countryCode string, configName string, found bool) {
	t.Helper()
	resource, err := server.Config.SimpleResource("iso-country-list.yml")
	if err != nil {
		t.Fatalf("read iso-country-list.yml: %s", err)
	}
	// sorted so a failure names the same code on every run
	codes := []string{}
	names := map[string]string{}
	for code, name := range resource.Parse() {
		configName, ok := name.(string)
		if !ok || configName == "" {
			continue
		}
		code = strings.ToLower(code)
		codes = append(codes, code)
		names[code] = configName
	}
	slices.Sort(codes)
	for _, code := range codes {
		tableName, ok := ISOCountryName(code)
		if ok && tableName != names[code] {
			return code, names[code], true
		}
	}
	return "", "", false
}
