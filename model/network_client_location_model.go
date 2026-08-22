package model

import (
	"context"
	// "encoding/hex"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"bytes"
	"encoding/gob"
	"encoding/json"
	"errors"
	"fmt"
	// "math"
	mathrand "math/rand"
	"slices"
	"unicode/utf8"

	"github.com/urnetwork/glog"

	"maps"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/search"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/stats"
)

func init() {
	resetCountryCodeLocationIds()
	server.OnReset(func() {
		resetCountryCodeLocationIds()
	})
	server.OnWarmup(func() {
		countryCodeLocationIds()
	})

	server.OnReset(func() {
		resetLocationDirectory()
	})
	server.OnWarmup(func() {
		loadLocationDirectory()
	})
}

func resetCountryCodeLocationIds() {
	countryCodeLocationIds = sync.OnceValue(func() map[string]server.Id {
		ctx := context.Background()

		countryCodeLocationIds := map[string]server.Id{}

		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(
				ctx,
				`
				SELECT
					country_code,
					location_id
				FROM location
				WHERE
					location_type = 'country'
				`,
			)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var countryCode string
					var locationId server.Id
					server.Raise(result.Scan(
						&countryCode,
						&locationId,
					))
					countryCode = strings.ToLower(countryCode)
					countryCodeLocationIds[countryCode] = locationId
				}
			})
		})

		return countryCodeLocationIds
	})
}

// country code is lowercase
var countryCodeLocationIds func() map[string]server.Id

type locationDirectoryEntry struct {
	Name        string
	CountryCode string
	Latitude    *float64
	Longitude   *float64
}

type locationDirectorySnapshot struct {
	entries  map[server.Id]*locationDirectoryEntry
	loadTime time.Time
}

// refresh matches the `clientLocationKey` ttl
const locationDirectoryStaleAfter = 30 * time.Minute

var locationDirectoryValue atomic.Pointer[locationDirectorySnapshot]
var locationDirectoryLoading atomic.Bool

func resetLocationDirectory() {
	locationDirectoryValue.Store(nil)
	locationDirectoryLoading.Store(false)
}

// the current location directory without blocking the caller.
// nil until the first load completes (callers omit locations), and a stale
// snapshot is served while a single background reload runs.
func locationDirectory() map[server.Id]*locationDirectoryEntry {
	snapshot := locationDirectoryValue.Load()
	if snapshot == nil || locationDirectoryStaleAfter <= time.Since(snapshot.loadTime) {
		if locationDirectoryLoading.CompareAndSwap(false, true) {
			go connect.HandleError(func() {
				defer locationDirectoryLoading.Store(false)
				loadLocationDirectory()
			})
		}
	}
	if snapshot == nil {
		return nil
	}
	return snapshot.entries
}

// locationDirectoryRedisKey shares one computed directory across the fleet.
// The query behind it scans the whole ~53M-row reliability table, and every
// process used to run it independently on its own staleness timer: ~1,550
// executions per 48h, ~2% of all db time, to produce the same ~9k rows each
// time (2026-08-11 audit). The ttl is the same staleness bound the per-process
// snapshot already had, so nothing is served staler than before — the scan is
// just paid once per window for the fleet instead of once per process.
const locationDirectoryRedisKey = "location_directory"

// locationDirectoryRow is the wire form of one directory entry. The directory is
// keyed by location id in memory, but server.Id implements MarshalJSON and not
// MarshalText, so it cannot be a json map key — the cache carries a list and the
// map is rebuilt on read.
type locationDirectoryRow struct {
	LocationId  server.Id `json:"location_id"`
	Name        string    `json:"name"`
	CountryCode string    `json:"country_code"`
	Latitude    *float64  `json:"latitude,omitempty"`
	Longitude   *float64  `json:"longitude,omitempty"`
}

// getLocationDirectoryCache reads the fleet-shared directory, or nil on miss.
// A redis failure is a miss, not an error: the caller falls back to querying pg,
// which is exactly the pre-cache behavior.
func getLocationDirectoryCache(ctx context.Context) map[server.Id]*locationDirectoryEntry {
	var entries map[server.Id]*locationDirectoryEntry
	server.Redis(ctx, func(r server.RedisClient) {
		rowsJson, err := r.Get(ctx, locationDirectoryRedisKey).Result()
		if err != nil {
			// miss, or redis is unavailable; fall back to pg
			return
		}
		rows := []*locationDirectoryRow{}
		if err := json.Unmarshal([]byte(rowsJson), &rows); err != nil {
			glog.V(1).Infof("[nclm]location directory cache decode err = %v\n", err)
			return
		}
		entries = map[server.Id]*locationDirectoryEntry{}
		for _, row := range rows {
			entries[row.LocationId] = &locationDirectoryEntry{
				Name:        row.Name,
				CountryCode: row.CountryCode,
				Latitude:    row.Latitude,
				Longitude:   row.Longitude,
			}
		}
	})
	return entries
}

func setLocationDirectoryCache(
	ctx context.Context,
	entries map[server.Id]*locationDirectoryEntry,
	ttl time.Duration,
) {
	rows := make([]*locationDirectoryRow, 0, len(entries))
	for locationId, entry := range entries {
		rows = append(rows, &locationDirectoryRow{
			LocationId:  locationId,
			Name:        entry.Name,
			CountryCode: entry.CountryCode,
			Latitude:    entry.Latitude,
			Longitude:   entry.Longitude,
		})
	}
	rowsJson, err := json.Marshal(rows)
	if err != nil {
		glog.V(1).Infof("[nclm]location directory cache encode err = %v\n", err)
		return
	}
	server.Redis(ctx, func(r server.RedisClient) {
		if err := r.Set(ctx, locationDirectoryRedisKey, string(rowsJson), ttl).Err(); err != nil {
			// the directory is still usable in-process; the next loader just
			// pays the query again
			glog.V(1).Infof("[nclm]location directory cache write err = %v\n", err)
		}
	})
}

func loadLocationDirectory() {
	ctx := context.Background()

	entries := getLocationDirectoryCache(ctx)
	if entries == nil {
		entries = queryLocationDirectory(ctx)
		setLocationDirectoryCache(ctx, entries, locationDirectoryStaleAfter)
	}

	locationDirectoryValue.Store(&locationDirectorySnapshot{
		entries:  entries,
		loadTime: server.NowUtc(),
	})
}

func queryLocationDirectory(ctx context.Context) map[server.Id]*locationDirectoryEntry {
	entries := map[server.Id]*locationDirectoryEntry{}

	server.Db(ctx, func(conn server.PgConn) {
		// The seeded city list can be ~10^6 rows, so bound the directory to
		// locations referenced by providers that are usable now. Historical
		// disconnected rows dominate this table (~58M on main) but cannot appear
		// in a current provider result; excluding them lets the existing
		// (valid,connected,client_id) index drive a tiny materialized set.
		//
		// The referenced set is collected as DISTINCT (city, region, country)
		// TRIPLES in a single pass, then unnested. Selecting each column's
		// DISTINCT separately and UNIONing them reads the same table three
		// times — the planner runs three independent parallel seq scans, which
		// measured 3x the buffers and ~3.5x the cpu of this shape (2026-08-11).
		// The triple set is tiny (~7.8k rows against ~53M scanned), so the
		// unnest above it is free; unnesting the three columns per ROW instead
		// would push ~159M rows through the function scan and cost far more
		// than the scan it saves.
		result, err := conn.Query(
			ctx,
			`
			SELECT
				location.location_id,
				location.location_name,
				location.country_code,
				location.latitude,
				location.longitude
			FROM location
			WHERE location.location_id IN (
				SELECT DISTINCT loc.id
					FROM (
						SELECT DISTINCT
							city_location_id AS c,
							region_location_id AS r,
							country_location_id AS n
						FROM network_client_location_reliability
						WHERE
							network_client_location_reliability.valid = true AND
							network_client_location_reliability.connected = true
					) triples
				CROSS JOIN LATERAL unnest(ARRAY[triples.c, triples.r, triples.n]) AS loc(id)
				WHERE loc.id IS NOT NULL
			)
			`,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var locationId server.Id
				entry := &locationDirectoryEntry{}
				server.Raise(result.Scan(
					&locationId,
					&entry.Name,
					&entry.CountryCode,
					&entry.Latitude,
					&entry.Longitude,
				))
				entry.CountryCode = strings.ToLower(entry.CountryCode)
				entries[locationId] = entry
			}
		})
	})

	return entries
}

const DefaultMaxDistanceFraction = float32(0.2)

const StrongPrivacyLaws = "Strong Privacy Laws and Internet Freedom"

// called from db_migrations to add default locations and groups
func AddDefaultLocations(ctx context.Context, cityLimit int) {
	createCountry := func(countryCode string, country string) {
		location := &Location{
			LocationType: LocationTypeCountry,
			Country:      country,
			CountryCode:  countryCode,
		}
		CreateLocation(ctx, location)
	}

	createCity := func(countryCode string, country string, region string, city string) {
		location := &Location{
			LocationType: LocationTypeCity,
			Country:      country,
			CountryCode:  countryCode,
			Region:       region,
			City:         city,
		}
		CreateLocation(ctx, location)
	}

	createLocationGroup := func(promoted bool, name string, members ...any) {
		// member can be a country code, Location, or *Location,

		memberLocationIds := []server.Id{}

		var add func(member any)
		add = func(member any) {
			switch v := member.(type) {
			case []any:
				for _, a := range v {
					add(a)
				}
			case string:
				// country code
				//
				// The name MUST be resolved here. This branch used to build a
				// `&Location{LocationType: LocationTypeCountry, CountryCode: v}`
				// with no `Country` at all, and `CreateLocation` writes
				// `location_name` from `Country` -- so every country reachable
				// only through a location group (i.e. every code below that
				// iso-country-list.yml does not name) was inserted with an empty
				// name. That single omission produced 161 blank-named country
				// rows on the live beta deployment.
				countryCode := strings.ToLower(v)
				country, ok := resolveCountryName(countryCode)
				if !ok {
					// deliberately fatal: the member lists below are hardcoded
					// country codes, so an unresolvable one is a typo in this
					// file, not a data condition to tolerate.
					panic(fmt.Errorf(
						"location group \"%s\" member \"%s\" is not a known country code",
						name,
						v,
					))
				}
				location := &Location{
					LocationType: LocationTypeCountry,
					Country:      country,
					CountryCode:  countryCode,
				}
				CreateLocation(ctx, location)
				memberLocationIds = append(memberLocationIds, location.LocationId)
			case Location:
				CreateLocation(ctx, &v)
				memberLocationIds = append(memberLocationIds, v.LocationId)
			case *Location:
				CreateLocation(ctx, v)
				memberLocationIds = append(memberLocationIds, v.LocationId)
			}
		}
		for _, member := range members {
			add(member)
		}

		locationGroup := &LocationGroup{
			Name:              name,
			Promoted:          promoted,
			MemberLocationIds: memberLocationIds,
		}
		CreateLocationGroup(ctx, locationGroup)
	}

	// country code -> name
	countries := server.Config.RequireSimpleResource("iso-country-list.yml").Parse()

	// country code -> region -> []city
	cities := server.Config.RequireSimpleResource("city-list.yml").Parse()

	countryCodesToRemoveFromCities := []string{}
	for countryCode, _ := range cities {
		if _, ok := countries[countryCode]; !ok {
			// server.Logger().Printf("Missing country for %s", countryCode)
			countryCodesToRemoveFromCities = append(countryCodesToRemoveFromCities, countryCode)
		}
	}
	for _, countryCode := range countryCodesToRemoveFromCities {
		delete(cities, countryCode)
	}

	func() {
		// countries
		countryCount := len(countries)
		countryIndex := 0
		for countryCode, country := range countries {
			countryIndex += 1
			glog.Infof("[loc][%d/%d] %s, %s\n", countryIndex, countryCount, countryCode, country)
			createCountry(countryCode, country.(string))
		}
	}()

	func() {
		// cities
		cityCount := 0
		for _, regions := range cities {
			for _, cities := range regions.(map[string]any) {
				for range cities.([]any) {
					cityCount += 1
				}
			}
		}
		cityIndex := 0
		for countryCode, regions := range cities {
			for region, cities := range regions.(map[string]any) {
				country_, ok := countries[countryCode]
				if !ok {
					panic(fmt.Errorf("Missing country for %s", countryCode))
				}
				country := country_.(string)
				for _, city := range cities.([]any) {
					cityIndex += 1
					if 0 <= cityLimit && cityLimit < cityIndex {
						return
					}
					glog.Infof("[loc][%d/%d] %s, %s, %s\n", cityIndex, cityCount, countryCode, region, city)
					createCity(countryCode, country, region, city.(string))
				}
			}
		}
	}()

	// values can be country code or *Location
	eu := []any{
		"at",
		"be",
		"bg",
		"hr",
		"cy",
		"cz",
		"dk",
		"ee",
		"fi",
		"fr",
		"de",
		"gr",
		"hu",
		"ie",
		"it",
		"lv",
		"lt",
		"lu",
		"mt",
		"nl",
		"pl",
		"pt",
		"ro",
		"sk",
		"si",
		"es",
		"se",
	}
	nordic := []any{
		"dk",
		"fi",
		"is",
		"no",
		"se",
	}

	customRegions := map[string][]any{
		// https://www.gov.uk/eu-eea
		"European Union (EU)": eu,
		"Nordic":              nordic,
		StrongPrivacyLaws: []any{
			// all EU/EEA countries adhere to the General Data Protection Regulation (GDPR), which establishes a globally recognized high standard for individual privacy and data rights
			// Strong national framework complementing GDPR, good overall human rights ranking.
			"at",
			// Adheres to GDPR, central location for many EU institutions with strong historical focus on privacy.
			"be",
			// High ranking in civil liberties and democratic institutions.
			"cz",
			// Consistently ranked globally for strong democracy, civil liberties, and robust privacy culture (Nordic model/GDPR).
			"dk",
			// Highly digitized state with a strong emphasis on digital freedom, transparency, and data protection via the GDPR.
			"ee",
			// Consistently ranked globally for human rights, democracy, and strong national privacy laws (Data Protection Act) supplementing GDPR.
			"fi",
			// Strong independent supervisory authority (CNIL) and firm commitment to the GDPR framework.
			"fr",
			// The strongest national legal tradition of data protection (Informational Self-Determination) which led to the GDPR; Bundesdatenschutzgesetz (BDSG) reinforces privacy protections.
			"de",
			// As the main establishment for many global tech companies, the Data Protection Commission (DPC) has a central role in GDPR enforcement.
			"ie",
			// Part of the EEA, adhering to GDPR, and consistently ranked top globally for human rights and press freedom.
			"is",
			// Strong privacy law tradition and active national data protection authority (Garante).
			"it",
			// High ranking in political rights and civil liberties, strong adherence to GDPR.
			"lt",
			// Active role in EU policy and GDPR compliance, high economic stability.
			"lu",
			// High ranking in civil liberties and democracy, with an independent and active data protection authority.
			"nl",
			// Part of the EEA, adhering to GDPR, and consistently ranked top globally for human rights and rule of law.
			"no",
			// High ranking in civil liberties and strong adherence to GDPR.
			"pt",
			// Highly active in GDPR enforcement (highest number of fines in EU), strong focus on citizen data protection.
			"es",
			// Consistently ranked globally for human rights, transparency, and robust privacy culture (Nordic model/GDPR).
			"se",

			// these countries also rank high in privacy, human rights, and internet freedom
			"ch",
			"jp",
			"ca",
			"kr",
			"nz",
			"ar",
			"br",
			"sg",

			// Within the US, since there is no national privacy law, strong privacy is at the state level
			// https://www.ncsl.org/technology-and-communication/state-laws-related-to-digital-privacy
			// https://pro.bloomberglaw.com/insights/privacy/state-privacy-legislation-tracker
			&Location{
				LocationType: LocationTypeRegion,
				Region:       "California",
				Country:      "United States",
				CountryCode:  "us",
			},
			&Location{
				LocationType: LocationTypeRegion,
				Region:       "Colorado",
				Country:      "United States",
				CountryCode:  "us",
			},
			&Location{
				LocationType: LocationTypeRegion,
				Region:       "Connecticut",
				Country:      "United States",
				CountryCode:  "us",
			},
			&Location{
				LocationType: LocationTypeRegion,
				Region:       "Delaware",
				Country:      "United States",
				CountryCode:  "us",
			},
			&Location{
				LocationType: LocationTypeRegion,
				Region:       "Maryland",
				Country:      "United States",
				CountryCode:  "us",
			},
			&Location{
				LocationType: LocationTypeRegion,
				Region:       "Minnesota",
				Country:      "United States",
				CountryCode:  "us",
			},
			&Location{
				LocationType: LocationTypeRegion,
				Region:       "Oregon",
				Country:      "United States",
				CountryCode:  "us",
			},
			&Location{
				LocationType: LocationTypeRegion,
				Region:       "Virginia",
				Country:      "United States",
				CountryCode:  "us",
			},
			&Location{
				LocationType: LocationTypeRegion,
				Region:       "Texas",
				Country:      "United States",
				CountryCode:  "us",
			},
			&Location{
				LocationType: LocationTypeRegion,
				Region:       "New Hampshire",
				Country:      "United States",
				CountryCode:  "us",
			},
			&Location{
				LocationType: LocationTypeRegion,
				Region:       "Montana",
				Country:      "United States",
				CountryCode:  "us",
			},
		},
	}
	for name, members := range customRegions {
		// server.Logger().Printf("Create promoted group %s\n", name)
		createLocationGroup(false, name, members...)
	}

	// subregions
	// https://en.wikipedia.org/wiki/Subregion
	unSubregions := map[string][]any{
		// https://en.wikipedia.org/wiki/United_Nations_geoscheme_for_Africa
		"Northern Africa": []any{
			"dz",
			"eg",
			"ly",
			"ma",
			"sd",
			"tn",
			"eh",
		},
		"Eastern Africa": []any{
			"io",
			"bi",
			"km",
			"dj",
			"er",
			"et",
			"tf",
			"ke",
			"mg",
			"mw",
			"mu",
			"yt",
			"mz",
			"re",
			"rw",
			"sc",
			"so",
			"ss",
			"ug",
			"tz",
			"zw",
		},
		"Central Africa": []any{
			"ao",
			"cm",
			"cf",
			"td",
			"cg",
			"cd",
			"gq",
			"ga",
			"st",
		},
		"Southern Africa": []any{
			"bw",
			"sz",
			"ls",
			"na",
			"za",
		},
		"Western Africa": []any{
			"bj",
			"bf",
			"cv",
			"ci",
			"gm",
			"gh",
			"gn",
			"gw",
			"lr",
			"ml",
			"mr",
			"ne",
			"ng",
			"sh",
			"sn",
			"sl",
			"tg",
		},

		// https://en.wikipedia.org/wiki/United_Nations_geoscheme_for_Asia
		"Central Asia": []any{
			"kz",
			"kg",
			"tj",
			"tm",
			"uz",
		},
		"Eastern Asia": []any{
			"cn",
			"hk",
			"mo",
			"kp",
			"jp",
			"mn",
			"kr",
		},
		"Southeastern Asia": []any{
			"bn",
			"kh",
			"id",
			"la",
			"my",
			"mm",
			"ph",
			"sg",
			"th",
			"tl",
			"vn",
		},
		"Southern Asia": []any{
			"af",
			"bd",
			"bt",
			"in",
			"ir",
			"mv",
			"np",
			"pk",
			"lk",
		},
		"Western Asia": []any{
			"am",
			"az",
			"bh",
			"cy",
			"ge",
			"iq",
			"il",
			"jo",
			"kw",
			"lb",
			"om",
			"qa",
			"sa",
			"ps",
			"sy",
			"tr",
			"ae",
			"ye",
		},

		// https://en.wikipedia.org/wiki/United_Nations_geoscheme_for_Europe
		"Eastern Europe": []any{
			"by",
			"bg",
			"cz",
			"hu",
			"pl",
			"md",
			"ro",
			"ru",
			"sk",
			"ua",
		},
		"Northern Europe": []any{
			"ax",
			"dk",
			"ee",
			"fo",
			"fi",
			"is",
			"ie",
			"im",
			"lv",
			"lt",
			"no",
			"sj",
			"se",
			"gb",
		},
		"Southern Europe": []any{
			"al",
			"ad",
			"ba",
			"hr",
			"gi",
			"gr",
			"va",
			"it",
			"mt",
			"me",
			"mk",
			"pt",
			"sm",
			"rs",
			"si",
			"es",
		},
		"Western Europe": []any{
			"at",
			"be",
			"fr",
			"de",
			"li",
			"lu",
			"mc",
			"nl",
			"ch",
		},

		// https://en.wikipedia.org/wiki/United_Nations_geoscheme_for_the_Americas
		"Caribbean": []any{
			"ai",
			"ag",
			"aw",
			"bs",
			"bb",
			"bq",
			"vg",
			"ky",
			"cu",
			"cw",
			"dm",
			"do",
			"gd",
			"gp",
			"ht",
			"jm",
			"mq",
			"ms",
			"pr",
			"bl",
			"kn",
			"lc",
			"mf",
			"vc",
			"sx",
			"tt",
			"tc",
			"vi",
		},
		"Central America": []any{
			"bz",
			"cr",
			"sv",
			"gt",
			"hn",
			"mx",
			"ni",
			"pa",
		},
		"South America": []any{
			"ar",
			"bo",
			"bv",
			"br",
			"cl",
			"co",
			"ec",
			"fk",
			"gf",
			"gy",
			"py",
			"pe",
			"gs",
			"sr",
			"uy",
			"ve",
		},
		"Northern America": []any{
			"bm",
			"ca",
			"gl",
			"pm",
			"us",
		},

		"Antarctica": []any{
			"aq",
		},
	}
	for name, members := range unSubregions {
		// server.Logger().Printf("Create group %s\n", name)
		createLocationGroup(false, name, members...)
	}
}

