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
	"errors"
	"fmt"
	// "math"
	mathrand "math/rand"
	"slices"
	"unicode/utf8"

	"github.com/urnetwork/glog/v2026"

	"golang.org/x/exp/maps"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/search"
	"github.com/urnetwork/server/v2026/session"
)

func init() {
	resetCountryCodeLocationIds()
	server.OnReset(func() {
		resetCountryCodeLocationIds()
	})
	server.OnWarmup(func() {
		countryCodeLocationIds()
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
				location := &Location{
					LocationType: LocationTypeCountry,
					CountryCode:  v,
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
            `,
			LocationTypeRegion,
			countryCode,
			location.Region,
			countryLocation.LocationId,
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
                    region_location_id = $4 AND
                    country_location_id = $5
                    
            `,
			LocationTypeCity,
			countryCode,
			location.City,
			regionLocation.LocationId,
			countryLocation.LocationId,
		)

		server.WithPgResult(result, err, func() {
			if result.Next() {
				var locationId server.Id
				server.Raise(result.Scan(&locationId))
				cityLocation = &Location{
					LocationType:      LocationTypeCity,
					City:              location.City,
					Region:            regionLocation.Region,
					Country:           countryLocation.Country,
					CountryCode:       countryCode,
					LocationId:        locationId,
					CityLocationId:    locationId,
					RegionLocationId:  regionLocation.LocationId,
					CountryLocationId: countryLocation.LocationId,
				}
			}
		})

		if cityLocation == nil {
			// create a new location

			locationId := server.NewId()

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
                        location_full_name
                    )
                    VALUES ($1, $2, $3, $1, $4, $5, $6, $7)
                `,
				locationId,
				LocationTypeCity,
				location.City,
				regionLocation.LocationId,
				countryLocation.LocationId,
				countryCode,
				fmt.Sprintf("%s, %s, %s", location.City, location.Region, countryCode),
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
                    location_name = $2,
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
	LocationId   server.Id    `json:"location_id"`
	LocationType LocationType `json:"location_type"`
	Name         string       `json:"name"`
	// FIXME add City, Region, Country names
	CityLocationId    *server.Id `json:"city_location_id,omitempty"`
	RegionLocationId  *server.Id `json:"region_location_id,omitempty"`
	CountryLocationId *server.Id `json:"country_location_id,omitempty"`
	CountryCode       string     `json:"country_code"`
	ProviderCount     int        `json:"provider_count,omitempty"`
	MatchDistance     int        `json:"match_distance,omitempty"`

	Stable        bool `json:"stable"`
	StrongPrivacy bool `json:"strong_privacy"`
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

func clientLocationKey(locationId server.Id) string {
	return fmt.Sprintf("{cl}_l_%s", locationId)
}

func initialClientLocationsKey() string {
	return fmt.Sprintf("{cl}i")
}

func UpdateClientLocations(ctx context.Context, ttl time.Duration) (returnErr error) {
	topCitiesPerRegion := 20
	topCitiesPerCountry := 10
	topRegionsPerCountry := 10

	clientLocations := map[server.Id]*ClientLocation{}
	removeClientLocations := map[server.Id]bool{}

	initialClientLocations := &InitialClientLocations{}

	server.Tx(ctx, func(tx server.PgTx) {

		locationClientCounts := map[server.Id]int{}

		result, err := tx.Query(
			ctx,
			`
	        SELECT
	        	network_client_location_reliability.city_location_id,
	        	network_client_location_reliability.region_location_id,
	        	network_client_location_reliability.country_location_id

	        FROM network_client_location_reliability

	        INNER JOIN client_connection_reliability_score ON
	        	client_connection_reliability_score.client_id = network_client_location_reliability.client_id AND
				client_connection_reliability_score.lookback_index = 0

	        WHERE
	        	network_client_location_reliability.connected = true AND
	        	network_client_location_reliability.valid = true
	        `,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var cityLocationId server.Id
				var regionLocationId server.Id
				var countryLocationId server.Id
				server.Raise(result.Scan(
					&cityLocationId,
					&regionLocationId,
					&countryLocationId,
				))

				locationClientCounts[cityLocationId] += 1
				locationClientCounts[regionLocationId] += 1
				locationClientCounts[countryLocationId] += 1
			}
		})

		server.CreateTempTableInTx(
			ctx,
			tx,
			"temp_location_ids(location_id uuid)",
			maps.Keys(locationClientCounts)...,
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
			locationIds := maps.Keys(locationIdCounts)
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
		pipe := r.TxPipeline()

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

			pipe := r.TxPipeline()
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
	rankMode RankMode,
	clientLocationId server.Id,
) (
	locationStables map[server.Id]bool,
	returnErr error,
) {
	locationStables = map[server.Id]bool{}

	server.Redis(ctx, func(r server.RedisClient) {
		locationFilterCmds := map[server.Id]*redis.StringCmd{}

		pipe := r.TxPipeline()
		for _, locationId := range locationIds {
			locationFilterCmds[locationId] = pipe.Get(
				ctx,
				clientScoreLocationFilterKey(false, rankMode, locationId, clientLocationId),
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
	if clientId, err := server.ParseId(findLocations.Query); err == nil {
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

		query := strings.TrimSpace(findLocations.Query)

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
			maps.Keys(clientLocations),
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
	if err != nil {
		return nil, err
	}

	ipInfo, err := server.GetIpInfo(clientIp)
	if err != nil {
		return nil, err
	}

	clientLocationId := countryCodeLocationIds()[ipInfo.CountryCode]

	initialClientLocations, err := loadInitialClientLocations(session.Ctx)
	if err != nil {
		return nil, err
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
	ClientId                   server.Id   `json:"client_id"`
	EstimatedBytesPerSecond    ByteCount   `json:"estimated_bytes_per_second"`
	HasEstimatedBytesPerSecond bool        `json:"has_estimated_bytes_per_second"`
	Tier                       int         `json:"tier"`
	IntermediaryIds            []server.Id `json:"intermediary_ids"`
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

func clientScoreLocationCountsKey(forceMinimum bool, rankMode RankMode, locationId server.Id, callerLocationId server.Id) string {
	fm := 0
	if forceMinimum {
		fm = 1
	}
	rm, _ := utf8.DecodeRuneInString(rankMode)
	return fmt.Sprintf("{cs_%d_%c_%s}c_l_%s", fm, rm, callerLocationId, locationId)
}

func clientScoreLocationGroupCountsKey(forceMinimum bool, rankMode RankMode, locationGroupId server.Id, callerLocationId server.Id) string {
	fm := 0
	if forceMinimum {
		fm = 1
	}
	rm, _ := utf8.DecodeRuneInString(rankMode)
	return fmt.Sprintf("{cs_%d_%c_%s}c_g_%s", fm, rm, callerLocationId, locationGroupId)
}

func clientScoreLocationFilterKey(forceMinimum bool, rankMode RankMode, locationId server.Id, callerLocationId server.Id) string {
	fm := 0
	if forceMinimum {
		fm = 1
	}
	rm, _ := utf8.DecodeRuneInString(rankMode)
	return fmt.Sprintf("{cs_%d_%c_%s}f_l_%s", fm, rm, callerLocationId, locationId)
}

func clientScoreLocationGroupFilterKey(forceMinimum bool, rankMode RankMode, locationGroupId server.Id, callerLocationId server.Id) string {
	fm := 0
	if forceMinimum {
		fm = 1
	}
	rm, _ := utf8.DecodeRuneInString(rankMode)
	return fmt.Sprintf("{cs_%d_%c_%s}f_g_%s", fm, rm, callerLocationId, locationGroupId)
}

func clientScoreLocationSampleKey(forceMinimum bool, rankMode RankMode, locationId server.Id, callerLocationId server.Id, index int) string {
	fm := 0
	if forceMinimum {
		fm = 1
	}
	rm, _ := utf8.DecodeRuneInString(rankMode)
	return fmt.Sprintf("{cs_%d_%c_%s}s_l_%s_%d", fm, rm, callerLocationId, locationId, index)
}

func clientScoreLocationGroupSampleKey(forceMinimum bool, rankMode RankMode, locationGroupId server.Id, callerLocationId server.Id, index int) string {
	fm := 0
	if forceMinimum {
		fm = 1
	}
	rm, _ := utf8.DecodeRuneInString(rankMode)
	return fmt.Sprintf("{cs_%d_%c_%s}s_g_%s_%d", fm, rm, callerLocationId, locationGroupId, index)
}

func UpdateClientScores(ctx context.Context, ttl time.Duration, parallel int) (returnErr error) {
	addClientScore := func(lookbackClientScore *ClientScore, m map[server.Id]*ClientScore) *ClientScore {
		clientScore, ok := m[lookbackClientScore.ClientId]
		if !ok {
			clientScore = &ClientScore{
				ClientId:             lookbackClientScore.ClientId,
				NetworkId:            lookbackClientScore.NetworkId,
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
		))
		lookbackClientScore = &ClientScore{
			ClientId:                     clientId,
			LookbackIndex:                lookbackIndex,
			NetworkId:                    networkId,
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
	            client_connection_reliability_score.lookback_index,
	            client_connection_reliability_score.reliability_weight,
	            client_connection_reliability_score.independent_reliability_weight

	        FROM network_client_location_reliability

	        INNER JOIN client_connection_reliability_score ON
	        	client_connection_reliability_score.client_id = network_client_location_reliability.client_id
	        WHERE
	        	network_client_location_reliability.connected = true AND
	        	network_client_location_reliability.valid = true
	        `,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				lookbackClientScore, cityLocationId, regionLocationId, countryLocationId := loadClientScore(result)

				clientScores, ok := locationClientScores[*cityLocationId]
				if !ok {
					clientScores = map[server.Id]*ClientScore{}
					locationClientScores[*cityLocationId] = clientScores
				}
				addClientScore(lookbackClientScore, clientScores)

				clientScores, ok = locationClientScores[*regionLocationId]
				if !ok {
					clientScores = map[server.Id]*ClientScore{}
					locationClientScores[*regionLocationId] = clientScores
				}
				addClientScore(lookbackClientScore, clientScores)

				clientScores, ok = locationClientScores[*countryLocationId]
				if !ok {
					clientScores = map[server.Id]*ClientScore{}
					locationClientScores[*countryLocationId] = clientScores
				}
				addClientScore(lookbackClientScore, clientScores)
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
		            client_connection_reliability_score.lookback_index,
	                client_connection_reliability_score.reliability_weight,
	                client_connection_reliability_score.independent_reliability_weight

	            FROM network_client_location_reliability

	            INNER JOIN client_connection_reliability_score ON
	        		client_connection_reliability_score.client_id = network_client_location_reliability.client_id

	            LEFT JOIN location_group_member location_group_member_city ON
	                location_group_member_city.location_id = network_client_location_reliability.city_location_id

	            LEFT JOIN location_group_member location_group_member_region ON
	                location_group_member_region.location_id = network_client_location_reliability.region_location_id

	            LEFT JOIN location_group_member location_group_member_country ON
	                location_group_member_country.location_id = network_client_location_reliability.country_location_id

	            WHERE
	            	network_client_location_reliability.connected = true AND
	            	network_client_location_reliability.valid = true
	        `,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				lookbackClientScore, cityLocationGroupId, regionLocationGroupId, countryLocationGroupId := loadClientScore(result)

				if cityLocationGroupId != nil {
					clientScores, ok := locationGroupClientScores[*cityLocationGroupId]
					if !ok {
						clientScores = map[server.Id]*ClientScore{}
						locationGroupClientScores[*cityLocationGroupId] = clientScores
					}
					addClientScore(lookbackClientScore, clientScores)
				}

				if regionLocationGroupId != nil {
					clientScores, ok := locationGroupClientScores[*regionLocationGroupId]
					if !ok {
						clientScores = map[server.Id]*ClientScore{}
						locationGroupClientScores[*regionLocationGroupId] = clientScores
					}
					addClientScore(lookbackClientScore, clientScores)
				}

				if countryLocationGroupId != nil {
					clientScores, ok := locationGroupClientScores[*countryLocationGroupId]
					if !ok {
						clientScores = map[server.Id]*ClientScore{}
						locationGroupClientScores[*countryLocationGroupId] = clientScores
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
	if NormalNetworkConditions() {
		minFilter.minIndependentReliabilityWeights = map[int]float64{
			1: float64(0.99),
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

	// migration: set each client score to the lowest lookback index index
	migrateClientScore := func(clientScore *ClientScore) {
		lookbackIndexes := maps.Keys(clientScore.LookbackClientScores)
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

		for _, rankMode := range maps.Keys(clientScore.Scores) {
			passesMinimum := true
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
		netReliabilityWeight := float64(0)
		for _, clientScore := range s {
			if clientScore.PassesMinimums[rankMode] || forceMinimum {
				netReliabilityWeight += clientScore.ReliabilityWeight
				clientScores = append(clientScores, clientScore)
			}
		}

		filter = &ClientFilter{
			Count:                len(clientScores),
			NetReliabilityWeight: netReliabilityWeight,
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
	clientLocationIds = append(clientLocationIds, maps.Values(countryCodeLocationIds())...)

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
							pipe := r.TxPipeline()

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
		}, wg.Done)
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

		pipe := r.TxPipeline()
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

		keys := maps.Keys(sampleKeyCounts)
		mathrand.Shuffle(len(keys), func(i int, j int) {
			keys[i], keys[j] = keys[j], keys[i]
		})

		samples := []*redis.StringCmd{}
		netCount := 0

		pipe = r.TxPipeline()
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
				clientScores[clientScore.ClientId] = clientScore
			}
		}
	})

	return
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
		loadMillis := float64(loadEndTime.Sub(loadStartTime)/time.Nanosecond) / (1000.0 * 1000.0)
		if 50.0 <= loadMillis {
			glog.Infof("[nclm]findproviders2 load %.2fms (%d)\n", loadMillis, len(clientScores))
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

		clientIds := maps.Keys(clientScores)
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

		// output in order of `clientIds`
		for _, clientId := range clientIds {
			clientScore := clientScores[clientId]
			provider := &FindProvidersProvider{
				ClientId:                   clientId,
				Tier:                       clientScore.Tiers[rankMode],
				EstimatedBytesPerSecond:    clientScore.MaxBytesPerSecond,
				HasEstimatedBytesPerSecond: clientScore.HasSpeedTest,
			}
			providers = append(providers, provider)
		}
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
	// return &CreateProviderSpecResult{
	// 	Specs: []*ProviderSpec{},
	// }, nil
	// FIXME
	return nil, fmt.Errorf("Not implemented.")
}