type LocationType = string

const (
	LocationTypeCity    LocationType = "city"
	LocationTypeRegion  LocationType = "region"
	LocationTypeCountry LocationType = "country"
)

type Location struct {
	LocationType      LocationType
	City              string
	Region            string
	Country           string
	CountryCode       string
	Continent         string
	ContinentCode     string
	LocationId        server.Id
	CityLocationId    server.Id
	RegionLocationId  server.Id
	CountryLocationId server.Id
	Latitude          float64
	Longitude         float64
	Timezone          string
}

func (self *Location) GuessLocationType() (LocationType, error) {
	if self.City != "" {
		return LocationTypeCity, nil
	}
	if self.Region != "" {
		return LocationTypeRegion, nil
	}
	if self.CountryCode != "" {
		return LocationTypeCountry, nil
	}
	return "", fmt.Errorf("Unknown location type.")
}

func (self *Location) SearchStrings() []string {
	switch self.LocationType {
	case LocationTypeCity:
		return []string{
			fmt.Sprintf("%s, %s", self.City, self.Country),
			fmt.Sprintf("%s (%s)", self.City, self.CountryCode),
			fmt.Sprintf("%s, %s", self.City, self.Region),
		}
	case LocationTypeRegion:
		return []string{
			fmt.Sprintf("%s, %s", self.Region, self.Country),
			fmt.Sprintf("%s (%s)", self.Region, self.CountryCode),
		}
	default:
		return []string{
			fmt.Sprintf("%s (%s)", self.Country, self.CountryCode),
			fmt.Sprintf("%s", self.CountryCode),
		}
	}
}

func (self *Location) CountryLocation() (*Location, error) {
	return &Location{
		LocationType:      LocationTypeCountry,
		Country:           self.Country,
		CountryCode:       self.CountryCode,
		LocationId:        self.CountryLocationId,
		CountryLocationId: self.CountryLocationId,
	}, nil
}

func (self *Location) RegionLocation() (*Location, error) {
	switch self.LocationType {
	case LocationTypeCity, LocationTypeRegion:
		return &Location{
			LocationType:      LocationTypeRegion,
			Region:            self.Region,
			Country:           self.Country,
			CountryCode:       self.CountryCode,
			LocationId:        self.RegionLocationId,
			RegionLocationId:  self.RegionLocationId,
			CountryLocationId: self.CountryLocationId,
		}, nil
	default:
		return nil, fmt.Errorf("Cannot get region from %s.", self.LocationType)
	}
}

func (self *Location) CityLocation() (*Location, error) {
	switch self.LocationType {
	case LocationTypeCity:
		return &Location{
			LocationType:      LocationTypeCity,
			City:              self.City,
			Region:            self.Region,
			Country:           self.Country,
			CountryCode:       self.CountryCode,
			LocationId:        self.CityLocationId,
			CityLocationId:    self.CityLocationId,
			RegionLocationId:  self.RegionLocationId,
			CountryLocationId: self.CountryLocationId,
		}, nil
	default:
		return nil, fmt.Errorf("Cannot get city from %s.", self.LocationType)
	}
}

// resolveCountryName resolves the display name of an ISO-3166-1 alpha-2
// country code.
//
// The order matters. The deployment's own `iso-country-list.yml` wins wherever
// it has an entry, because some deployments deliberately use their own naming
// ("South Korea", not "Korea, Republic of"). The built-in ISO table is the
// fallback for the codes that file omits -- which on the live beta deployment
// is 191 of the 249 assigned codes.
//
// The fallback lives in Go rather than in config on purpose:
// `iso-country-list.yml` is a per-deployment vault resource and only one
// deployment's copy is in this repo, so a config-only fix would repair that one
// deployment and leave every other one inserting blank names.
//
// ok is false when the code is in neither, which means it is not a country.
// Callers must NOT substitute the code for the name -- a location row named
// "cn" is the same bug wearing a different hat, just quieter.
func resolveCountryName(countryCode string) (string, bool) {
	code := strings.ToLower(countryCode)
	if len(code) != 2 {
		return "", false
	}

	// unlike `AddDefaultLocations`, this does not *require* the config
	// resource: `CreateLocation` is also a runtime path, and a deployment
	// without the file must fall through to the Go table rather than panic.
	if resource, err := server.Config.SimpleResource("iso-country-list.yml"); err == nil {
		// the file is keyed by upper case code (`AE: United Arab Emirates`)
		for configCode, configName := range resource.Parse() {
			if !strings.EqualFold(configCode, code) {
				continue
			}
			if name, ok := configName.(string); ok && name != "" {
				return name, true
			}
		}
	}

	return ISOCountryName(code)
}

func CreateLocation(ctx context.Context, location *Location) {
	var countryCode string
	if location.CountryCode != "" {
		countryCode = strings.ToLower(location.CountryCode)
	} else {
		// use the country name
		countryCode = strings.ToLower(string([]rune(location.Country)))
	}
	if 2 < len(countryCode) {
		countryCode = countryCode[0:2]
	}

	// No location row may be inserted with an empty `location_name`. Each
	// insert below writes the name straight from the field the row is named
	// after -- `Country`, `Region`, `City` -- so an empty field is an empty
	// name, and the `location_full_name` built from it comes out shaped like
	// ", hk". Both were observed on the live beta deployment.
	//
	// A country with no name is resolved from its code. Everything else
	// resolves to its nearest NAMED ancestor rather than erroring, because the
	// unnamed-region case is reached on the connect-announce hot path: mmdb
	// returns no subdivision for the subdivision-less countries (hk, sg, mc,
	// va, ...), `GuessLocationType` still classifies those as a city because
	// the city is set, and refusing the whole location there would turn a
	// cosmetically-bad row into a failed connection for every client in those
	// countries. Degrading loses city precision for them; it does not lose the
	// connection, and it never writes a blank name.
	switch location.LocationType {
	case LocationTypeCountry, LocationTypeRegion, LocationTypeCity:
	default:
		// the inserts below are selected by `LocationType`, and an unrecognized
		// one (including the zero value, from a `Location` built without the
		// field) falls through every early return and lands on the city insert
		// with whatever the caller left empty. That is a blank name by another
		// route, so it is refused here rather than resolved.
		glog.Errorf(
			"[loc]refusing to create a location: \"%s\" is not a known location type.\n",
			location.LocationType,
		)
		server.Raise(fmt.Errorf("Unknown location type \"%s\".", location.LocationType))
	}
	if location.Country == "" {
		country, ok := resolveCountryName(countryCode)
		if !ok {
			// not a country. There is no ancestor to fall back to, and
			// inventing a name (or reusing the code as one) is what made this
			// class of bug invisible in the first place.
			glog.Errorf(
				"[loc]refusing to create a location: \"%s\" is not a known country code.\n",
				countryCode,
			)
			server.Raise(fmt.Errorf("Unknown country code \"%s\".", countryCode))
		}
		location.Country = country
	}
	if location.LocationType == LocationTypeCity && location.City == "" {
		glog.Infof(
			"[loc]unnamed city in \"%s\"; resolving at region granularity.\n",
			countryCode,
		)
		location.LocationType = LocationTypeRegion
	}
	if location.LocationType != LocationTypeCountry && location.Region == "" {
		// a city row is keyed under a region row, so a city whose region has no
		// name cannot be created either
		glog.Infof(
			"[loc]unnamed region in \"%s\"; resolving at country granularity.\n",
			countryCode,
		)
		location.LocationType = LocationTypeCountry
	}

	// country
	server.Tx(ctx, func(tx server.PgTx) {
		var countryLocation *Location
		var regionLocation *Location
		var cityLocation *Location

		result, err := tx.Query(
			ctx,
			`
                SELECT
                    location_id
                FROM location
                WHERE
                    location_type = $1 AND
                    country_code = $2
            `,
			LocationTypeCountry,
			countryCode,
		)

		server.WithPgResult(result, err, func() {
			if result.Next() {
				var locationId server.Id
				server.Raise(result.Scan(&locationId))
				countryLocation = &Location{
					LocationType:      LocationTypeCountry,
					Country:           location.Country,
					CountryCode:       countryCode,
					LocationId:        locationId,
					CountryLocationId: locationId,
				}
			}
		})

		if countryLocation == nil {
			locationId := server.NewId()
			_, err = tx.Exec(
				ctx,
				`
                    INSERT INTO location (
                        location_id,
                        location_type,
                        location_name,
                        country_location_id,
                        country_code,
                        location_full_name
                    )
                    VALUES ($1, $2, $3, $1, $4, $5)
                `,
				locationId,
				LocationTypeCountry,
				location.Country,
				countryCode,
				countryCode,
			)
			server.Raise(err)

			countryLocation = &Location{
				LocationType:      LocationTypeCountry,
				Country:           location.Country,
				CountryCode:       countryCode,
				LocationId:        locationId,
				CountryLocationId: locationId,
			}

			// add to the search
			for i, searchStr := range countryLocation.SearchStrings() {
				locationSearch().AddInTx(ctx, searchStr, locationId, i, tx)
			}
		}

		if location.LocationType == LocationTypeCountry {
			*location = *countryLocation
			return
		}

		// A blank-name backfill can leave a legacy row alongside the canonical
		// region when both full names would otherwise collide. Prefer the row
		// that owns the normally-composed full name; the id tie-breaker keeps
		// selection deterministic when only legacy rows exist.
		result, err = tx.Query(
			ctx,
			`
                SELECT
                    location_id
                FROM location
                WHERE
                    location_type = $1 AND
                    country_code = $2 AND
                    location_name = $3 AND
                    country_location_id = $4
				ORDER BY
					(location_full_name = $5) DESC,
					location_id
				LIMIT 1
            `,
			LocationTypeRegion,
			countryCode,
			location.Region,
			countryLocation.LocationId,
			fmt.Sprintf("%s, %s", location.Region, countryCode),
		)

		server.WithPgResult(result, err, func() {
			if result.Next() {
				var locationId server.Id
				server.Raise(result.Scan(&locationId))
				regionLocation = &Location{
					LocationType:      LocationTypeRegion,
					Region:            location.Region,
					Country:           countryLocation.Country,
					CountryCode:       countryCode,
					LocationId:        locationId,
					RegionLocationId:  locationId,
					CountryLocationId: countryLocation.LocationId,
				}
			}
		})

		if regionLocation == nil {
			// create a new location

			locationId := server.NewId()

			_, err = tx.Exec(
				ctx,
				`
                    INSERT INTO location (
                        location_id,
                        location_type,
                        location_name,
                        region_location_id,
                        country_location_id,
                        country_code,
                        location_full_name
                    )
                    VALUES ($1, $2, $3, $1, $4, $5, $6)
                `,
				locationId,
				LocationTypeRegion,
				location.Region,
				countryLocation.LocationId,
				countryCode,
				fmt.Sprintf("%s, %s", location.Region, countryCode),
			)
			server.Raise(err)

			regionLocation = &Location{
				LocationType:      LocationTypeRegion,
				Region:            location.Region,
				Country:           countryLocation.Country,
				CountryCode:       countryCode,
				LocationId:        locationId,
				RegionLocationId:  locationId,
				CountryLocationId: countryLocation.LocationId,
			}

			// add to the search
			for i, searchStr := range regionLocation.SearchStrings() {
				locationSearch().AddInTx(ctx, searchStr, locationId, i, tx)
			}
		}

		if location.LocationType == LocationTypeRegion {
			*location = *regionLocation
			return
		}

		// A non-conflicting legacy city may have had its full name normalized
		// while retaining its legacy region id. The globally-unique full name is
		// therefore a safe fallback when the canonical region lookup above does
		// not find the city under that exact region id. Scan the stored region id
		// so the returned hierarchy remains internally consistent.
		result, err = tx.Query(
			ctx,
			`
                SELECT 
					location_id,
					region_location_id
                FROM location
                WHERE
                    location_type = $1 AND
                    country_code = $2 AND
                    location_name = $3 AND
					country_location_id = $5 AND
					(
						region_location_id = $4 OR
						location_full_name = $6
					)
				ORDER BY
					(region_location_id = $4) DESC,
					location_id
				LIMIT 1
            `,
			LocationTypeCity,
			countryCode,
			location.City,
			regionLocation.LocationId,
			countryLocation.LocationId,
			fmt.Sprintf("%s, %s, %s", location.City, location.Region, countryCode),
		)

		server.WithPgResult(result, err, func() {
			if result.Next() {
				var locationId server.Id
				var actualRegionLocationId server.Id
				server.Raise(result.Scan(&locationId, &actualRegionLocationId))
				cityLocation = &Location{
					LocationType:      LocationTypeCity,
					City:              location.City,
					Region:            regionLocation.Region,
					Country:           countryLocation.Country,
					CountryCode:       countryCode,
					LocationId:        locationId,
					CityLocationId:    locationId,
					RegionLocationId:  actualRegionLocationId,
					CountryLocationId: countryLocation.LocationId,
				}
			}
		})

		// the mmdb uses 0,0 for unknown coordinates, and a genuine 0,0 city is
		// effectively impossible, so 0,0 is stored as NULL (unknown)
		hasCoordinates := location.Latitude != 0 || location.Longitude != 0

		if cityLocation != nil {
			if hasCoordinates {
				// self-heal rows created before coordinates were stored
				_, err = tx.Exec(
					ctx,
					`
                    UPDATE location
                    SET
                        latitude = $2,
                        longitude = $3
                    WHERE
                        location_id = $1 AND
                        latitude IS NULL
                `,
					cityLocation.LocationId,
					location.Latitude,
					location.Longitude,
				)
				server.Raise(err)
			}
		} else {
			// create a new location

			locationId := server.NewId()

			var latitude *float64
			var longitude *float64
			if hasCoordinates {
				latitude = &location.Latitude
				longitude = &location.Longitude
			}

			_, err = tx.Exec(
				ctx,
				`
                    INSERT INTO location (
                        location_id,
                        location_type,
                        location_name,
                        city_location_id,
                        region_location_id,
                        country_location_id,
                        country_code,
                        location_full_name,
                        latitude,
                        longitude
                    )
                    VALUES ($1, $2, $3, $1, $4, $5, $6, $7, $8, $9)
                `,
				locationId,
				LocationTypeCity,
				location.City,
				regionLocation.LocationId,
				countryLocation.LocationId,
				countryCode,
				fmt.Sprintf("%s, %s, %s", location.City, location.Region, countryCode),
				latitude,
				longitude,
			)
			server.Raise(err)

			cityLocation = &Location{
				LocationType:      LocationTypeCity,
				City:              location.City,
				Region:            regionLocation.Region,
				Country:           countryLocation.Country,
				CountryCode:       countryLocation.CountryCode,
				LocationId:        locationId,
				CityLocationId:    locationId,
				RegionLocationId:  regionLocation.LocationId,
				CountryLocationId: countryLocation.LocationId,
			}

			// add to the search
			for i, searchStr := range cityLocation.SearchStrings() {
				locationSearch().AddInTx(ctx, searchStr, locationId, i, tx)
			}
		}

		*location = *cityLocation
	})
}

type LocationGroup struct {
	LocationGroupId   server.Id
	Name              string
	Promoted          bool
	MemberLocationIds []server.Id
}

func (self *LocationGroup) SearchStrings() []string {
	return []string{
		self.Name,
	}
}

func CreateLocationGroup(ctx context.Context, locationGroup *LocationGroup) {
	uniqueMemberLocationIds := map[server.Id]bool{}
	for _, memberLocationId := range locationGroup.MemberLocationIds {
		if uniqueMemberLocationIds[memberLocationId] {
			glog.Infof("[nclm]duplicate member[%s] found in group \"%s\". Ignoring.\n", memberLocationId, locationGroup.Name)
		}
		uniqueMemberLocationIds[memberLocationId] = true
	}
	server.Tx(ctx, func(tx server.PgTx) {
		ok := false
		var locationGroupId server.Id

		result, err := tx.Query(
			ctx,
			`
            SELECT location_group_id FROM location_group
            WHERE location_group_name = $1
            `,
			locationGroup.Name,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&locationGroupId))
				ok = true
			}
		})

		if ok {
			server.RaisePgResult(tx.Exec(
				ctx,
				`
	                UPDATE location_group
	                SET
	                	location_group_name = $2,
	                	promoted = $3
	                WHERE
	                	location_group_id = $1
	            `,
				locationGroupId,
				locationGroup.Name,
				locationGroup.Promoted,
			))

			server.RaisePgResult(tx.Exec(
				ctx,
				`
	            DELETE FROM location_group_member
	            WHERE 
	                location_group_id = $1
	            `,
				locationGroupId,
			))

		} else {
			locationGroupId = server.NewId()

			server.RaisePgResult(tx.Exec(
				ctx,
				`
	                INSERT INTO location_group (
	                    location_group_id,
	                    location_group_name,
	                    promoted
	                )
	                VALUES ($1, $2, $3)
	            `,
				locationGroupId,
				locationGroup.Name,
				locationGroup.Promoted,
			))
		}

		server.BatchInTx(ctx, tx, func(batch server.PgBatch) {
			for memberLocationId, _ := range uniqueMemberLocationIds {
				batch.Queue(
					`
                        INSERT INTO location_group_member (
                            location_group_id,
                            location_id
                        )
                        VALUES ($1, $2)
                    `,
					locationGroupId,
					memberLocationId,
				)
			}
		})

		locationGroup.LocationGroupId = locationGroupId

		for i, searchStr := range locationGroup.SearchStrings() {
			locationGroupSearch().AddInTx(ctx, searchStr, locationGroupId, i, tx)
		}
	})
}

func UpdateLocationGroup(ctx context.Context, locationGroup *LocationGroup) bool {
	success := false

	server.Tx(ctx, func(tx server.PgTx) {
		tag, err := tx.Exec(
			ctx,
			`
                UPDATE location_group
                SET
                    location_group_name = $2,
                    promoted = $3
                WHERE
                    location_group_id = $1
            `,
			locationGroup.LocationGroupId,
			locationGroup.Name,
			locationGroup.Promoted,
		)
		server.Raise(err)
		if tag.RowsAffected() != 1 {
			// does not exist
			return
		}

		tag, err = tx.Exec(
			ctx,
			`
                DELETE FROM location_group_member
                WHERE location_group_id = $1
            `,
			locationGroup.LocationGroupId,
		)
		server.Raise(err)

		server.BatchInTx(ctx, tx, func(batch server.PgBatch) {
			for _, locationId := range locationGroup.MemberLocationIds {
				batch.Queue(
					`
                        INSERT INTO location_group_member (
                            location_group_id,
                            location_id
                        )
                        VALUES ($1, $2)
                    `,
					locationGroup.LocationGroupId,
					locationId,
				)
			}
		})

		success = true
	})

	return success
}

type ConnectionLocationScores struct {
	NetTypeHosting int
	NetTypePrivacy int
	NetTypeVirtual int
	NetTypeForeign int
}

func SetConnectionLocation(
	ctx context.Context,
	connectionId server.Id,
	locationId server.Id,
	connectionLocationScores *ConnectionLocationScores,
) (returnErr error) {
	server.Tx(ctx, func(tx server.PgTx) {
		// note the network_id is allowed to be nil for a connection without an associated client
		result, err := tx.Query(
			ctx,
			`
                SELECT
                    network_client_connection.client_id,
                    network_client.network_id
                FROM network_client_connection 
                LEFT JOIN network_client ON network_client.client_id = network_client_connection.client_id
                WHERE network_client_connection.connection_id = $1
            `,
			connectionId,
		)
		var clientId *server.Id
		var networkId *server.Id
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(
					&clientId,
					&networkId,
				))
			}
		})

		if clientId == nil {
			returnErr = fmt.Errorf("Missing client connection.")
			return
		}

		result, err = tx.Query(
			ctx,
			`
                SELECT
                    location.city_location_id,
                    location.region_location_id,
                    location.country_location_id
                FROM location 
                WHERE location_id = $1
            `,
			locationId,
		)
		var cityLocationId *server.Id
		var regionLocationId *server.Id
		var countryLocationId *server.Id
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(
					&cityLocationId,
					&regionLocationId,
					&countryLocationId,
				))
			}
		})

		// fix(beta): the free-tier ipinfo geo db resolves many IPs --
		// datacenter, mobile, and VPN egress especially -- to country or
		// region granularity only, with no city. network_client_location
		// requires city_location_id and region_location_id NOT NULL, so a
		// country-only location's NULL city/region made this INSERT panic
		// inside server.Tx. That panic propagated out of the connection
		// announce goroutine (connect/transport_announce.go), whose
		// HandleError wrapper then cancelled the whole connection context --
		// tearing down every country-only client's connect connection right
		// after auth (the app itself included, whichever egress it resolved
		// to), and, because the panic hit before the disconnect-cleanup
		// defer was registered, orphaning the connection row as
		// connected=true forever. Fall back to the coarsest available
		// granularity so the columns are always non-null: a country-only
		// location stores its country id for city/region too, which keeps
		// the provider locatable at country level instead of crashing the
		// connection. If even the country id is missing (location row absent
		// or malformed), return a clean error so the caller's existing
		// graceful retry path handles it -- never panic here.
		if countryLocationId == nil {
			returnErr = fmt.Errorf("Location %s has no country granularity.", locationId)
			return
		}
		if cityLocationId == nil {
			cityLocationId = countryLocationId
		}
		if regionLocationId == nil {
			regionLocationId = countryLocationId
		}

		server.RaisePgResult(tx.Exec(
			ctx,
			`
                INSERT INTO network_client_location (
                    connection_id,
                    client_id,
                    city_location_id,
                    region_location_id,
                    country_location_id,
		            net_type_hosting,
		            net_type_privacy,
		            net_type_virtual,
		            net_type_foreign,
		            network_id
                )
                VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
                ON CONFLICT (connection_id) DO UPDATE
                SET
                    client_id = $2,
                    city_location_id = $3,
                    region_location_id = $4,
                    country_location_id = $5,
                    net_type_hosting = $6,
                    net_type_privacy = $7,
                    net_type_virtual = $8,
                    net_type_foreign = $9,
                    network_id = $10
            `,
			connectionId,
			clientId,
			cityLocationId,
			regionLocationId,
			countryLocationId,
			connectionLocationScores.NetTypeHosting,
			connectionLocationScores.NetTypePrivacy,
			connectionLocationScores.NetTypeVirtual,
			connectionLocationScores.NetTypeForeign,
			networkId,
		))
	})
	return
}

type LocationGroupResult struct {
	LocationGroupId server.Id `json:"location_group_id"`
	Name            string    `json:"name"`
	ProviderCount   int       `json:"provider_count,omitempty"`
	Promoted        bool      `json:"promoted,omitempty"`
	MatchDistance   int       `json:"match_distance,omitempty"`
}

type LocationResult struct {
	LocationId        server.Id    `json:"location_id"`
	LocationType      LocationType `json:"location_type"`
	Name              string       `json:"name"`
	City              string       `json:"city,omitempty"`
	Region            string       `json:"region,omitempty"`
	Country           string       `json:"country,omitempty"`
	CityLocationId    *server.Id   `json:"city_location_id,omitempty"`
	RegionLocationId  *server.Id   `json:"region_location_id,omitempty"`
	CountryLocationId *server.Id   `json:"country_location_id,omitempty"`
	CountryCode       string       `json:"country_code"`
	ProviderCount     int          `json:"provider_count,omitempty"`
	MatchDistance     int          `json:"match_distance,omitempty"`

	Stable        bool `json:"stable"`
	StrongPrivacy bool `json:"strong_privacy"`
}

// clientLocationName resolves a parent location id (city/region/country) to its
// display name from the location set, or "" if the parent is not present.
func clientLocationName(byId map[server.Id]*ClientLocation, id *server.Id) string {
	if id == nil {
		return ""
	}
	if cl, ok := byId[*id]; ok {
		return cl.Name
	}
	return ""
}

type LocationDeviceResult struct {
	ClientId   server.Id `json:"client_id"`
	DeviceName string    `json:"device_name"`
}

type FindLocationsArgs struct {
	Query string `json:"query"`
	// the max search distance is `MaxDistanceFraction * len(Query)`
	// in other words `len(Query) * (1 - MaxDistanceFraction)` length the query must match
	MaxDistanceFraction       float32 `json:"max_distance_fraction,omitempty"`
	EnableMaxDistanceFraction bool    `json:"enable_max_distance_fraction,omitempty"`
	RankMode                  string  `json:"rank_mode"`
}

type FindLocationsResult struct {
	// this includes groups that show up in the location results
	// all `ProviderCount` are from inside the location results
	// groups are suggestions that can be used to broaden the search
	Groups []*LocationGroupResult `json:"groups"`
	// this includes all parent locations that show up in the location results
	// every `CityId`, `RegionId`, `CountryId` will have an entry
	Locations []*LocationResult `json:"locations"`
	// direct devices
	Devices []*LocationDeviceResult `json:"devices"`

	// location stats
	CountryCount       int `json:"country_count"`
	RegionCount        int `json:"region_count"`
	CityCount          int `json:"city_count"`
	StableCount        int `json:"stable_count"`
	StrongPrivacyCount int `json:"strong_privacy_count"`
}

func (self *FindLocationsResult) SetStats() {
	countryCount := 0
	regionCount := 0
	cityCount := 0
	stableCount := 0
	strongPrivacyCount := 0

	for _, location := range self.Locations {
		switch location.LocationType {
		case LocationTypeCountry:
			countryCount += 1
		case LocationTypeRegion:
			regionCount += 1
		case LocationTypeCity:
			cityCount += 1
		}
		if location.Stable {
			stableCount += 1
		}
		if location.StrongPrivacy {
			strongPrivacyCount += 1
		}
	}

	self.CountryCount = countryCount
	self.RegionCount = regionCount
	self.CityCount = cityCount
	self.StableCount = stableCount
	self.StrongPrivacyCount = strongPrivacyCount
}

// used for debugging
func SearchLocations(ctx context.Context, query string, distance int) []*search.SearchResult {
	s := locationSearch()
	s.WaitForInitialSync(ctx)

	startTime := time.Now()
	r := s.Around(
		ctx,
		query,
		distance,
		search.OptMostLikley(10),
	)
	endTime := time.Now()
	glog.Infof("Search took %.2fms\n", float64(endTime.Sub(startTime)/time.Microsecond)/1000.0)

	return r
}

type ClientLocation struct {
	LocationId  server.Id
	ClientCount int

	Name              string
	LocationType      LocationType
	CityLocationId    *server.Id
	RegionLocationId  *server.Id
	CountryLocationId *server.Id
	CountryCode       string

	// location id -> client count
	TopCityLocationIdCounts map[server.Id]int
	// location id -> client count
	TopRegionLocationIdCounts map[server.Id]int

	StrongPrivacy bool
}

type ClientLocationGroup struct {
	LocationGroupId server.Id

	Name     string
	Promoted bool
}

type InitialClientLocations struct {
	Locations      []*ClientLocation
	LocationGroups []*ClientLocationGroup
}

// the hash tag is per location so that the family spreads across cluster slots.
// a single shared tag would concentrate the entire cache on one node.
func clientLocationKey(locationId server.Id) string {
	return fmt.Sprintf("{cl_%s}l", locationId)
}

func initialClientLocationsKey() string {
	return fmt.Sprintf("{cl}i")
}

// returns the given ids with nils dropped and duplicates removed, preserving
// order.
//
// A client's city/region/country location ids are not guaranteed to differ: a
// country-only geo resolution stores the country id in all three columns and a
// region-only one stores it in two, because SetConnectionLocation falls back to
// the coarsest granularity available rather than writing a NULL into the NOT
// NULL city/region columns. Anything that fans a client out across the three
// columns has to treat them as a set, or the client is counted once per column
// instead of once per location it is in.
//
// Scope: that coarsest-granularity fallback is a beta change. upstream/main has
// no country-only fallback -- it passes the NULL through and the insert raises
// -- so on main today no row with city = region = country exists and this
// dedupe changes nothing there. It becomes load-bearing upstream when the
// fallback lands with PR #407, which carries its own copy of this helper.
func distinctIds(ids ...*server.Id) []server.Id {
	distinct := make([]server.Id, 0, len(ids))
	for _, id := range ids {
		if id == nil {
			continue
		}
		if !slices.Contains(distinct, *id) {
			distinct = append(distinct, *id)
		}
	}
	return distinct
}

// The share of destinations a provider must reach to count as healthy: 90%,
// as 9/10. Compared exactly as `10*ok >= 9*total` rather than through a float
// division, so the boundary is the same for every denominator.
//
// 90% because it cleanly separates working from broken on the real
// population: the healthy fleet measures 129-131 of 131 destinations, while
// a dead proxy measures 0 of 131. Nothing observed sits near the line, so
// the exact figure is not load bearing -- it only has to be far above 0 and
// below the ~98% a genuinely working provider always clears.
//
// Package scope, not function scope: both UpdateClientScores and
// UpdateClientLocations gate on this now, and two copies could drift.
const minEgressHealthOKNumerator = 9
const minEgressHealthOKDenominator = 10

const providerConfigResourceName = "provider.yml"

// providerEgressTestEnabled controls whether egress-test evidence is an
// eligibility requirement. The probe pipeline can still run while this is
// false; its results simply do not gate provider discovery or public counts.
//
// Defaulting to false is deliberate. A deployment can introduce the server
// side of the probe pipeline before any prober has populated its tables. In
// that state, treating every missing measurement as an individual failure
// publishes an empty FindProviders2 cache even though the connected provider
// fleet is healthy.
func providerEgressTestEnabled() bool {
	return providerEgressTestEnabledFromResource(
		server.Config.SimpleResource(providerConfigResourceName),
	)
}

func providerEgressTestEnabledFromResource(resource *server.SimpleResource, err error) bool {
	if err != nil || resource == nil {
		return false
	}
	enabled := resource.Bool("enable_egress_test")
	return len(enabled) == 1 && enabled[0]
}

// providerCountFilter answers one question: does this provider count as real,
// reachable supply?
//
// It exists so the advertised provider_count (UpdateClientLocations) and the
// gated membership (UpdateClientScores) apply an IDENTICAL predicate. They ran
// different rules before: membership was gated on egress health while the count
// was not, so a location could survive the gate and still advertise providers
// that no probe had ever reached.
//
// Both maps are loaded once per pass. These loops run over the entire provider
// population, so a per-provider query here is one round trip per provider.
type providerCountFilter struct {
	healthCounts map[server.Id]ProviderEgressHealthCounts
	countryCodes map[server.Id]string
	// blackholed is the FAILING set only, not a verdict for every provider:
	// absent means "no current evidence this provider is dark", which covers
	// both a passing check and no check at all. See
	// GetAllProviderBlackholedClientIds for why it fails in that direction.
	blackholed map[server.Id]bool
}

func newProviderCountFilter(ctx context.Context) providerCountFilter {
	return providerCountFilter{
		healthCounts: GetAllProviderEgressHealthCounts(ctx),
		countryCodes: GetAllProviderEgressCountryCodes(ctx),
		blackholed:   GetAllProviderBlackholedClientIds(ctx),
	}
}

// passesHealth reports whether a probe has MEASURED this provider healthy.
// Fail closed: no record at all (never probed) does not pass, and neither does
// a record with no destinations in it, which is not a measurement of anything.
// Guarding total also keeps the ratio well defined.
//
// Compared exactly as 10*ok >= 9*total rather than through a float, so the 90%
// boundary cannot drift with rounding.
func (f providerCountFilter) passesHealth(clientId server.Id) bool {
	// The hourly blackhole check overrides a passing health measurement, and
	// only ever in the removing direction. The two run on very different
	// cadences -- health sweeps the fleet over hours to days, the blackhole
	// check over an hour -- so a provider that went dark since its last health
	// measurement is caught here rather than at the next health sweep. A
	// provider is only in this set when a CURRENT check says nothing got
	// through; see GetAllProviderBlackholedClientIds.
	if f.blackholed[clientId] {
		return false
	}

	counts, ok := f.healthCounts[clientId]
	if !ok {
		return false
	}
	if counts.Total <= 0 {
		return false
	}
	return minEgressHealthOKDenominator*counts.OKCount >= minEgressHealthOKNumerator*counts.Total
}

// countsTowardCountry reports whether this provider counts as supply for
// countryCode. It must both be measured healthy and have been OBSERVED
// egressing from that country.
//
// The two locations are different claims. network_client_location is where the
// provider says it is, derived from its own connection. provider_egress_location
// is where a probe actually watched its traffic leave. Counting on the claim
// alone advertises providers in countries they do not egress from -- measured
// on beta at 3 of 152 healthy providers claiming `at` while egressing from `gb`
// -- which is what an adversarial provider would exploit at scale.
//
// A provider with no observed location is not counted, matching the health rule.
func (f providerCountFilter) countsTowardCountry(clientId server.Id, countryCode string) bool {
	if !f.passesHealth(clientId) {
		return false
	}
	observed, ok := f.countryCodes[clientId]
	if !ok {
		return false
	}
	return observed == strings.ToLower(countryCode)
}

// shouldSkipCountGate reports whether the count gate should be skipped
// entirely for this pass, falling back to the pre-gate behavior (connected +
// valid + Public key only) instead of fail-closed per provider.
//
// Both maps are checked, not just healthCounts, because they are fed by two
// INDEPENDENT pipelines that can stall separately: health arrives over the
// external push endpoint (api/handlers/provider_egress_health_handlers.go),
// while the observed egress location comes from a separate internal job
// (controller/provider_egress_location_controller.go). If only the health
// pipeline stalls (or vice versa), the healthy-but-unlocated -- or
// located-but-unhealthy -- provider still fails closed in countsTowardCountry
// and locationClientCounts still empties fleet-wide, which is exactly the
// wiped-list failure this gate exists to prevent. Do NOT collapse this back
// to a single condition: either map being empty is "we know nothing from that
// pipeline", which must not be treated as "everything failed."
func (f providerCountFilter) shouldSkipCountGate() bool {
	return len(f.healthCounts) == 0 || len(f.countryCodes) == 0
}

// shouldRecountUngated is the SECOND half of the fleet-wide floor, applied
// after a gated counting pass instead of before it: gated says the gate was
// actually applied to this pass, providerRows is how many connected + valid +
// Public rows the count query returned, and countedLocations is how many
// locations came out of it with any supply at all.
//
// This is NOT redundant with shouldSkipCountGate, and a future reader must not
// collapse the two. They answer different questions and neither implies the
// other:
//
//   - shouldSkipCountGate asks "did either probe pipeline produce ANY rows at
//     all". It reads the two input maps.
//   - this asks "did rows that exist produce ANY counted supply". It reads the
//     OUTPUT of the pass.
//
// Non-empty inputs can still yield an empty output, by more than one route: a
// fleet-wide mismatch between claimed and observed countries, a location-table
// anomaly that makes every claimed country NULL (see the countryCode == nil
// branch below), a partially drained egress-location table whose surviving rows
// all belong to churned clients, or any future gate term added to
// countsTowardCountry. In every one of those, the input maps are non-empty so
// shouldSkipCountGate stays false, and yet locationClientCounts comes out
// empty.
//
// An empty locationClientCounts is not a benign "no supply" result: every
// location then misses the lookup below and lands in removeClientLocations,
// which DELs every clientLocationKey from redis and publishes an empty
// initialClientLocations -- /network/provider-locations returns nothing to
// every app. Treat "rows existed but nothing counted" as "this pass learned
// nothing" and redo it with the gate off, which is the same fallback
// shouldSkipCountGate selects.
//
// providerRows > 0 is what separates this from a genuinely empty fleet. If the
// count query returned no rows at all, there really is no connected + valid +
// Public supply and emptying the published list is the correct answer.
func shouldRecountUngated(gated bool, providerRows int, countedLocations int) bool {
	return gated && providerRows > 0 && countedLocations == 0
}

// providerCountRow is one connected + valid + Public provider row from the
// count query, held in memory so the pass can be counted twice (gated, then
// ungated if the gated pass came out empty) without issuing a second query.
type providerCountRow struct {
	clientId          server.Id
	cityLocationId    server.Id
	regionLocationId  server.Id
	countryLocationId server.Id
	// the country the provider CLAIMS. nil when the claimed country has no
	// `location` row to resolve it against.
	claimedCountryCode *string
}

func UpdateClientLocations(ctx context.Context, ttl time.Duration) (returnErr error) {
	topCitiesPerRegion := 20
	topCitiesPerCountry := 10
	topRegionsPerCountry := 10

	clientLocations := map[server.Id]*ClientLocation{}
	removeClientLocations := map[server.Id]bool{}

	initialClientLocations := &InitialClientLocations{}

	// one bulk load per pass, outside the tx: this loop runs over the whole
	// provider population. Do not query the probe tables when their result is
	// not an eligibility requirement.
	egressTestEnabled := providerEgressTestEnabled()
	countFilter := providerCountFilter{}
	if egressTestEnabled {
		countFilter = newProviderCountFilter(ctx)
	}

	// An empty health OR countryCodes map means one of the two probe
	// pipelines has told us nothing yet -- stalled job, truncated table, cold
	// environment -- NOT "every provider measured unhealthy" or "every
	// provider is mislocated". Per-provider fail-closed (an individual
	// provider with no record does not count) is the intended behavior; but
	// applying it fleet-wide when an entire pipeline has produced zero rows
	// would empty locationClientCounts entirely, which sends every single
	// location through removeClientLocations below and DELs every key from
	// redis -- wiping the whole public provider list because one prober
	// died, not because supply is actually gone. That is a different, worse
	// failure mode than the one this gate exists to fix, so skip the gate
	// for this pass and count as before (connected + valid + Public key
	// only) instead. See shouldSkipCountGate for why BOTH maps are checked.
	// Do NOT remove this as "redundant" with passesHealth's per-provider
	// check -- it is a fleet-wide floor, not a per-provider one.
	//
	// This is only the input-side half of that floor: empty inputs are not the
	// only way to reach an emptied count. See shouldRecountUngated, applied to
	// the counted result below, for the other half.
	skipCountGate := !egressTestEnabled || countFilter.shouldSkipCountGate()
	if skipCountGate {
		if egressTestEnabled {
			glog.Infof("[nclm]egress health or location records are empty; skipping the provider count gate for this pass\n")
		} else {
			glog.Infof("[nclm]provider egress test is disabled; skipping the provider count gate for this pass\n")
		}
	}

	server.Tx(ctx, func(tx server.PgTx) {

		providerCountRows := []providerCountRow{}

		result, err := tx.Query(
			ctx,
			`
	        SELECT
	        	network_client_location_reliability.client_id,
	        	network_client_location_reliability.city_location_id,
	        	network_client_location_reliability.region_location_id,
	        	network_client_location_reliability.country_location_id,
	        	-- the country the provider CLAIMS, to check against the country a
	        	-- probe observed it egressing from
	        	country_location.country_code

	        FROM network_client_location_reliability

	        -- fix(beta): this was an INNER JOIN upstream, which requires a
	        -- client to already have a row in client_connection_reliability_score
	        -- (populated by a separate multi-stage rollup: raw events -> redis
	        -- drain -> client_reliability_running -> reliability scores) before
	        -- it counts toward any provider location at all. At small/cold-start
	        -- scale (this self-contained beta env) that rollup chain can go
	        -- indefinitely without producing a single row even though real,
	        -- currently-connected/valid clients exist -- the INNER JOIN then
	        -- discards every one of them, and /network/provider-locations comes
	        -- back completely empty despite real providers being connected.
	        -- LEFT JOIN counts a location from connected+valid alone, which is
	        -- real, already-verified data (see SetConnectionLocation), without
	        -- waiting on the reliability-scoring pipeline to catch up.
	        LEFT JOIN client_connection_reliability_score ON
	        	client_connection_reliability_score.client_id = network_client_location_reliability.client_id AND
				client_connection_reliability_score.lookback_index = 0

	        LEFT JOIN location AS country_location ON
	        	country_location.location_id = network_client_location_reliability.country_location_id

	        WHERE
	        	network_client_location_reliability.connected = true AND
	        	network_client_location_reliability.valid = true AND
	        	-- this is the number shown to everyone, so count only providers
	        	-- a stranger can actually reach. GetProvideRelationship returns
	        	-- ProvideModePublic for a cross-network pair, so a Public
	        	-- provide key is what makes a provider generally reachable;
	        	-- without one it advertises supply nobody outside its own
	        	-- network can use.
	        	--
	        	-- Note this is deliberately narrower than the candidate pool
	        	-- UpdateClientScores builds. That pool also carries
	        	-- ProvideModeNetwork providers, which are real usable supply
	        	-- for sources inside their own network, and FindProviders2
	        	-- filters them per request against the caller's network. They
	        	-- do not belong in a public count.
	        	--
	        	-- Every other mode is excluded, not just Stream. In particular
	        	-- resolveNonCompanionProvideMode
	        	-- (controller/connect_controller.go) lets a Stream-only
	        	-- destination be resolved as a *companion* stream, but that
	        	-- dead-ends at CreateCompanionTransferEscrow, which requires a
	        	-- pre-existing reverse-direction origin contract -- so a
	        	-- Stream-only destination can never bootstrap a session and is
	        	-- correctly absent from both the count and the pool.
	        	EXISTS (
	        		SELECT 1 FROM provide_key
	        		WHERE
	        			provide_key.client_id = network_client_location_reliability.client_id AND
	        			provide_key.provide_mode = $1
	        	)
	        `,
			ProvideModePublic,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				// declared per iteration on purpose: claimedCountryCode is a
				// pointer, and hoisting these out of the loop would make every
				// retained row alias the last scanned value.
				var row providerCountRow
				server.Raise(result.Scan(
					&row.clientId,
					&row.cityLocationId,
					&row.regionLocationId,
					&row.countryLocationId,
					&row.claimedCountryCode,
				))
				providerCountRows = append(providerCountRows, row)
			}
		})

		// counted from the retained rows rather than inline in the scan loop, so
		// the same pass can be counted a second time with the gate off without
		// re-querying. See shouldRecountUngated.
		countProviderRows := func(gated bool) map[server.Id]int {
			locationClientCounts := map[server.Id]int{}
			for _, row := range providerCountRows {
				// This is the number every app shows when a user picks a
				// location, so count only providers a probe has MEASURED
				// healthy and OBSERVED egressing from the country they claim.
				// Counting on the claim alone advertised providers that were
				// either unreachable or in a different country entirely.
				//
				// claimedCountryCode is NULL when the claimed country has no
				// location row, which cannot be verified against anything --
				// fail closed, same as an unobserved provider.
				//
				// Unless the gate is off for this pass (see skipCountGate
				// above and shouldRecountUngated below) -- an unprobed fleet is
				// "unknown", not "unhealthy", and must not empty the public
				// list.
				if gated {
					if row.claimedCountryCode == nil {
						continue
					}
					if !countFilter.countsTowardCountry(row.clientId, *row.claimedCountryCode) {
						continue
					}
				}

				// count each client at most once per distinct location id. A
				// client whose geo lookup resolved neither a city nor a region
				// is stored with city = region = country (see
				// SetConnectionLocation's country fallback), so incrementing
				// all three unconditionally counted that one client three
				// times in its own country. Distinct ids -- a real
				// city-granular client -- still roll up into their region and
				// country exactly as before.
				//
				// This is a live fix on beta, where that fallback exists, and a
				// forward guard against upstream/main, where it does not yet --
				// see distinctIds.
				for _, locationId := range distinctIds(
					&row.cityLocationId,
					&row.regionLocationId,
					&row.countryLocationId,
				) {
					locationClientCounts[locationId] += 1
				}
			}
			return locationClientCounts
		}

		gated := !skipCountGate
		locationClientCounts := countProviderRows(gated)

		// the output-side half of the fleet-wide floor. shouldSkipCountGate
		// guards the INPUTS (did a probe pipeline produce rows); this guards
		// the OUTPUT (did those rows produce any counted supply). Neither
		// implies the other -- see shouldRecountUngated for why they must not
		// be collapsed.
		if shouldRecountUngated(gated, len(providerCountRows), len(locationClientCounts)) {
			glog.Infof(
				"[nclm]the count gate emptied all %d connected provider rows fleet-wide; recounting ungated for this pass\n",
				len(providerCountRows),
			)
			locationClientCounts = countProviderRows(false)
		}

		server.CreateTempTableInTx(
			ctx,
			tx,
			"temp_location_ids(location_id uuid)",
			slices.Collect(maps.Keys(locationClientCounts))...,
		)

		result, err = tx.Query(
			ctx,
			`
                SELECT
                    location_id,
                    location_type,
                    location_name,
                    city_location_id,
                    region_location_id,
                    country_location_id,
                    country_code
                FROM location
            `,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				clientLocation := &ClientLocation{
					TopCityLocationIdCounts:   map[server.Id]int{},
					TopRegionLocationIdCounts: map[server.Id]int{},
				}
				server.Raise(result.Scan(
					&clientLocation.LocationId,
					&clientLocation.LocationType,
					&clientLocation.Name,
					&clientLocation.CityLocationId,
					&clientLocation.RegionLocationId,
					&clientLocation.CountryLocationId,
					&clientLocation.CountryCode,
				))
				if clientCount, ok := locationClientCounts[clientLocation.LocationId]; ok {
					clientLocation.ClientCount = clientCount
					clientLocations[clientLocation.LocationId] = clientLocation

					if clientLocation.LocationType == LocationTypeCountry {
						initialClientLocations.Locations = append(initialClientLocations.Locations, clientLocation)
					}
				} else {
					removeClientLocations[clientLocation.LocationId] = true
				}
			}
		})

		// create top links
		for locationId, clientLocation := range clientLocations {
			switch clientLocation.LocationType {
			case LocationTypeCity:
				regionClientLocation := clientLocations[*(clientLocation.RegionLocationId)]
				regionClientLocation.TopCityLocationIdCounts[locationId] = clientLocation.ClientCount

				countryClientLocation := clientLocations[*(clientLocation.CountryLocationId)]
				countryClientLocation.TopCityLocationIdCounts[locationId] = clientLocation.ClientCount
			case LocationTypeRegion:
				countryClientLocation := clientLocations[*(clientLocation.CountryLocationId)]
				countryClientLocation.TopRegionLocationIdCounts[locationId] = clientLocation.ClientCount
			}
		}
		filterTop := func(locationIdCounts map[server.Id]int, n int) map[server.Id]int {
			locationIds := slices.Collect(maps.Keys(locationIdCounts))
			slices.SortFunc(locationIds, func(a server.Id, b server.Id) int {
				d := locationIdCounts[b] - locationIdCounts[a]
				if d != 0 {
					return d
				}
				return a.Cmp(b)
			})
			filteredLocationIdCounts := map[server.Id]int{}
			for _, locationId := range locationIds[:min(n, len(locationIds))] {
				filteredLocationIdCounts[locationId] = locationIdCounts[locationId]
			}
			return filteredLocationIdCounts
		}
		for _, clientLocation := range clientLocations {
			switch clientLocation.LocationType {
			case LocationTypeRegion:
				clientLocation.TopCityLocationIdCounts = filterTop(clientLocation.TopCityLocationIdCounts, topCitiesPerRegion)
			case LocationTypeCountry:
				clientLocation.TopCityLocationIdCounts = filterTop(clientLocation.TopCityLocationIdCounts, topCitiesPerCountry)
				clientLocation.TopRegionLocationIdCounts = filterTop(clientLocation.TopRegionLocationIdCounts, topRegionsPerCountry)
			}
		}

		// fill in strong privacy flag based on membership in the `StrongPrivacyLaws` group
		// strong privacy is transitive to all sub-locations
		result, err = tx.Query(
			ctx,
			`
                SELECT
                    location_group_member.location_id
                FROM location_group_member

                INNER JOIN location_group ON
                	location_group.location_group_id = location_group_member.location_group_id AND
                	location_group.location_group_name = $1
            `,
			StrongPrivacyLaws,
		)
		strongPrivacyLocations := map[server.Id]bool{}
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var locationId server.Id
				server.Raise(result.Scan(&locationId))
				strongPrivacyLocations[locationId] = true
			}
		})
		for _, clientLocation := range clientLocations {
			strongPrivacy := false
			if clientLocation.CountryLocationId != nil {
				if strongPrivacyLocations[*clientLocation.CountryLocationId] {
					strongPrivacy = true
				}
			}
			if clientLocation.RegionLocationId != nil {
				if strongPrivacyLocations[*clientLocation.RegionLocationId] {
					strongPrivacy = true
				}
			}
			if clientLocation.CityLocationId != nil {
				if strongPrivacyLocations[*clientLocation.CityLocationId] {
					strongPrivacy = true
				}
			}
			clientLocation.StrongPrivacy = strongPrivacy
		}

		result, err = tx.Query(
			ctx,
			`
                SELECT
                    location_group_id,
                    location_group_name,
                    promoted
                FROM location_group
                WHERE promoted = true
            `,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				clientLocationGroup := &ClientLocationGroup{}
				server.Raise(result.Scan(
					&clientLocationGroup.LocationGroupId,
					&clientLocationGroup.Name,
					&clientLocationGroup.Promoted,
				))
				initialClientLocations.LocationGroups = append(initialClientLocations.LocationGroups, clientLocationGroup)
			}
		})
	})

	server.Redis(ctx, func(r server.RedisClient) {
		// plain pipeline instead of tx: the sets are independent and the keys
		// hash to different cluster slots, which multi/exec cannot span
		pipe := r.Pipeline()

		for locationId, clientLocation := range clientLocations {
			b := bytes.NewBuffer(nil)
			e := gob.NewEncoder(b)
			e.Encode(clientLocation)
			clientLocationBytes := b.Bytes()

			pipe.Set(ctx, clientLocationKey(locationId), clientLocationBytes, ttl)
			glog.V(2).Infof("[nclm]update client location (%s)\n", locationId)
		}
		for locationId, _ := range removeClientLocations {
			pipe.Del(ctx, clientLocationKey(locationId))
			glog.V(2).Infof("[nclm]remove client location (%s)\n", locationId)
		}

		b := bytes.NewBuffer(nil)
		e := gob.NewEncoder(b)
		e.Encode(initialClientLocations)
		initialClientLocationsBytes := b.Bytes()
		pipe.Set(ctx, initialClientLocationsKey(), initialClientLocationsBytes, ttl)
		glog.V(2).Infof("[nclm]update initial client locations\n")

		_, returnErr = pipe.Exec(ctx)
		if returnErr != nil {
			return
		}
	})

	glog.Infof("[nclm]updated %d client locations, removed %d, and updated initial\n", len(clientLocations), len(removeClientLocations))

	return
}

func loadClientLocations(
	ctx context.Context,
	locationIds map[server.Id]bool,
) (clientLocations map[server.Id]*ClientLocation, returnErr error) {
	server.Redis(ctx, func(r server.RedisClient) {
		load := func(locationIds map[server.Id]bool, clientLocations map[server.Id]*ClientLocation) error {
			clientLocationCmds := map[server.Id]*redis.StringCmd{}

			// plain pipeline instead of tx: independent gets across cluster slots
			pipe := r.Pipeline()
			for locationId, _ := range locationIds {
				v := pipe.Get(ctx, clientLocationKey(locationId))
				clientLocationCmds[locationId] = v
			}
			// note ignore the error for GET since it will include missing key
			pipe.Exec(ctx)

			for locationId, clientLocationCmd := range clientLocationCmds {
				clientLocationBytes, _ := clientLocationCmd.Bytes()
				if len(clientLocationBytes) == 0 {
					continue
				}
				b := bytes.NewBuffer(clientLocationBytes)
				e := gob.NewDecoder(b)
				var clientLocation ClientLocation
				err := e.Decode(&clientLocation)
				if err != nil {
					return err
				}

				clientLocations[locationId] = &clientLocation
			}

			return nil
		}

		clientLocations = map[server.Id]*ClientLocation{}

		returnErr = load(locationIds, clientLocations)
		if returnErr != nil {
			return
		}

		expandedLocationIds := map[server.Id]bool{}

		for _, clientLocation := range clientLocations {
			if clientLocation.CityLocationId != nil {
				_, ok := locationIds[*clientLocation.CityLocationId]
				if !ok {
					expandedLocationIds[*clientLocation.CityLocationId] = true
				}
			}
			if clientLocation.RegionLocationId != nil {
				_, ok := locationIds[*clientLocation.RegionLocationId]
				if !ok {
					expandedLocationIds[*clientLocation.RegionLocationId] = true
				}
			}
			if clientLocation.CountryLocationId != nil {
				_, ok := locationIds[*clientLocation.CountryLocationId]
				if !ok {
					expandedLocationIds[*clientLocation.CountryLocationId] = true
				}
			}

			for locationId, _ := range clientLocation.TopCityLocationIdCounts {
				expandedLocationIds[locationId] = true
			}
			for locationId, _ := range clientLocation.TopRegionLocationIdCounts {
				expandedLocationIds[locationId] = true
			}
		}

		returnErr = load(expandedLocationIds, clientLocations)
		if returnErr != nil {
			return
		}
	})

	return
}

func loadInitialClientLocations(ctx context.Context) (initialClientLocations *InitialClientLocations, returnErr error) {
	server.Redis(ctx, func(r server.RedisClient) {

		cmd := r.Get(ctx, initialClientLocationsKey())

		initialClientLocationsBytes, _ := cmd.Bytes()
		if len(initialClientLocationsBytes) == 0 {
			return
		}
		b := bytes.NewBuffer(initialClientLocationsBytes)
		e := gob.NewDecoder(b)
		var initialClientLocations_ InitialClientLocations
		returnErr = e.Decode(&initialClientLocations_)
		if returnErr != nil {
			return
		}

		initialClientLocations = &initialClientLocations_
	})
	return
}

// a missing entry means the location has no providers
func loadLocationStables(
	ctx context.Context,
	locationIds []server.Id,
	// forceMinimum selects which pre-computed key family to read. The writer
	// (UpdateClientScores) populates both, so this only chooses between them.
	// User-facing listing passes false and keeps today's behaviour; an operator
	// census passes true, because a location where every provider fails the
	// minimums gate is otherwise invisible and its providers can never be
	// probed or graduate probation.
	forceMinimum bool,
	rankMode RankMode,
	clientLocationId server.Id,
) (
	locationStables map[server.Id]bool,
	returnErr error,
) {
	locationStables = map[server.Id]bool{}

	server.Redis(ctx, func(r server.RedisClient) {
		locationFilterCmds := map[server.Id]*redis.StringCmd{}

		// plain pipeline instead of tx: independent gets across cluster slots
		pipe := r.Pipeline()
		for _, locationId := range locationIds {
			locationFilterCmds[locationId] = pipe.Get(
				ctx,
				clientScoreLocationFilterKey(forceMinimum, rankMode, locationId, clientLocationId),
			)
		}
		// note ignore the error for GET since it will include missing key
		pipe.Exec(ctx)

		for locationId, filterCmd := range locationFilterCmds {
			filterBytes, _ := filterCmd.Bytes()
			if len(filterBytes) == 0 {
				// there are no providers
				continue
			}
			b := bytes.NewBuffer(filterBytes)
			e := gob.NewDecoder(b)
			var filter ClientFilter
			returnErr = e.Decode(&filter)
			if returnErr != nil {
				return
			}
			if 0 < filter.Count {
				stable := MinStableNetReliabilityWeight <= filter.NetReliabilityWeight
				locationStables[locationId] = stable
			}
			// else there are no providers
		}
	})
	return
}

func FindProviderLocations(
	findLocations *FindLocationsArgs,
	session *session.ClientSession,
) (*FindLocationsResult, error) {
	query := strings.TrimSpace(findLocations.Query)
	if clientId, err := server.ParseId(query); err == nil {
		device := &LocationDeviceResult{
			ClientId:   clientId,
			DeviceName: fmt.Sprintf("%s", clientId),
		}

		deviceResults := []*LocationDeviceResult{
			device,
		}

		return &FindLocationsResult{
			Locations: []*LocationResult{},
			Groups:    []*LocationGroupResult{},
			Devices:   deviceResults,
		}, nil
	} else {
		// note group search is no longer supported

		rankMode := RankModeQuality
		if findLocations.RankMode != "" {
			rankMode = findLocations.RankMode
		}

		// the caller ip is used to match against provider excluded lists
		clientIp, _, err := session.ParseClientIpPort()
		if err != nil {
			return nil, err
		}

		ipInfo, err := server.GetIpInfo(clientIp)
		if err != nil {
			return nil, err
		}

		clientLocationId := countryCodeLocationIds()[ipInfo.CountryCode]

		var matchDistances map[server.Id]int
		var clientLocations map[server.Id]*ClientLocation
		if query == "" {
			initialClientLocations, err := loadInitialClientLocations(session.Ctx)
			if err != nil {
				return nil, err
			}
			matchDistances = map[server.Id]int{}
			clientLocations = map[server.Id]*ClientLocation{}
			for _, clientLocation := range initialClientLocations.Locations {
				clientLocations[clientLocation.LocationId] = clientLocation
			}
		} else {
			maxSearchDistance := 2
			locationSearchResults := locationSearch().AroundIds(
				session.Ctx,
				query,
				maxSearchDistance,
				search.OptMostLikley(30),
			)

			locationIds := map[server.Id]bool{}
			for locationId, _ := range locationSearchResults {
				locationIds[locationId] = true
			}
			var err error
			clientLocations, err = loadClientLocations(session.Ctx, locationIds)
			if err != nil {
				return nil, err
			}

			matchDistances = map[server.Id]int{}
			for locationId, _ := range clientLocations {
				if r, ok := locationSearchResults[locationId]; ok {
					matchDistances[locationId] = r.ValueDistance
				} else {
					matchDistances[locationId] = maxSearchDistance + 1
				}
			}
		}

		// ignore if this meta data can't be loaded
		// in that case, all locations will be considered unstable
		locationStables, _ := loadLocationStables(
			session.Ctx,
			slices.Collect(maps.Keys(clientLocations)),
			// user-facing search: only surface locations that meet the bar
			false,
			rankMode,
			clientLocationId,
		)
		if locationStables == nil {
			locationStables = map[server.Id]bool{}
		}

		locationResults := []*LocationResult{}

		for locationId, clientLocation := range clientLocations {
			stable, ok := locationStables[locationId]
			if ok {
				locationResult := &LocationResult{
					LocationId:        clientLocation.LocationId,
					LocationType:      clientLocation.LocationType,
					Name:              clientLocation.Name,
					CityLocationId:    clientLocation.CityLocationId,
					RegionLocationId:  clientLocation.RegionLocationId,
					CountryLocationId: clientLocation.CountryLocationId,
					CountryCode:       clientLocation.CountryCode,
					ProviderCount:     clientLocation.ClientCount,
					StrongPrivacy:     clientLocation.StrongPrivacy,
					Stable:            stable,
					MatchDistance:     matchDistances[locationId],
				}
				locationResults = append(locationResults, locationResult)
			}
		}

		for _, locationResult := range locationResults {
			locationResult.City = clientLocationName(clientLocations, locationResult.CityLocationId)
			locationResult.Region = clientLocationName(clientLocations, locationResult.RegionLocationId)
			locationResult.Country = clientLocationName(clientLocations, locationResult.CountryLocationId)
		}

		result := &FindLocationsResult{
			Locations: locationResults,
			Groups:    []*LocationGroupResult{},
			Devices:   []*LocationDeviceResult{},
		}
		result.SetStats()

		return result, nil
	}
}

// since there are no promoted groups, this call can be replaced with `FindProviderLocations` with an empty query
func GetProviderLocations(
	session *session.ClientSession,
) (*FindLocationsResult, error) {
	rankMode := RankModeQuality

	// the caller ip is used to match against provider excluded lists
	clientIp, _, err := session.ParseClientIpPort()

	var clientLocationId server.Id
	if err == nil {
		ipInfo, err := server.GetIpInfo(clientIp)
		if err == nil {
			clientLocationId = countryCodeLocationIds()[ipInfo.CountryCode]
		} else {
			glog.V(2).Infof("[GetProviderLocations] could not get ip info for %s: %s\n", clientIp, err)
		}
	} else {
		glog.V(2).Infof("[GetProviderLocations] could not parse client ip: %s\n", err)
	}

	initialClientLocations, err := loadInitialClientLocations(session.Ctx)
	if err != nil {
		return nil, err
	}
	if initialClientLocations == nil {
		initialClientLocations = &InitialClientLocations{}
	}

	locationIds := []server.Id{}
	for _, clientLocation := range initialClientLocations.Locations {
		locationIds = append(locationIds, clientLocation.LocationId)
	}

	// ignore if this meta data can't be loaded
	// in that case, all locations will be considered unstable
	locationStables, _ := loadLocationStables(
		session.Ctx,
		locationIds,
		// user-facing listing: only surface locations that meet the bar
		false,
		rankMode,
		clientLocationId,
	)
	if locationStables == nil {
		locationStables = map[server.Id]bool{}
	}

	locationResults := []*LocationResult{}
	locationGroupResults := []*LocationGroupResult{}

	for _, clientLocation := range initialClientLocations.Locations {
		stable, ok := locationStables[clientLocation.LocationId]
		if ok {
			locationResult := &LocationResult{
				LocationId:        clientLocation.LocationId,
				LocationType:      clientLocation.LocationType,
				Name:              clientLocation.Name,
				CityLocationId:    clientLocation.CityLocationId,
				RegionLocationId:  clientLocation.RegionLocationId,
				CountryLocationId: clientLocation.CountryLocationId,
				CountryCode:       clientLocation.CountryCode,
				ProviderCount:     clientLocation.ClientCount,
				StrongPrivacy:     clientLocation.StrongPrivacy,
				Stable:            stable,
			}
			locationResults = append(locationResults, locationResult)
		}
	}
	for _, clientLocationGroup := range initialClientLocations.LocationGroups {
		locationGroupResult := &LocationGroupResult{
			LocationGroupId: clientLocationGroup.LocationGroupId,
			Name:            clientLocationGroup.Name,
			Promoted:        clientLocationGroup.Promoted,
		}
		locationGroupResults = append(locationGroupResults, locationGroupResult)
	}

	locationsById := map[server.Id]*ClientLocation{}
	for _, cl := range initialClientLocations.Locations {
		locationsById[cl.LocationId] = cl
	}
	for _, locationResult := range locationResults {
		locationResult.City = clientLocationName(locationsById, locationResult.CityLocationId)
		locationResult.Region = clientLocationName(locationsById, locationResult.RegionLocationId)
		locationResult.Country = clientLocationName(locationsById, locationResult.CountryLocationId)
	}

	result := &FindLocationsResult{
		Locations: locationResults,
		Groups:    locationGroupResults,
		Devices:   []*LocationDeviceResult{},
	}
	result.SetStats()

	return result, nil
}

// no longer supported
func FindLocations(
	findLocations *FindLocationsArgs,
	session *session.ClientSession,
) (*FindLocationsResult, error) {
	return &FindLocationsResult{
		Locations: []*LocationResult{},
		Groups:    []*LocationGroupResult{},
		Devices:   []*LocationDeviceResult{},
	}, nil
}

type FindProvidersArgs struct {
	LocationId       *server.Id  `json:"location_id,omitempty"`
	LocationGroupId  *server.Id  `json:"location_group_id,omitempty"`
	Count            int         `json:"count"`
	ExcludeClientIds []server.Id `json:"exclude_location_ids,omitempty"`
}

type FindProvidersResult struct {
	ClientIds []server.Id `json:"client_ids,omitempty"`
}

// no longer supported. See `FindProviders2`
func FindProviders(
	findProviders *FindProvidersArgs,
	session *session.ClientSession,
) (*FindProvidersResult, error) {
	return &FindProvidersResult{
		ClientIds: []server.Id{},
	}, nil
}

type ProviderSpec struct {
	LocationId      *server.Id `json:"location_id,omitempty"`
	LocationGroupId *server.Id `json:"location_group_id,omitempty"`
	ClientId        *server.Id `json:"client_id,omitempty"`
	BestAvailable   bool       `json:"best_available,omitempty"`
}

type RankMode = string

const (
	RankModeQuality RankMode = "quality"
	RankModeSpeed   RankMode = "speed"
)

type FindProviders2Args struct {
	Specs               []*ProviderSpec `json:"specs"`
	Count               int             `json:"count"`
	ForceCount          bool            `json:"force_count"`
	ExcludeClientIds    []server.Id     `json:"exclude_client_ids"`
	ExcludeDestinations [][]server.Id   `json:"exclude_destinations"`
	RankMode            RankMode        `json:"rank_mode"`
	ForceMinimum        bool            `json:"force_minimum"`
}

type FindProviders2Result struct {
	Providers []*FindProvidersProvider `json:"providers"`
}

type FindProvidersProvider struct {
	ClientId                   server.Id         `json:"client_id"`
	EstimatedBytesPerSecond    ByteCount         `json:"estimated_bytes_per_second"`
	HasEstimatedBytesPerSecond bool              `json:"has_estimated_bytes_per_second"`
	Tier                       int               `json:"tier"`
	IntermediaryIds            []server.Id       `json:"intermediary_ids"`
	Location                   *ProviderLocation `json:"location,omitempty"`
}

type LocationCoordinates struct {
	Lat float64 `json:"lat"`
	Lon float64 `json:"lon"`
}

type ProviderLocation struct {
	Country           string               `json:"country,omitempty"`
	CountryCode       string               `json:"country_code,omitempty"`
	Region            string               `json:"region,omitempty"`
	City              string               `json:"city,omitempty"`
	CountryLocationId *server.Id           `json:"country_location_id,omitempty"`
	RegionLocationId  *server.Id           `json:"region_location_id,omitempty"`
	CityLocationId    *server.Id           `json:"city_location_id,omitempty"`
	RegionCoordinates *LocationCoordinates `json:"region_coordinates,omitempty"`
	CityCoordinates   *LocationCoordinates `json:"city_coordinates,omitempty"`
}

type ClientScore struct {
	ClientId                     server.Id
	NetworkId                    server.Id
	Scores                       map[string]int
	ReliabilityWeight            float64
	IndependentReliabilityWeight float64
	Tiers                        map[string]int
	MinRelativeLatencyMillis     int
	MaxBytesPerSecond            ByteCount
	HasLatencyTest               bool
	HasSpeedTest                 bool

	// true when the provider holds a ProvideModeNetwork provide key but no
	// ProvideModePublic one, i.e. it can only settle a contract with a source
	// in its own network. FindProviders2 keeps such a provider only for callers
	// in NetworkId. It is stored negated on purpose: the score cache is gob
	// encoded with a 5h ttl, so entries written before this field existed decode
	// with the zero value, and the zero value has to mean "publicly usable" --
	// the pre-existing behaviour -- or every provider would be treated as
	// network-only until the cache turned over.
	NetworkOnly bool

	// set only on the top-level score, never on the `LookbackClientScores`
	// copies: each score is gob-serialized into thousands of cache key
	// permutations, and gob transmits zero-valued arrays, so these are
	// pointers (omitted when nil) and stay nil on the nested lookback copies
	CityLocationId    *server.Id
	RegionLocationId  *server.Id
	CountryLocationId *server.Id

	LookbackIndex        int
	LookbackClientScores map[int]*ClientScore

	ScaledWeights  map[string]float32
	PassesMinimums map[string]bool
}

type ClientFilter struct {
	Count                int
	NetReliabilityWeight float64
	Index                int
}

// scores are [0, max], where 0 is best
const MaxClientScore = 50
const ClientScoreSampleCount = 200

// choose a filter that has at least this number of providers
// FIXME this scale based on traffic for region
// const MinExportNetReliabilityWeight = float64(400)

// the number of filtered providers to consider a location stable
const MinStableNetReliabilityWeight = float64(4)

// the client score cache keys hash tag on the (caller location, target) pair
// so that the family spreads across cluster slots. tagging only the caller
// location would concentrate all targets for a popular caller location
// (e.g. us) on a single node, which can exceed the node's memory.
// the sample index stays outside the tag.
func clientScoreLocationCountsKey(forceMinimum bool, rankMode RankMode, locationId server.Id, callerLocationId server.Id) string {
	fm := 0
	if forceMinimum {
		fm = 1
	}
	rm, _ := utf8.DecodeRuneInString(rankMode)
	return fmt.Sprintf("{cs_%d_%c_%s_%s}c_l", fm, rm, callerLocationId, locationId)
}

func clientScoreLocationGroupCountsKey(forceMinimum bool, rankMode RankMode, locationGroupId server.Id, callerLocationId server.Id) string {
	fm := 0
	if forceMinimum {
		fm = 1
	}
	rm, _ := utf8.DecodeRuneInString(rankMode)
	return fmt.Sprintf("{cs_%d_%c_%s_%s}c_g", fm, rm, callerLocationId, locationGroupId)
}

func clientScoreLocationFilterKey(forceMinimum bool, rankMode RankMode, locationId server.Id, callerLocationId server.Id) string {
	fm := 0
	if forceMinimum {
		fm = 1
	}
	rm, _ := utf8.DecodeRuneInString(rankMode)
	return fmt.Sprintf("{cs_%d_%c_%s_%s}f_l", fm, rm, callerLocationId, locationId)
}

func clientScoreLocationGroupFilterKey(forceMinimum bool, rankMode RankMode, locationGroupId server.Id, callerLocationId server.Id) string {
	fm := 0
	if forceMinimum {
		fm = 1
	}
	rm, _ := utf8.DecodeRuneInString(rankMode)
	return fmt.Sprintf("{cs_%d_%c_%s_%s}f_g", fm, rm, callerLocationId, locationGroupId)
}

func clientScoreLocationSampleKey(forceMinimum bool, rankMode RankMode, locationId server.Id, callerLocationId server.Id, index int) string {
	fm := 0
	if forceMinimum {
		fm = 1
	}
	rm, _ := utf8.DecodeRuneInString(rankMode)
	return fmt.Sprintf("{cs_%d_%c_%s_%s}s_l_%d", fm, rm, callerLocationId, locationId, index)
}

func clientScoreLocationGroupSampleKey(forceMinimum bool, rankMode RankMode, locationGroupId server.Id, callerLocationId server.Id, index int) string {
	fm := 0
	if forceMinimum {
		fm = 1
	}
	rm, _ := utf8.DecodeRuneInString(rankMode)
	return fmt.Sprintf("{cs_%d_%c_%s_%s}s_g_%d", fm, rm, callerLocationId, locationGroupId, index)
}

func UpdateClientScores(ctx context.Context, ttl time.Duration, parallel int) (returnErr error) {
	addClientScore := func(lookbackClientScore *ClientScore, m map[server.Id]*ClientScore) *ClientScore {
		clientScore, ok := m[lookbackClientScore.ClientId]
		if !ok {
			clientScore = &ClientScore{
				ClientId:             lookbackClientScore.ClientId,
				NetworkId:            lookbackClientScore.NetworkId,
				NetworkOnly:          lookbackClientScore.NetworkOnly,
				LookbackClientScores: map[int]*ClientScore{},
			}
			m[lookbackClientScore.ClientId] = clientScore
		}
		clientScore.LookbackClientScores[lookbackClientScore.LookbackIndex] = lookbackClientScore
		return clientScore
	}

	locationClientScores := map[server.Id]map[server.Id]*ClientScore{}
	locationGroupClientScores := map[server.Id]map[server.Id]*ClientScore{}

	type performanceTarget struct {
		relativeLatencyMillisThreshold int
		relativeLatencyMillisCutoff    int
		relativeLatencyMillisPerScore  int
		bytesPerSecondThreshold        ByteCount
		bytesPerSecondCutoff           ByteCount
		bytesPerSecondPerScore         ByteCount
	}

	scorePerTier := 20
	missingLatencyScore := 2 * scorePerTier
	missingSpeedScore := 2 * scorePerTier

	performanceTargets := map[RankMode]performanceTarget{
		RankModeQuality: performanceTarget{
			relativeLatencyMillisThreshold: 50,
			relativeLatencyMillisCutoff:    200,
			relativeLatencyMillisPerScore:  20,
			bytesPerSecondThreshold:        8 * Mib,
			bytesPerSecondCutoff:           800 * Kib,
			bytesPerSecondPerScore:         200 * Kib,
		},
		RankModeSpeed: performanceTarget{
			relativeLatencyMillisThreshold: 20,
			relativeLatencyMillisCutoff:    50,
			relativeLatencyMillisPerScore:  5,
			bytesPerSecondThreshold:        40 * Mib,
			bytesPerSecondCutoff:           4 * Mib,
			bytesPerSecondPerScore:         1 * Mib,
		},
	}

	setScore := func(
		clientScore *ClientScore,
		netTypeScores map[RankMode]int,
		minRelativeLatencyMillis int,
		maxBytesPerSecond ByteCount,
		hasLatencyTest bool,
		hasSpeedTest bool,
	) {
		for rankMode, target := range performanceTargets {
			exclude := false
			scoreAdjust := 0

			if hasLatencyTest {
				if target.relativeLatencyMillisCutoff < minRelativeLatencyMillis {
					exclude = true
				} else if d := minRelativeLatencyMillis - target.relativeLatencyMillisThreshold; 0 < d {
					scoreAdjust += (d + target.relativeLatencyMillisPerScore/2) / target.relativeLatencyMillisPerScore
				}
			} else {
				scoreAdjust += missingLatencyScore
			}

			if hasSpeedTest {
				if maxBytesPerSecond < target.bytesPerSecondCutoff {
					exclude = true
				} else if d := target.bytesPerSecondThreshold - maxBytesPerSecond; 0 < d {
					scoreAdjust += int((d + target.bytesPerSecondPerScore/2) / target.bytesPerSecondPerScore)
				}
			} else {
				scoreAdjust += missingSpeedScore
			}

			if !exclude {
				score := min(
					scorePerTier*netTypeScores[rankMode]+scoreAdjust,
					MaxClientScore,
				)
				clientScore.Scores[rankMode] = score
				clientScore.Tiers[rankMode] = score / scorePerTier
			} else {
				clientScore.Scores[rankMode] = 0
				clientScore.Tiers[rankMode] = (MaxClientScore + scorePerTier - 1) / scorePerTier
			}
		}
	}

	loadClientScore := func(result server.PgResult) (lookbackClientScore *ClientScore, cityLocationXId *server.Id, regionLocationXId *server.Id, countryLocationXId *server.Id) {
		var clientId server.Id
		var networkId server.Id
		var netTypeScore int
		var netTypeScoreSpeed int
		var minRelativeLatencyMillis int
		var maxBytesPerSecond ByteCount
		var hasLatencyTest bool
		var hasSpeedTest bool
		var lookbackIndex int
		var reliabilityWeight float64
		var independentReliabilityWeight float64
		var publiclyUsable bool
		server.Raise(result.Scan(
			&cityLocationXId,
			&regionLocationXId,
			&countryLocationXId,
			&clientId,
			&networkId,
			&netTypeScore,
			&netTypeScoreSpeed,
			&minRelativeLatencyMillis,
			&maxBytesPerSecond,
			&hasLatencyTest,
			&hasSpeedTest,
			&lookbackIndex,
			&reliabilityWeight,
			&independentReliabilityWeight,
			&publiclyUsable,
		))
		lookbackClientScore = &ClientScore{
			ClientId:                     clientId,
			LookbackIndex:                lookbackIndex,
			NetworkId:                    networkId,
			NetworkOnly:                  !publiclyUsable,
			ReliabilityWeight:            reliabilityWeight,
			IndependentReliabilityWeight: independentReliabilityWeight,
			MinRelativeLatencyMillis:     minRelativeLatencyMillis,
			MaxBytesPerSecond:            maxBytesPerSecond,
			HasLatencyTest:               hasLatencyTest,
			HasSpeedTest:                 hasSpeedTest,
			Scores:                       map[string]int{},
			Tiers:                        map[string]int{},
		}

		netTypeScores := map[RankMode]int{
			RankModeQuality: netTypeScore,
			RankModeSpeed:   netTypeScoreSpeed,
		}

		setScore(
			lookbackClientScore,
			netTypeScores,
			minRelativeLatencyMillis,
			maxBytesPerSecond,
			hasLatencyTest,
			hasSpeedTest,
		)

		return
	}

	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
	        SELECT
	        	network_client_location_reliability.city_location_id,
	        	network_client_location_reliability.region_location_id,
	        	network_client_location_reliability.country_location_id,
	            network_client_location_reliability.client_id,
	            network_client_location_reliability.network_id,
	            network_client_location_reliability.max_net_type_score,
	            network_client_location_reliability.max_net_type_score_speed,
	            network_client_location_reliability.min_relative_latency_ms,
	            network_client_location_reliability.max_bytes_per_second,
	            network_client_location_reliability.has_latency_test,
	            network_client_location_reliability.has_speed_test,
	            -- fix(beta): see the LEFT JOIN comment below
	            COALESCE(client_connection_reliability_score.lookback_index, 0),
	            COALESCE(client_connection_reliability_score.reliability_weight, 1),
	            COALESCE(client_connection_reliability_score.independent_reliability_weight, 1),
	            -- publicly usable: holds a Public provide key, so a caller from
	            -- any network can settle a contract with it. A provider that is
	            -- in the pool without this is Network-only, and FindProviders2
	            -- hands it out to callers in its own network only.
	            EXISTS (
	            	SELECT 1 FROM provide_key
	            	WHERE
	            		provide_key.client_id = network_client_location_reliability.client_id AND
	            		provide_key.provide_mode = $1
	            )

	        FROM network_client_location_reliability

	        -- fix(beta): same class of issue as UpdateClientLocations above --
	        -- an INNER JOIN here requires a reliability score to already exist
	        -- before a client counts toward its location's stability filter at
	        -- all, which the reliability-scoring pipeline may never produce at
	        -- this env's small/cold-start scale. LEFT JOIN plus the COALESCE
	        -- defaults above treats an unscored client as neutral (full
	        -- weight, lookback 0) rather than excluding it outright.
	        LEFT JOIN client_connection_reliability_score ON
	        	client_connection_reliability_score.client_id = network_client_location_reliability.client_id
	        WHERE
	        	network_client_location_reliability.connected = true AND
	        	network_client_location_reliability.valid = true AND
	        	-- the candidate pool, unlike the public count in
	        	-- UpdateClientLocations, carries every provider that can settle
	        	-- a contract with *someone*: Public for any caller, Network for
	        	-- callers in the provider's own network. GetProvideRelationship
	        	-- only ever returns one of those two, so this pair is exactly
	        	-- the set CreateContract can accept. FindProviders2 then decides
	        	-- eligibility per request -- it has to be done there and not
	        	-- here, because the score cache is keyed by (forceMinimum,
	        	-- rankMode, locationId, callerLocationId) and is not
	        	-- network-scoped.
	        	--
	        	-- Restricting this to Public would remove Network-only
	        	-- providers from their own network's discovery, which works
	        	-- today via CreateContractNoEscrow. Stream is still excluded:
	        	-- resolveNonCompanionProvideMode can resolve a Stream-only
	        	-- destination as a companion, but that dead-ends at
	        	-- CreateCompanionTransferEscrow, which needs a pre-existing
	        	-- reverse origin contract, so it can never bootstrap a session.
	        	--
	        	-- GetProviderLocations gates on loadLocationStables, populated
	        	-- from here; see exportClientScores for why the ClientFilter it
	        	-- reads stays Public-only.
	        	EXISTS (
	        		SELECT 1 FROM provide_key
	        		WHERE
	        			provide_key.client_id = network_client_location_reliability.client_id AND
	        			provide_key.provide_mode IN ($1, $2)
	        	)
	        `,
			ProvideModePublic,
			ProvideModeNetwork,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				lookbackClientScore, cityLocationId, regionLocationId, countryLocationId := loadClientScore(result)

				// top-level only; the lookback copies stay nil (see `ClientScore`)
				setLocationIds := func(clientScore *ClientScore) {
					clientScore.CityLocationId = cityLocationId
					clientScore.RegionLocationId = regionLocationId
					clientScore.CountryLocationId = countryLocationId
				}

				// once per distinct location id: a country-only client stores
				// its country id in all three columns (see
				// SetConnectionLocation), and a client belongs in a location's
				// pool once. The per-location map is keyed by client id so a
				// repeat is already absorbed, but going through the set makes
				// the intent explicit and keeps this loop in step with the
				// counting loop in UpdateClientLocations.
				for _, locationId := range distinctIds(
					cityLocationId,
					regionLocationId,
					countryLocationId,
				) {
					clientScores, ok := locationClientScores[locationId]
					if !ok {
						clientScores = map[server.Id]*ClientScore{}
						locationClientScores[locationId] = clientScores
					}
					setLocationIds(addClientScore(lookbackClientScore, clientScores))
				}
			}
		})

		result, err = conn.Query(
			ctx,
			`
	            SELECT
	            	location_group_member_city.location_group_id AS city_location_group_id,
	            	location_group_member_region.location_group_id AS region_location_group_id,
	            	location_group_member_country.location_group_id AS country_location_group_id,
	                network_client_location_reliability.client_id,
	                network_client_location_reliability.network_id,
	                network_client_location_reliability.max_net_type_score,
	                network_client_location_reliability.max_net_type_score_speed,
	                network_client_location_reliability.min_relative_latency_ms,
		            network_client_location_reliability.max_bytes_per_second,
		            network_client_location_reliability.has_latency_test,
		            network_client_location_reliability.has_speed_test,
		            COALESCE(client_connection_reliability_score.lookback_index, 0),  -- fix(beta): see LEFT JOIN comment below
	                COALESCE(client_connection_reliability_score.reliability_weight, 1),
	                COALESCE(client_connection_reliability_score.independent_reliability_weight, 1),
	                -- publicly usable; see the per-location query above
	                EXISTS (
	                	SELECT 1 FROM provide_key
	                	WHERE
	                		provide_key.client_id = network_client_location_reliability.client_id AND
	                		provide_key.provide_mode = $1
	                )

	            FROM network_client_location_reliability

	            -- fix(beta): same class of issue as UpdateClientLocations/the query
            -- above this one -- treats an unscored client as neutral rather
            -- than excluding it, since the reliability-scoring pipeline may
            -- never populate at this env's small/cold-start scale
            LEFT JOIN client_connection_reliability_score ON
	        		client_connection_reliability_score.client_id = network_client_location_reliability.client_id

	            LEFT JOIN location_group_member location_group_member_city ON
	                location_group_member_city.location_id = network_client_location_reliability.city_location_id

	            LEFT JOIN location_group_member location_group_member_region ON
	                location_group_member_region.location_id = network_client_location_reliability.region_location_id

	            LEFT JOIN location_group_member location_group_member_country ON
	                location_group_member_country.location_id = network_client_location_reliability.country_location_id

	            WHERE
	            	network_client_location_reliability.connected = true AND
	            	network_client_location_reliability.valid = true AND
	            	-- same rule as the per-location query above: Public or
	            	-- Network. This one fills locationGroupClientScores -> the
	            	-- clientScoreLocationGroup* redis keys -> loadClientScores
	            	-- -> FindProviders2 whenever a spec carries a
	            	-- LocationGroupId, so a user who picks a promoted group
	            	-- (e.g. "Strong Privacy Laws") must be filtered by the same
	            	-- request-time network check as a plain location.
	            	EXISTS (
	            		SELECT 1 FROM provide_key
	            		WHERE
	            			provide_key.client_id = network_client_location_reliability.client_id AND
	            			provide_key.provide_mode IN ($1, $2)
	            	)
	        `,
			ProvideModePublic,
			ProvideModeNetwork,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				lookbackClientScore, cityLocationGroupId, regionLocationGroupId, countryLocationGroupId := loadClientScore(result)

				// once per distinct group id. The three location columns can
				// be the same id (a country-only client), in which case all
				// three group joins resolve to the same membership rows.
				for _, locationGroupId := range distinctIds(
					cityLocationGroupId,
					regionLocationGroupId,
					countryLocationGroupId,
				) {
					clientScores, ok := locationGroupClientScores[locationGroupId]
					if !ok {
						clientScores = map[server.Id]*ClientScore{}
						locationGroupClientScores[locationGroupId] = clientScores
					}
					addClientScore(lookbackClientScore, clientScores)
				}
			}
		})
	})

	type filter struct {
		maxScore                         int
		minIndependentReliabilityWeights map[int]float64
		// minBytesPerSecond                ByteCount
		// maxRelativeLatencyMillis         int
	}
	// filters are tested in order of declaration for `MinExportNetReliabilityWeight`
	// to minimize the chance of bad providers in the `FindProviders2` randomized shuffle
	// the last filter represents the worst case the network will expose to users
	minFilter := filter{
		maxScore: 2 * scorePerTier,
	}
	// keyed by lookback index (see ClientLookbacks): 1 = the hour, 2 = 12h.
	// The hour threshold is the gate that decides whether a provider is in the
	// market at all. It sat at 0.99 -- less than one bad block in 60 -- which
	// left no slack for a reconnect that straddles a block boundary (the block
	// carrying only the new-connection sync has no established sync, so it is
	// invalid however tolerant the rule is). 0.95 allows three such blocks an
	// hour. Repeated reconnects still fail it: they are real user impact, and
	// `client_reliability_valid` only forgives ONE per block.
	if NormalNetworkConditions() {
		minFilter.minIndependentReliabilityWeights = map[int]float64{
			1: float64(0.95),
			2: float64(0.7),
			3: float64(0.6),
		}
	} else {
		// some abormal conditions, loosen the stats as they reset
		minFilter.minIndependentReliabilityWeights = map[int]float64{
			1: float64(0.8),
			2: float64(0.6),
			3: float64(0.6),
		}
	}
	minReliabilityWeightScale := 0.1
	maxReliabilityWeightScale := 1.0
	minScoreScale := 0.1
	maxScoreScale := 1.0

	// health is loaded once for the whole pass rather than per client: this
	// walks every provider, and the table is one row per ever-probed provider.
	//
	// # Staleness
	//
	// A record is used however old it is, deliberately. Nothing sweeps
	// provider_egress_health and SetProviderEgressHealth upserts on client_id,
	// so a row is always that provider's most recent measurement -- there is no
	// such thing as a superseded row still sitting in the table. Ageing records
	// out would mean that if the prober ever stalls, the entire public list
	// silently empties, which is a far worse failure than trusting a
	// measurement that is a day old.
	//
	// The asymmetry that leaves is intended. A stale *good* record keeps a
	// provider visible; a stale *bad* record keeps it hidden until it is probed
	// again. That is only safe because the probe due-queue
	// (GetProviderEgressLocationDue) is deliberately not gated on any of this --
	// it reads the live provider population directly, so an excluded provider is
	// still handed to the prober and still graduates the moment it measures
	// healthy. If that queue ever starts consulting these scores, an excluded
	// provider can never be re-measured and is stuck out permanently.
	// Shared with UpdateClientLocations so the gated membership and the
	// advertised count can never disagree about what "healthy" means.
	// UpdateClientScores uses passesHealth ONLY: its candidate pool is not
	// country-scoped, so the observed-country check does not apply here.
	egressTestEnabled := providerEgressTestEnabled()
	countFilter := providerCountFilter{}
	if egressTestEnabled {
		countFilter = newProviderCountFilter(ctx)
	} else {
		glog.Infof("[nclm]provider egress test is disabled; skipping the provider score gate for this pass\n")
	}

	// migration: set each client score to the lowest lookback index index
	migrateClientScore := func(clientScore *ClientScore) {
		lookbackIndexes := slices.Collect(maps.Keys(clientScore.LookbackClientScores))
		slices.Sort(lookbackIndexes)
		minLookbackIndex := lookbackIndexes[0]

		minClientScore := clientScore.LookbackClientScores[minLookbackIndex]

		clientScore.Scores = minClientScore.Scores
		clientScore.ReliabilityWeight = minClientScore.ReliabilityWeight
		clientScore.IndependentReliabilityWeight = minClientScore.IndependentReliabilityWeight
		clientScore.Tiers = minClientScore.Tiers
		clientScore.MinRelativeLatencyMillis = minClientScore.MinRelativeLatencyMillis
		clientScore.MaxBytesPerSecond = minClientScore.MaxBytesPerSecond
		clientScore.HasLatencyTest = minClientScore.HasLatencyTest
		clientScore.HasSpeedTest = minClientScore.HasSpeedTest

		clientScore.ScaledWeights = map[string]float32{}
		clientScore.PassesMinimums = map[string]bool{}

		// measured egress health does not vary by rank mode, so it is evaluated
		// once per client and seeds every mode's minimum. It is a gate and
		// nothing else: it can only take a provider out of the pool, and the
		// scaled-weight arithmetic below is untouched, so every provider that
		// still qualifies keeps exactly the weight and ordering it has today.
		passesHealth := !egressTestEnabled || countFilter.passesHealth(clientScore.ClientId)

		for _, rankMode := range slices.Collect(maps.Keys(clientScore.Scores)) {
			passesMinimum := passesHealth
			// all lookback thresholds must pass
			for lookbackIndex, lookbackClientScore := range clientScore.LookbackClientScores {
				if lookbackClientScore.IndependentReliabilityWeight < minFilter.minIndependentReliabilityWeights[lookbackIndex] {
					passesMinimum = false
					break
				}
				if minFilter.maxScore <= lookbackClientScore.Scores[rankMode] {
					passesMinimum = false
					break
				}
			}

			if passesMinimum {
				u := float64(minClientScore.IndependentReliabilityWeight-minFilter.minIndependentReliabilityWeights[minLookbackIndex]) / (1.0 - minFilter.minIndependentReliabilityWeights[minLookbackIndex])
				reliabilityWeightScale := (1-u)*minReliabilityWeightScale + u*maxReliabilityWeightScale
				v := float64(minFilter.maxScore-clientScore.Scores[rankMode]) / float64(minFilter.maxScore)
				scoreScale := (1-v)*minScoreScale + v*maxScoreScale
				clientScore.ScaledWeights[rankMode] = float32(reliabilityWeightScale * clientScore.ReliabilityWeight * scoreScale)
				clientScore.PassesMinimums[rankMode] = true
			}
		}
	}
	for _, clientScores := range locationClientScores {
		for _, clientScore := range clientScores {
			migrateClientScore(clientScore)
		}
	}
	for _, clientScores := range locationGroupClientScores {
		for _, clientScore := range clientScores {
			migrateClientScore(clientScore)
		}
	}

	exportClientScores := func(forceMinimum bool, rankMode RankMode, s map[server.Id]*ClientScore) (
		countsBytes []byte,
		samplesBytes [][]byte,
		filterBytes []byte,
		counts []int,
		samples [][]*ClientScore,
		filter *ClientFilter,
	) {
		clientScores := []*ClientScore{}
		publicCount := 0
		publicNetReliabilityWeight := float64(0)
		for _, clientScore := range s {
			if clientScore.PassesMinimums[rankMode] || forceMinimum {
				clientScores = append(clientScores, clientScore)
				if !clientScore.NetworkOnly {
					publicCount += 1
					publicNetReliabilityWeight += clientScore.ReliabilityWeight
				}
			}
		}

		// the samples above carry Network-only providers too -- FindProviders2
		// filters them per request against the caller's network -- but the
		// ClientFilter does not. It is read only by loadLocationStables, which
		// decides the `Stable` flag GetProviderLocations publishes to every
		// user. That is a public surface, so like the provider count in
		// UpdateClientLocations it counts only providers a stranger can reach:
		// a location whose only supply is network-only is not stable, and with
		// zero public providers it reports no providers at all.
		filter = &ClientFilter{
			Count:                publicCount,
			NetReliabilityWeight: publicNetReliabilityWeight,
		}

		mathrand.Shuffle(len(clientScores), func(i int, j int) {
			clientScores[i], clientScores[j] = clientScores[j], clientScores[i]
		})

		n := (len(clientScores) + ClientScoreSampleCount - 1) / ClientScoreSampleCount

		counts = make([]int, n)
		samples = make([][]*ClientScore, n)
		samplesBytes = make([][]byte, n)

		if 0 < n {
			c := (len(clientScores) + n - 1) / n
			for i := range n {
				i0 := i * c
				i1 := min((i+1)*c, len(clientScores))
				sample := clientScores[i0:i1]

				counts[i] = len(sample)
				samples[i] = sample

				b := bytes.NewBuffer(nil)
				e := gob.NewEncoder(b)
				e.Encode(sample)
				samplesBytes[i] = b.Bytes()
			}
		}

		b := bytes.NewBuffer(nil)
		e := gob.NewEncoder(b)
		e.Encode(counts)
		countsBytes = b.Bytes()

		b = bytes.NewBuffer(nil)
		e = gob.NewEncoder(b)
		e.Encode(filter)
		filterBytes = b.Bytes()

		return
	}

	// location id -> network id
	excludeLocationNetworkIds := map[server.Id]map[server.Id]bool{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				network_id,
				client_location_id
			FROM exclude_network_client_location
			`,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var networkId server.Id
				var clientLocationId server.Id
				server.Raise(result.Scan(
					&networkId,
					&clientLocationId,
				))
				networkIds, ok := excludeLocationNetworkIds[clientLocationId]
				if !ok {
					networkIds = map[server.Id]bool{}
					excludeLocationNetworkIds[clientLocationId] = networkIds
				}
				networkIds[networkId] = true
			}
		})
	})

	filterActive := func(clientScores map[server.Id]*ClientScore, clientLocationId server.Id) map[server.Id]*ClientScore {
		excludeNetworkIds := excludeLocationNetworkIds[clientLocationId]
		if len(excludeNetworkIds) == 0 {
			return clientScores
		}
		activeClientScores := map[server.Id]*ClientScore{}
		for clientId, clientScore := range clientScores {
			if !excludeNetworkIds[clientScore.NetworkId] {
				activeClientScores[clientId] = clientScore
			}
		}
		return activeClientScores
	}

	clientLocationIds := []server.Id{
		// no client location match
		server.Id{},
	}
	clientLocationIds = append(clientLocationIds, slices.Collect(maps.Values(countryCodeLocationIds()))...)

	m := (len(clientLocationIds) + parallel - 1) / parallel
	allBlockClientLocationIds := [][]server.Id{}
	for i := 0; i < len(clientLocationIds); i += m {
		allBlockClientLocationIds = append(allBlockClientLocationIds, clientLocationIds[i:min(len(clientLocationIds), i+m)])
	}

	var wg sync.WaitGroup
	var exportCount atomic.Uint32
	returnErrs := make(chan error, parallel)

	for i := 0; i < len(clientLocationIds); i += m {
		blockClientLocationIds := clientLocationIds[i:min(len(clientLocationIds), i+m)]

		wg.Add(1)
		go connect.HandleError(func() {
			defer wg.Done()

			server.Redis(ctx, func(r server.RedisClient) {
				for _, forceMinimum := range []bool{false, true} {
					for rankMode, _ := range performanceTargets {
						for _, clientLocationId := range blockClientLocationIds {
							// plain pipeline instead of tx: the sets are independent and the
							// keys hash to different cluster slots, which multi/exec cannot span
							pipe := r.Pipeline()

							exportIndex := exportCount.Add(1)
							glog.Infof("[nclm]export client location[%d/%d] %s\n", exportIndex, 2*len(performanceTargets)*len(clientLocationIds), clientLocationId)
							for locationId, clientScores := range locationClientScores {
								activeClientScores := filterActive(clientScores, clientLocationId)
								countsBytes, samplesBytes, filterBytes, counts, _, _ := exportClientScores(forceMinimum, rankMode, activeClientScores)
								pipe.Set(ctx, clientScoreLocationCountsKey(forceMinimum, rankMode, locationId, clientLocationId), countsBytes, ttl)
								pipe.Set(ctx, clientScoreLocationFilterKey(forceMinimum, rankMode, locationId, clientLocationId), filterBytes, ttl)
								for i, sampleBytes := range samplesBytes {
									pipe.Set(ctx, clientScoreLocationSampleKey(forceMinimum, rankMode, locationId, clientLocationId, i), sampleBytes, ttl)
								}
								glog.V(2).Infof("[nclm]update client scores location samples(%s)[%d] = %v\n", locationId, len(counts), counts)
							}
							for locationGroupId, clientScores := range locationGroupClientScores {
								activeClientScores := filterActive(clientScores, clientLocationId)
								countsBytes, samplesBytes, filterBytes, counts, _, _ := exportClientScores(forceMinimum, rankMode, activeClientScores)
								pipe.Set(ctx, clientScoreLocationGroupCountsKey(forceMinimum, rankMode, locationGroupId, clientLocationId), countsBytes, ttl)
								pipe.Set(ctx, clientScoreLocationGroupFilterKey(forceMinimum, rankMode, locationGroupId, clientLocationId), filterBytes, ttl)
								for i, sampleBytes := range samplesBytes {
									pipe.Set(ctx, clientScoreLocationGroupSampleKey(forceMinimum, rankMode, locationGroupId, clientLocationId, i), sampleBytes, ttl)
								}
								glog.V(2).Infof("[nclm]update client scores location group samples(%s)[%d] = %v\n", locationGroupId, len(counts), counts)
							}

							_, err := pipe.Exec(ctx)
							if err != nil {
								select {
								case <-ctx.Done():
									return
								case returnErrs <- err:
									return
								}
							}
						}
					}
				}
			})
			// the deferred wg.Done above covers both return and panic (the
			// closure's defers run before HandleError's recover), so wg.Done
			// must not also be a rescue handler — the pair double-counted on
			// the panic path and crashed the process with a negative
			// WaitGroup counter inside the recovery (2026-08-02).
		})
	}

	wg.Wait()
	close(returnErrs)

	func() {
		for {
			select {
			case <-ctx.Done():
				return
			case err, ok := <-returnErrs:
				if !ok {
					return
				}
				returnErr = errors.Join(returnErr, err)
			}
		}
	}()

	if returnErr == nil {
		glog.Infof(
			"[nclm]update %d client locations x %d location scores, %d location group scores\n",
			len(clientLocationIds),
			len(locationClientScores),
			len(locationGroupClientScores),
		)
	} else {
		glog.Infof("[nclm]update err = %s\n", returnErr)
	}

	return
}

func loadClientScores(
	forceMinimum bool,
	rankMode RankMode,
	ctx context.Context,
	locationIds map[server.Id]bool,
	locationGroupIds map[server.Id]bool,
	clientLocationId server.Id,
	n int,
) (clientScores map[server.Id]*ClientScore, returnErr error) {
	server.Redis(ctx, func(r server.RedisClient) {
		locationCounts := map[server.Id]*redis.StringCmd{}
		locationGroupCounts := map[server.Id]*redis.StringCmd{}

		// plain pipeline instead of tx: independent gets across cluster slots
		pipe := r.Pipeline()
		for locationId, _ := range locationIds {
			v := pipe.Get(ctx, clientScoreLocationCountsKey(forceMinimum, rankMode, locationId, clientLocationId))
			locationCounts[locationId] = v
		}
		for locationGroupId, _ := range locationGroupIds {
			v := pipe.Get(ctx, clientScoreLocationGroupCountsKey(forceMinimum, rankMode, locationGroupId, clientLocationId))
			locationGroupCounts[locationGroupId] = v
		}
		// note ignore the error for GET since it will include missing key
		pipe.Exec(ctx)

		sampleKeyCounts := map[string]int{}

		for locationId, countsCmd := range locationCounts {
			countsBytes, _ := countsCmd.Bytes()
			if len(countsBytes) == 0 {
				continue
			}
			b := bytes.NewBuffer(countsBytes)
			e := gob.NewDecoder(b)
			var counts []int
			returnErr = e.Decode(&counts)
			if returnErr != nil {
				return
			}
			for i, count := range counts {
				sampleKeyCounts[clientScoreLocationSampleKey(forceMinimum, rankMode, locationId, clientLocationId, i)] = count
			}
		}
		for locationGroupId, countsCmd := range locationGroupCounts {
			countsBytes, _ := countsCmd.Bytes()
			if len(countsBytes) == 0 {
				continue
			}
			b := bytes.NewBuffer(countsBytes)
			e := gob.NewDecoder(b)
			var counts []int
			returnErr = e.Decode(&counts)
			if returnErr != nil {
				return
			}
			for i, count := range counts {
				sampleKeyCounts[clientScoreLocationGroupSampleKey(forceMinimum, rankMode, locationGroupId, clientLocationId, i)] = count
			}
		}

		keys := slices.Collect(maps.Keys(sampleKeyCounts))
		mathrand.Shuffle(len(keys), func(i int, j int) {
			keys[i], keys[j] = keys[j], keys[i]
		})

		samples := []*redis.StringCmd{}
		netCount := 0

		pipe = r.Pipeline()
		for _, key := range keys {
			if n <= netCount {
				break
			}
			c := sampleKeyCounts[key]
			v := pipe.Get(ctx, key)
			samples = append(samples, v)
			netCount += c
		}
		// note ignore the error for GET since it will include missing key
		pipe.Exec(ctx)

		clientScores = map[server.Id]*ClientScore{}

		for _, sampleCmd := range samples {
			sampleBytes, _ := sampleCmd.Bytes()
			if len(sampleBytes) == 0 {
				continue
			}
			b := bytes.NewBuffer(sampleBytes)
			e := gob.NewDecoder(b)
			var sample []*ClientScore
			returnErr = e.Decode(&sample)
			if returnErr != nil {
				return
			}

			for _, clientScore := range sample {
				// a client can appear under multiple requested keys with
				// identical ranking fields, but only location-keyed samples
				// carry location ids. keep the copy that has them.
				if existing, ok := clientScores[clientScore.ClientId]; ok {
					if existing.CountryLocationId != nil && clientScore.CountryLocationId == nil {
						continue
					}
				}
				clientScores[clientScore.ClientId] = clientScore
			}
		}
	})

	return
}

// the response location from the score's cached location ids and the
// process-local directory. nil when the ids are unknown (cache blobs written
// before the ids were cached, group-keyed samples) or when the directory has
// not loaded yet or cannot resolve the country.
func resolveProviderLocation(
	directory map[server.Id]*locationDirectoryEntry,
	clientScore *ClientScore,
) *ProviderLocation {
	if clientScore.CountryLocationId == nil {
		return nil
	}
	countryEntry := directory[*clientScore.CountryLocationId]
	if countryEntry == nil {
		return nil
	}

	location := &ProviderLocation{
		Country:           countryEntry.Name,
		CountryCode:       countryEntry.CountryCode,
		CountryLocationId: clientScore.CountryLocationId,
	}
	if clientScore.RegionLocationId != nil {
		if regionEntry := directory[*clientScore.RegionLocationId]; regionEntry != nil {
			location.Region = regionEntry.Name
			location.RegionLocationId = clientScore.RegionLocationId
			if lat, lon, ok := centroidFor(countryEntry.CountryCode, regionEntry.Name); ok {
				location.RegionCoordinates = &LocationCoordinates{
					Lat: lat,
					Lon: lon,
				}
			}
		}
	}
	if clientScore.CityLocationId != nil {
		if cityEntry := directory[*clientScore.CityLocationId]; cityEntry != nil {
			location.City = cityEntry.Name
			location.CityLocationId = clientScore.CityLocationId
			if cityEntry.Latitude != nil && cityEntry.Longitude != nil {
				location.CityCoordinates = &LocationCoordinates{
					Lat: *cityEntry.Latitude,
					Lon: *cityEntry.Longitude,
				}
			}
		}
	}
	return location
}

func FindProviders2(
	findProviders2 *FindProviders2Args,
	session *session.ClientSession,
) (*FindProviders2Result, error) {
	providers := []*FindProvidersProvider{}

	locationIds := map[server.Id]bool{}
	locationGroupIds := map[server.Id]bool{}

	excludeFinalDestinations := sync.OnceValue(func() map[server.Id]bool {
		excludeFinalDestinations := map[server.Id]bool{}
		for _, clientId := range findProviders2.ExcludeClientIds {
			excludeFinalDestinations[clientId] = true
		}
		for _, destination := range findProviders2.ExcludeDestinations {
			excludeFinalDestinations[destination[len(destination)-1]] = true
		}
		return excludeFinalDestinations
	})

	for _, spec := range findProviders2.Specs {
		if spec.LocationId != nil {
			locationIds[*spec.LocationId] = true
		}
		if spec.LocationGroupId != nil {
			locationGroupIds[*spec.LocationGroupId] = true
		}
		if spec.ClientId != nil {
			clientId := *(spec.ClientId)
			if !excludeFinalDestinations()[clientId] {
				provider := &FindProvidersProvider{
					ClientId: clientId,
				}
				providers = append(providers, provider)
			}
		}
		if spec.BestAvailable {
			homeLocationId, ok := countryCodeLocationIds()["us"]
			if ok {
				locationIds[homeLocationId] = true
			}
		}
	}

	if 0 < len(locationIds) || 0 < len(locationGroupIds) {
		// use a min block size to reduce db activity
		var count int
		if findProviders2.ForceCount {
			count = findProviders2.Count
		} else {
			count = max(findProviders2.Count, 20)
		}

		// the random process is
		// 1. load (ideally this would be all, but is truncated for performance)
		// 2. sample based on reliability * quality
		// 3. band based on tier and keep the top `count`
		minLoadCount := 1000
		loadMultiplier := 10

		rankMode := RankModeQuality
		if findProviders2.RankMode != "" {
			rankMode = findProviders2.RankMode
		}

		// the caller ip is used to match against provider excluded lists
		clientIp, _, err := session.ParseClientIpPort()
		if err != nil {
			return nil, err
		}

		ipInfo, err := server.GetIpInfo(clientIp)
		if err != nil {
			return nil, err
		}

		clientLocationId := countryCodeLocationIds()[ipInfo.CountryCode]

		loadStartTime := time.Now()
		clientScores, err := loadClientScores(
			findProviders2.ForceMinimum,
			rankMode,
			session.Ctx,
			locationIds,
			locationGroupIds,
			clientLocationId,
			max(loadMultiplier*count, minLoadCount),
		)
		if err != nil {
			return nil, err
		}
		loadEndTime := time.Now()
		loadDuration := loadEndTime.Sub(loadStartTime)
		loadMillis := float64(loadDuration) / float64(time.Millisecond)
		findProviders2LoadSeconds.Observe(loadDuration.Seconds())
		// one provider search per call makes this client-driven volume: the
		// histogram is the signal, the slow-case line is V(1) detail
		if 50*time.Millisecond <= loadDuration && glog.V(1) {
			glog.Infof(
				"[nclm]findproviders2 load %.2fms (%d)\n",
				loadMillis,
				len(clientScores),
			)
		}

		// drop providers this caller cannot contract with.
		//
		// UpdateClientScores puts both Public and Network-only providers in the
		// pool. A Network-only provider can only settle a contract with a
		// source in its own network (GetProvideRelationship ->
		// ProvideModeNetwork -> CreateContractNoEscrow); it is real, usable
		// supply for those users and has always been discoverable to them, so
		// it stays. For anyone else CreateContract would reject with
		// NoPermission, so it goes.
		//
		// This must happen here, after loadClientScores, and must never be
		// baked into the cached set: the client score redis entries are keyed
		// by (forceMinimum, rankMode, locationId, callerLocationId) with no
		// network component, so a single cached set is shared by callers from
		// every network.
		//
		// A session with no jwt yields the zero network id, which matches no
		// provider -- fail closed rather than leak.
		var callerNetworkId server.Id
		if session.ByJwt != nil {
			callerNetworkId = session.ByJwt.NetworkId
		}
		for clientId, clientScore := range clientScores {
			if clientScore.NetworkOnly && clientScore.NetworkId != callerNetworkId {
				delete(clientScores, clientId)
			}
		}

		for clientId, _ := range excludeFinalDestinations() {
			delete(clientScores, clientId)
		}
		if findProviders2.ForceMinimum {
			for _, clientScore := range clientScores {
				clientScore.ScaledWeights[rankMode] = 1.0
			}
		}
		// the final hop is excluded
		// intermediaries have score reduced
		intermediaryScale := float32(0.5)
		for _, destination := range findProviders2.ExcludeDestinations {
			for _, clientId := range destination[:len(destination)-1] {
				if clientScore, ok := clientScores[clientId]; ok {
					clientScore.ScaledWeights[rankMode] *= intermediaryScale
				}
			}
		}

		clientIds := slices.Collect(maps.Keys(clientScores))
		mathrand.Shuffle(len(clientScores), func(i int, j int) {
			clientIds[i], clientIds[j] = clientIds[j], clientIds[i]
		})

		connect.WeightedSelectFunc(clientIds, count, func(clientId server.Id) float32 {
			clientScore := clientScores[clientId]
			return clientScore.ScaledWeights[rankMode]
		})
		clientIds = clientIds[:min(count, len(clientIds))]

		// band by tier
		slices.SortStableFunc(clientIds, func(a server.Id, b server.Id) int {
			clientScoreA := clientScores[a]
			clientScoreB := clientScores[b]

			return clientScoreA.Tiers[rankMode] - clientScoreB.Tiers[rankMode]
		})

		directory := locationDirectory()

		// output in order of `clientIds`
		for _, clientId := range clientIds {
			clientScore := clientScores[clientId]
			provider := &FindProvidersProvider{
				ClientId:                   clientId,
				Tier:                       clientScore.Tiers[rankMode],
				EstimatedBytesPerSecond:    clientScore.MaxBytesPerSecond,
				HasEstimatedBytesPerSecond: clientScore.HasSpeedTest,
				Location:                   resolveProviderLocation(directory, clientScore),
			}
			providers = append(providers, provider)
		}

		// export one anonymized stats sample tracing this call's pool and
		// selection. Best-effort and gated on stats being enabled, so it is
		// inert unless a process opts in (see recordFindProviders2Sample).
		if s := stats.Default(); s.Enabled() {
			recordFindProviders2Sample(
				s,
				findProviders2,
				rankMode,
				count,
				ipInfo.CountryCode,
				float64(loadDuration.Nanoseconds())/1e6,
				clientScores,
				clientIds,
			)
		}
	}

	// record provider "search interest": each provider that appeared in this
	// result gets one match count, accumulated in redis (never pg on this hot
	// path) and rolled up by RollupSearchProviderStats. Best-effort.
	if 0 < len(providers) {
		providerClientIds := make([]server.Id, 0, len(providers))
		for _, provider := range providers {
			providerClientIds = append(providerClientIds, provider.ClientId)
		}
		RecordProviderSearchMatches(session.Ctx, providerClientIds, server.NowUtc())
	}

	return &FindProviders2Result{
		Providers: providers,
	}, nil
}

type CreateProviderSpecArgs struct {
	Query string `json:"query"`
}

type CreateProviderSpecResult struct {
	Specs []*ProviderSpec `json:"specs"`
}

func CreateProviderSpec(
	createProviderSpec *CreateProviderSpecArgs,
	session *session.ClientSession,
) (*CreateProviderSpecResult, error) {
	// TODO: parse the free-text query into location/group provider specs.
	// Until that resolver exists, return an empty (spec-conformant) result
	// rather than erroring, so the route behaves per the spec shape.
	return &CreateProviderSpecResult{
		Specs: []*ProviderSpec{},
	}, nil
}
