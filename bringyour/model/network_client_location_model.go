package model

import (
	"context"
	"strings"
	"time"

	// "errors"
	"fmt"
	"math"
	mathrand "math/rand"
	"slices"

	"github.com/golang/glog"

	"golang.org/x/exp/maps"

	"bringyour.com/bringyour"
	// "bringyour.com/bringyour/search"
	"bringyour.com/bringyour/session"
)

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

		memberLocationIds := []bringyour.Id{}
		for _, member := range members {
			switch v := member.(type) {
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

		locationGroup := &LocationGroup{
			Name:              name,
			Promoted:          promoted,
			MemberLocationIds: memberLocationIds,
		}
		CreateLocationGroup(ctx, locationGroup)
	}

	// country code -> name
	countries := bringyour.Config.RequireSimpleResource("iso-country-list.yml").Parse()

	// country code -> region -> []city
	cities := bringyour.Config.RequireSimpleResource("city-list.yml").Parse()

	countryCodesToRemoveFromCities := []string{}
	for countryCode, _ := range cities {
		if _, ok := countries[countryCode]; !ok {
			// bringyour.Logger().Printf("Missing country for %s", countryCode)
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

	promotedRegions := map[string][]any{
		// https://www.gov.uk/eu-eea
		"European Union (EU)": eu,
		"Nordic":              nordic,
		StrongPrivacyLaws: []any{
			eu,
			nordic,
			"jp",
			"ca",
			"au",
			// https://www.ncsl.org/technology-and-communication/state-laws-related-to-digital-privacy
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
				Region:       "Virginia",
				Country:      "United States",
				CountryCode:  "us",
			},
		},
	}
	for name, members := range promotedRegions {
		// bringyour.Logger().Printf("Create promoted group %s\n", name)
		createLocationGroup(true, name, members...)
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
		// bringyour.Logger().Printf("Create group %s\n", name)
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
	LocationId        bringyour.Id
	CityLocationId    bringyour.Id
	RegionLocationId  bringyour.Id
	CountryLocationId bringyour.Id
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
	bringyour.Tx(ctx, func(tx bringyour.PgTx) {
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

		bringyour.WithPgResult(result, err, func() {
			if result.Next() {
				var locationId bringyour.Id
				bringyour.Raise(result.Scan(&locationId))
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
			locationId := bringyour.NewId()
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
			bringyour.Raise(err)

			countryLocation = &Location{
				LocationType:      LocationTypeCountry,
				Country:           location.Country,
				CountryCode:       countryCode,
				LocationId:        locationId,
				CountryLocationId: locationId,
			}

			// add to the search
			for i, searchStr := range countryLocation.SearchStrings() {
				locationSearch.AddInTx(ctx, searchStr, locationId, i, tx)
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

		bringyour.WithPgResult(result, err, func() {
			if result.Next() {
				var locationId bringyour.Id
				bringyour.Raise(result.Scan(&locationId))
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

			locationId := bringyour.NewId()

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
			bringyour.Raise(err)

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
				locationSearch.AddInTx(ctx, searchStr, locationId, i, tx)
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

		bringyour.WithPgResult(result, err, func() {
			if result.Next() {
				var locationId bringyour.Id
				bringyour.Raise(result.Scan(&locationId))
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

			locationId := bringyour.NewId()

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
			bringyour.Raise(err)

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
				locationSearch.AddInTx(ctx, searchStr, locationId, i, tx)
			}
		}

		*location = *cityLocation
	})
}

type LocationGroup struct {
	LocationGroupId   bringyour.Id
	Name              string
	Promoted          bool
	MemberLocationIds []bringyour.Id
}

func (self *LocationGroup) SearchStrings() []string {
	return []string{
		self.Name,
	}
}

func CreateLocationGroup(ctx context.Context, locationGroup *LocationGroup) {
	bringyour.Tx(ctx, func(tx bringyour.PgTx) {
		bringyour.RaisePgResult(tx.Exec(
			ctx,
			`
            DELETE FROM location_group_member
            USING location_group
            WHERE 
                location_group.location_group_name = $1 AND 
                location_group_member.location_group_id = location_group.location_group_id
            `,
			locationGroup.Name,
		))

		bringyour.RaisePgResult(tx.Exec(
			ctx,
			`
            DELETE FROM location_group
            WHERE location_group_name = $1
            `,
			locationGroup.Name,
		))

		locationGroupId := bringyour.NewId()
		locationGroup.LocationGroupId = locationGroupId

		bringyour.RaisePgResult(tx.Exec(
			ctx,
			`
                INSERT INTO location_group (
                    location_group_id,
                    location_group_name,
                    promoted
                )
                VALUES ($1, $2, $3)
            `,
			locationGroup.LocationGroupId,
			locationGroup.Name,
			locationGroup.Promoted,
		))

		bringyour.BatchInTx(ctx, tx, func(batch bringyour.PgBatch) {
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

		for i, searchStr := range locationGroup.SearchStrings() {
			locationGroupSearch.AddInTx(ctx, searchStr, locationGroupId, i, tx)
		}
	})
}

func UpdateLocationGroup(ctx context.Context, locationGroup *LocationGroup) bool {
	success := false

	bringyour.Tx(ctx, func(tx bringyour.PgTx) {
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
		bringyour.Raise(err)
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
		bringyour.Raise(err)

		bringyour.BatchInTx(ctx, tx, func(batch bringyour.PgBatch) {
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
	NetTypeVpn     int
	NetTypeProxy   int
	NetTypeTor     int
	NetTypeRelay   int
	NetTypeHosting int
}

func SetConnectionLocation(
	ctx context.Context,
	connectionId bringyour.Id,
	locationId bringyour.Id,
	connectionLocationScores *ConnectionLocationScores,
) {
	bringyour.Tx(ctx, func(tx bringyour.PgTx) {
		result, err := tx.Query(
			ctx,
			`
                SELECT
                    client_id
                FROM network_client_connection 
                WHERE connection_id = $1
            `,
			connectionId,
		)
		var clientId *bringyour.Id
		bringyour.WithPgResult(result, err, func() {
			if result.Next() {
				bringyour.Raise(result.Scan(&clientId))
			}
		})

		if clientId == nil {
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
		var cityLocationId *bringyour.Id
		var regionLocationId *bringyour.Id
		var countryLocationId *bringyour.Id
		bringyour.WithPgResult(result, err, func() {
			if result.Next() {
				bringyour.Raise(result.Scan(
					&cityLocationId,
					&regionLocationId,
					&countryLocationId,
				))
			}
		})

		bringyour.RaisePgResult(tx.Exec(
			ctx,
			`
                INSERT INTO network_client_location (
                    connection_id,
                    client_id,
                    city_location_id,
                    region_location_id,
                    country_location_id,
                    net_type_vpn,
		            net_type_proxy,
		            net_type_tor,
		            net_type_relay,
		            net_type_hosting
                )
                VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
                ON CONFLICT (connection_id) DO UPDATE
                SET
                    client_id = $2,
                    city_location_id = $3,
                    region_location_id = $4,
                    country_location_id = $5,
                    net_type_vpn = $6,
                    net_type_proxy = $7,
                    net_type_tor = $8,
                    net_type_relay = $9,
                    net_type_hosting = $10
            `,
			connectionId,
			clientId,
			cityLocationId,
			regionLocationId,
			countryLocationId,
			connectionLocationScores.NetTypeVpn,
			connectionLocationScores.NetTypeProxy,
			connectionLocationScores.NetTypeTor,
			connectionLocationScores.NetTypeRelay,
			connectionLocationScores.NetTypeHosting,
		))
	})
}

type LocationGroupResult struct {
	LocationGroupId bringyour.Id `json:"location_group_id"`
	Name            string       `json:"name"`
	ProviderCount   int          `json:"provider_count,omitempty"`
	Promoted        bool         `json:"promoted,omitempty"`
	MatchDistance   int          `json:"match_distance,omitempty"`
}

type LocationResult struct {
	LocationId   bringyour.Id `json:"location_id"`
	LocationType LocationType `json:"location_type"`
	Name         string       `json:"name"`
	// FIXME add City, Region, Country names
	CityLocationId    *bringyour.Id `json:"city_location_id,omitempty"`
	RegionLocationId  *bringyour.Id `json:"region_location_id,omitempty"`
	CountryLocationId *bringyour.Id `json:"country_location_id,omitempty"`
	CountryCode       string        `json:"country_code"`
	ProviderCount     int           `json:"provider_count,omitempty"`
	MatchDistance     int           `json:"match_distance,omitempty"`
}

type LocationDeviceResult struct {
	ClientId   bringyour.Id `json:"client_id"`
	DeviceName string       `json:"device_name"`
}

type FindLocationsArgs struct {
	Query string `json:"query"`
	// the max search distance is `MaxDistanceFraction * len(Query)`
	// in other words `len(Query) * (1 - MaxDistanceFraction)` length the query must match
	MaxDistanceFraction       float32 `json:"max_distance_fraction,omitempty"`
	EnableMaxDistanceFraction bool    `json:"enable_max_distance_fraction,omitempty"`
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
}

// search for locations that match query
// match clients for those locations with provide enabled available to `clientId`
// sum number of unique client ids
// (this selects at least one connection because location is joined to the connection)
// result is a list of: location name, location type, location id, match score, number active providers
// args have query and count
// args have location types, which would typically be all (city, region, country, group)
// args have min search threshold
// FIXME (brien) I think there is a bug in this function. Needs more testing
func FindProviderLocations(
	findLocations *FindLocationsArgs,
	session *session.ClientSession,
) (*FindLocationsResult, error) {
	var maxDistanceFraction float32
	if findLocations.EnableMaxDistanceFraction {
		maxDistanceFraction = findLocations.MaxDistanceFraction
	} else {
		maxDistanceFraction = DefaultMaxDistanceFraction
	}
	maxSearchDistance := int(math.Ceil(
		float64(maxDistanceFraction) * float64(len(findLocations.Query)),
	))
	locationSearchResults := locationSearch.AroundIds(
		session.Ctx,
		findLocations.Query,
		maxSearchDistance,
	)
	locationGroupSearchResults := locationGroupSearch.AroundIds(
		session.Ctx,
		findLocations.Query,
		maxSearchDistance,
	)

	// bringyour.Logger().Printf("Found location search results: %v\n", locationSearchResults)
	// bringyour.Logger().Printf("Found location group results: %v\n", locationGroupSearchResults)

	locationResults := map[bringyour.Id]*LocationResult{}
	locationGroupResults := map[bringyour.Id]*LocationGroupResult{}

	bringyour.Tx(session.Ctx, func(tx bringyour.PgTx) {
		searchLocationIds := []bringyour.Id{}
		for searchLocationId, _ := range locationSearchResults {
			searchLocationIds = append(searchLocationIds, searchLocationId)
		}
		// extend the locations with the search group members
		bringyour.CreateTempTableInTx(
			session.Ctx,
			tx,
			"find_location_group_ids(location_group_id uuid)",
			maps.Keys(locationGroupSearchResults)...,
		)
		result, err := tx.Query(
			session.Ctx,
			`
                SELECT
                    DISTINCT location_group_member.location_id
                FROM location_group_member
                INNER JOIN find_location_group_ids ON
                    find_location_group_ids.location_group_id = location_group_member.location_group_id
            `,
		)
		bringyour.WithPgResult(result, err, func() {
			for result.Next() {
				var locationId bringyour.Id
				bringyour.Raise(result.Scan(&locationId))
				searchLocationIds = append(searchLocationIds, locationId)
			}
		})

		// bringyour.Logger().Printf("Search location ids: %v\n", searchLocationIds)
		// for _, searchLocationId := range searchLocationIds {
		// bringyour.Logger().Printf("  Search location id: %s\n", searchLocationId.String())
		// }

		bringyour.CreateTempTableInTx(
			session.Ctx,
			tx,
			"find_location_ids(location_id uuid)",
			searchLocationIds...,
		)

		// extend the locations with all regions and countries
		// this handles the case where the location searched for does not have matches,
		// but the parent locations do

		_, err = tx.Exec(
			session.Ctx,
			`
                INSERT INTO find_location_ids
                SELECT
                    DISTINCT location.region_location_id
                FROM location
                INNER JOIN find_location_ids ON find_location_ids.location_id = location.city_location_id
                ON CONFLICT DO NOTHING
            `,
		)
		bringyour.Raise(err)

		_, err = tx.Exec(
			session.Ctx,
			`
                INSERT INTO find_location_ids
                SELECT
                    DISTINCT location.country_location_id
                FROM location
                INNER JOIN find_location_ids ON 
                    find_location_ids.location_id = location.city_location_id OR
                    find_location_ids.location_id = location.region_location_id
                ON CONFLICT DO NOTHING
            `,
		)
		bringyour.Raise(err)

		result, err = tx.Query(
			session.Ctx,
			`
                SELECT
                    COUNT(DISTINCT network_client_location.client_id) AS client_count,
                    network_client_location.city_location_id,
                    network_client_location.region_location_id,
                    network_client_location.country_location_id

                FROM network_client_location

                INNER JOIN provide_key ON
                    provide_key.client_id = network_client_location.client_id AND
                    provide_key.provide_mode = $1

                INNER JOIN network_client_connection ON
                    network_client_connection.connection_id = network_client_location.connection_id AND
                    network_client_connection.connected = true

                LEFT JOIN find_location_ids find_location_ids_city ON
                    find_location_ids_city.location_id = network_client_location.city_location_id
                LEFT JOIN find_location_ids find_location_ids_region ON
                    find_location_ids_region.location_id = network_client_location.region_location_id
                LEFT JOIN find_location_ids find_location_ids_country ON
                    find_location_ids_country.location_id = network_client_location.country_location_id

                WHERE
                    find_location_ids_city.location_id IS NOT NULL OR
                    find_location_ids_region.location_id IS NOT NULL OR
                    find_location_ids_country.location_id IS NOT NULL

                GROUP BY
                    network_client_location.city_location_id,
                    network_client_location.region_location_id,
                    network_client_location.country_location_id
            `,
			ProvideModePublic,
		)
		providerCount := map[bringyour.Id]int{}
		bringyour.WithPgResult(result, err, func() {
			for result.Next() {
				var clientCount int
				var cityLocationId *bringyour.Id
				var regionLocationId *bringyour.Id
				var countryLocationId *bringyour.Id

				bringyour.Raise(result.Scan(
					&clientCount,
					&cityLocationId,
					&regionLocationId,
					&countryLocationId,
				))

				if cityLocationId != nil {
					providerCount[*cityLocationId] += clientCount
				}
				if regionLocationId != nil {
					providerCount[*regionLocationId] += clientCount
				}
				if countryLocationId != nil {
					providerCount[*countryLocationId] += clientCount
				}
			}
		})

		// bringyour.Logger().Printf("Found provider counts: %v\n", providerCount)

		bringyour.CreateTempJoinTableInTx(
			session.Ctx,
			tx,
			"result_location_ids(location_id uuid -> client_count int)",
			providerCount,
		)
		result, err = tx.Query(
			session.Ctx,
			`
                SELECT
                    location.location_id,
                    location.location_type,
                    location.location_name,
                    location.city_location_id,
                    location.region_location_id,
                    location.country_location_id,
                    location.country_code,
                    result_location_ids.client_count
                FROM location
                INNER JOIN result_location_ids ON
                    result_location_ids.location_id = location.location_id
            `,
		)
		bringyour.WithPgResult(result, err, func() {
			for result.Next() {
				locationResult := &LocationResult{}
				bringyour.Raise(result.Scan(
					&locationResult.LocationId,
					&locationResult.LocationType,
					&locationResult.Name,
					&locationResult.CityLocationId,
					&locationResult.RegionLocationId,
					&locationResult.CountryLocationId,
					&locationResult.CountryCode,
					&locationResult.ProviderCount,
				))
				// find the match score
				if searchResult, ok := locationSearchResults[locationResult.LocationId]; ok {
					locationResult.MatchDistance = searchResult.ValueDistance
				} else {
					locationResult.MatchDistance = maxSearchDistance + 1
				}
				locationResults[locationResult.LocationId] = locationResult
			}
		})

		result, err = tx.Query(
			session.Ctx,
			`
                SELECT
                    location_group.location_group_id,
                    location_group.location_group_name,
                    location_group.promoted,
                    t.client_count
                FROM location_group
                INNER JOIN (
                    SELECT
                        location_group_member.location_group_id,
                        SUM(result_location_ids.client_count) AS client_count
                    FROM location_group_member
                    INNER JOIN result_location_ids ON
                        result_location_ids.location_id = location_group_member.location_id
                    GROUP BY location_group_id
                ) t ON t.location_group_id = location_group.location_group_id
            `,
		)
		bringyour.WithPgResult(result, err, func() {
			for result.Next() {
				locationGroupResult := &LocationGroupResult{}
				bringyour.Raise(result.Scan(
					&locationGroupResult.LocationGroupId,
					&locationGroupResult.Name,
					&locationGroupResult.Promoted,
					&locationGroupResult.ProviderCount,
				))
				// find the match score
				if searchResult, ok := locationGroupSearchResults[locationGroupResult.LocationGroupId]; ok {
					locationGroupResult.MatchDistance = searchResult.ValueDistance
				} else {
					locationGroupResult.MatchDistance = maxSearchDistance + 1
				}
				locationGroupResults[locationGroupResult.LocationGroupId] = locationGroupResult
			}
		})
	})

	return &FindLocationsResult{
		Locations: maps.Values(locationResults),
		Groups:    maps.Values(locationGroupResults),
		Devices:   findLocationDevices(session.Ctx, findLocations),
	}, nil
}

func GetProviderLocations(
	session *session.ClientSession,
) (*FindLocationsResult, error) {
	locationResults := map[bringyour.Id]*LocationResult{}
	locationGroupResults := map[bringyour.Id]*LocationGroupResult{}

	bringyour.Tx(session.Ctx, func(tx bringyour.PgTx) {
		result, err := tx.Query(
			session.Ctx,
			`
                SELECT
                    COUNT(DISTINCT network_client_location.client_id) AS client_count,
                    network_client_location.city_location_id,
                    network_client_location.region_location_id,
                    network_client_location.country_location_id

                FROM network_client_location

                INNER JOIN provide_key ON
                    provide_key.client_id = network_client_location.client_id AND
                    provide_key.provide_mode = $1

                INNER JOIN network_client_connection ON
                    network_client_connection.connection_id = network_client_location.connection_id AND
                    network_client_connection.connected = true

                GROUP BY
                    network_client_location.city_location_id,
                    network_client_location.region_location_id,
                    network_client_location.country_location_id
            `,
			ProvideModePublic,
		)
		providerCount := map[bringyour.Id]int{}
		bringyour.WithPgResult(result, err, func() {
			for result.Next() {
				var clientCount int
				var cityLocationId *bringyour.Id
				var regionLocationId *bringyour.Id
				var countryLocationId *bringyour.Id

				bringyour.Raise(result.Scan(
					&clientCount,
					&cityLocationId,
					&regionLocationId,
					&countryLocationId,
				))

				if cityLocationId != nil {
					providerCount[*cityLocationId] += clientCount
				}
				if regionLocationId != nil {
					providerCount[*regionLocationId] += clientCount
				}
				if countryLocationId != nil {
					providerCount[*countryLocationId] += clientCount
				}
			}
		})

		bringyour.CreateTempJoinTableInTx(
			session.Ctx,
			tx,
			"result_location_ids(location_id uuid -> client_count int)",
			providerCount,
		)
		result, err = tx.Query(
			session.Ctx,
			`
                SELECT
                    location.location_id,
                    location.location_type,
                    location.location_name,
                    location.city_location_id,
                    location.region_location_id,
                    location.country_location_id,
                    location.country_code,
                    result_location_ids.client_count
                FROM location
                INNER JOIN result_location_ids ON
                    result_location_ids.location_id = location.location_id
            `,
		)
		bringyour.WithPgResult(result, err, func() {
			for result.Next() {
				locationResult := &LocationResult{}
				bringyour.Raise(result.Scan(
					&locationResult.LocationId,
					&locationResult.LocationType,
					&locationResult.Name,
					&locationResult.CityLocationId,
					&locationResult.RegionLocationId,
					&locationResult.CountryLocationId,
					&locationResult.CountryCode,
					&locationResult.ProviderCount,
				))
				locationResults[locationResult.LocationId] = locationResult
			}
		})

		result, err = tx.Query(
			session.Ctx,
			`
                SELECT
                    location_group.location_group_id,
                    location_group.location_group_name,
                    location_group.promoted,
                    t.client_count
                FROM location_group
                INNER JOIN (
                    SELECT
                        location_group_member.location_group_id,
                        SUM(result_location_ids.client_count) AS client_count
                    FROM location_group_member
                    INNER JOIN result_location_ids ON
                        result_location_ids.location_id = location_group_member.location_id
                    GROUP BY location_group_member.location_group_id
                ) t ON t.location_group_id = location_group.location_group_id
            `,
		)
		bringyour.WithPgResult(result, err, func() {
			for result.Next() {
				locationGroupResult := &LocationGroupResult{}
				bringyour.Raise(result.Scan(
					&locationGroupResult.LocationGroupId,
					&locationGroupResult.Name,
					&locationGroupResult.Promoted,
					&locationGroupResult.ProviderCount,
				))
				locationGroupResults[locationGroupResult.LocationGroupId] = locationGroupResult
			}
		})
	})

	return &FindLocationsResult{
		Locations: maps.Values(locationResults),
		Groups:    maps.Values(locationGroupResults),
		Devices:   getLocationDevices(session.Ctx),
	}, nil
}

// this just finds locations and groups regardless of whether there are active providers there
// these are locations where there could be providers
func FindLocations(
	findLocations *FindLocationsArgs,
	session *session.ClientSession,
) (*FindLocationsResult, error) {
	var maxDistanceFraction float32
	if findLocations.EnableMaxDistanceFraction {
		maxDistanceFraction = findLocations.MaxDistanceFraction
	} else {
		maxDistanceFraction = DefaultMaxDistanceFraction
	}
	maxSearchDistance := int(math.Ceil(
		float64(maxDistanceFraction) * float64(len(findLocations.Query)),
	))
	locationSearchResults := locationSearch.AroundIds(
		session.Ctx,
		findLocations.Query,
		maxSearchDistance,
	)
	locationGroupSearchResults := locationGroupSearch.AroundIds(
		session.Ctx,
		findLocations.Query,
		maxSearchDistance,
	)

	locationResults := map[bringyour.Id]*LocationResult{}
	locationGroupResults := map[bringyour.Id]*LocationGroupResult{}

	bringyour.Tx(session.Ctx, func(tx bringyour.PgTx) {
		searchLocationIds := []bringyour.Id{}
		for searchLocationId, _ := range locationSearchResults {
			searchLocationIds = append(searchLocationIds, searchLocationId)
		}
		// extend the locations with the search group members
		bringyour.CreateTempTableInTx(
			session.Ctx,
			tx,
			"find_location_group_ids(location_group_id uuid)",
			maps.Keys(locationGroupSearchResults)...,
		)
		result, err := tx.Query(
			session.Ctx,
			`
                SELECT
                    DISTINCT location_group_member.location_id
                FROM location_group_member
                INNER JOIN find_location_group_ids ON
                    find_location_group_ids.location_group_id = location_group_member.location_group_id
            `,
		)
		bringyour.WithPgResult(result, err, func() {
			for result.Next() {
				var locationId bringyour.Id
				bringyour.Raise(result.Scan(&locationId))
				searchLocationIds = append(searchLocationIds, locationId)
			}
		})

		bringyour.CreateTempTableInTx(
			session.Ctx,
			tx,
			"find_location_ids(location_id uuid)",
			searchLocationIds...,
		)

		// extend the locations with all regions and countries
		// this handles the case where the location searched for does not have matches,
		// but the parent locations do

		_, err = tx.Exec(
			session.Ctx,
			`
                INSERT INTO find_location_ids
                SELECT
                    location.region_location_id
                FROM location
                INNER JOIN find_location_ids ON find_location_ids.location_id = location.city_location_id
                ON CONFLICT DO NOTHING
            `,
		)
		bringyour.Raise(err)

		_, err = tx.Exec(
			session.Ctx,
			`
                INSERT INTO find_location_ids
                SELECT
                    location.country_location_id
                FROM location
                INNER JOIN find_location_ids ON 
                    find_location_ids.location_id = location.city_location_id OR
                    find_location_ids.location_id = location.region_location_id
                ON CONFLICT DO NOTHING
            `,
		)
		bringyour.Raise(err)

		result, err = tx.Query(
			session.Ctx,
			`
                SELECT
                    location.location_id,
                    location.location_type,
                    location.location_name,
                    location.city_location_id,
                    location.region_location_id,
                    location.country_location_id,
                    location.country_code
                FROM location
                INNER JOIN find_location_ids ON
                    find_location_ids.location_id = location.location_id
            `,
		)
		bringyour.WithPgResult(result, err, func() {
			for result.Next() {
				locationResult := &LocationResult{}
				bringyour.Raise(result.Scan(
					&locationResult.LocationId,
					&locationResult.LocationType,
					&locationResult.Name,
					&locationResult.CityLocationId,
					&locationResult.RegionLocationId,
					&locationResult.CountryLocationId,
					&locationResult.CountryCode,
				))
				// find the match score
				if searchResult, ok := locationSearchResults[locationResult.LocationId]; ok {
					locationResult.MatchDistance = searchResult.ValueDistance
				}
				locationResults[locationResult.LocationId] = locationResult
			}
		})

		result, err = tx.Query(
			session.Ctx,
			`
                SELECT
                    location_group.location_group_id,
                    location_group.location_group_name,
                    location_group.promoted
                FROM location_group
                INNER JOIN (
                    SELECT
                        DISTINCT location_group_member.location_group_id
                    FROM location_group_member
                    INNER JOIN find_location_ids ON
                        find_location_ids.location_id = location_group_member.location_id
                ) t ON t.location_group_id = location_group.location_group_id
            `,
		)
		bringyour.WithPgResult(result, err, func() {
			for result.Next() {
				locationGroupResult := &LocationGroupResult{}
				bringyour.Raise(result.Scan(
					&locationGroupResult.LocationGroupId,
					&locationGroupResult.Name,
					&locationGroupResult.Promoted,
				))
				// find the match score
				if searchResult, ok := locationGroupSearchResults[locationGroupResult.LocationGroupId]; ok {
					locationGroupResult.MatchDistance = searchResult.ValueDistance
				}
				locationGroupResults[locationGroupResult.LocationGroupId] = locationGroupResult
			}
		})
	})

	return &FindLocationsResult{
		Locations: maps.Values(locationResults),
		Groups:    maps.Values(locationGroupResults),
		Devices:   findLocationDevices(session.Ctx, findLocations),
	}, nil
}

type FindProvidersArgs struct {
	LocationId       *bringyour.Id  `json:"location_id,omitempty"`
	LocationGroupId  *bringyour.Id  `json:"location_group_id,omitempty"`
	Count            int            `json:"count"`
	ExcludeClientIds []bringyour.Id `json:"exclude_location_ids,omitempty"`
}

type FindProvidersResult struct {
	ClientIds []bringyour.Id `json:"client_ids,omitempty"`
}

func FindProviders(
	findProviders *FindProvidersArgs,
	session *session.ClientSession,
) (*FindProvidersResult, error) {
	clientIds := map[bringyour.Id]bool{}

	if findProviders.LocationId != nil {
		clientIdsForLocation := GetProvidersForLocation(
			session.Ctx,
			*findProviders.LocationId,
		)
		for _, clientId := range clientIdsForLocation {
			clientIds[clientId] = true
		}
	}

	if findProviders.LocationGroupId != nil {
		clientIdsForLocationGroup := GetProvidersForLocationGroup(
			session.Ctx,
			*findProviders.LocationGroupId,
		)
		for _, clientId := range clientIdsForLocationGroup {
			clientIds[clientId] = true
		}
	}

	for _, clientId := range findProviders.ExcludeClientIds {
		delete(clientIds, clientId)
	}

	outClientIds := maps.Keys(clientIds)

	if findProviders.Count < len(clientIds) {
		// sample
		mathrand.Shuffle(len(outClientIds), func(i int, j int) {
			outClientIds[i], outClientIds[j] = outClientIds[j], outClientIds[i]
		})
		outClientIds = outClientIds[:findProviders.Count]
	}

	return &FindProvidersResult{
		ClientIds: outClientIds,
	}, nil
}

type ProviderSpec struct {
	LocationId      *bringyour.Id `json:"location_id,omitempty"`
	LocationGroupId *bringyour.Id `json:"location_group_id,omitempty"`
	ClientId        *bringyour.Id `json:"client_id,omitempty"`
	BestAvailable   *bool         `json:"best_available,omitempty"`
}

type FindProviders2Args struct {
	Specs               []*ProviderSpec  `json:"specs"`
	Count               int              `json:"count"`
	ExcludeClientIds    []bringyour.Id   `json:"exclude_client_ids"`
	ExcludeDestinations [][]bringyour.Id `json:"exclude_destinations"`
}

type FindProviders2Result struct {
	Providers []*FindProvidersProvider `json:"providers"`
}

type FindProvidersProvider struct {
	ClientId                bringyour.Id `json:"client_id"`
	EstimatedBytesPerSecond int          `json:"estimated_bytes_per_second"`
}

func findLocationGroupByNameInTx(
	ctx context.Context,
	name string,
	tx bringyour.PgTx,
) *LocationGroup {

	var locationGroup *LocationGroup

	result, err := tx.Query(
		ctx,
		`
			SELECT
					location_group_id,
					location_group_name,
					promoted

			FROM location_group

			WHERE
					location_group_name = $1

			LIMIT 1
		`,
		name,
	)
	bringyour.WithPgResult(result, err, func() {
		if result.Next() {
			locationGroup = &LocationGroup{}

			bringyour.Raise(result.Scan(
				&locationGroup.LocationGroupId,
				&locationGroup.Name,
				&locationGroup.Promoted,
			))
		}
	})

	return locationGroup
}

func FindProviders2(
	findProviders2 *FindProviders2Args,
	session *session.ClientSession,
) (*FindProviders2Result, error) {
	// set a reasonable default count
	if findProviders2.Count <= 0 {
		findProviders2.Count = 10
	}

	type clientScore struct {
		netTypeScore int
		priority     int
	}

	// score 0 is best
	clientScores := map[bringyour.Id]*clientScore{}
	scoreScale := 2

	bringyour.Tx(session.Ctx, func(tx bringyour.PgTx) {
		locationIds := map[bringyour.Id]bool{}
		locationGroupIds := map[bringyour.Id]bool{}

		for _, spec := range findProviders2.Specs {
			if spec.LocationId != nil {
				locationIds[*spec.LocationId] = true
			}
			if spec.LocationGroupId != nil {
				locationGroupIds[*spec.LocationGroupId] = true
			}
			if spec.ClientId != nil {
				clientScores[*spec.ClientId] = &clientScore{}
			}
			if spec.BestAvailable != nil && *spec.BestAvailable {
				strongPrivacyLawsAndInternetFreedonGroupId := findLocationGroupByNameInTx(session.Ctx, StrongPrivacyLaws, tx)
				if strongPrivacyLawsAndInternetFreedonGroupId != nil {
					locationGroupIds[strongPrivacyLawsAndInternetFreedonGroupId.LocationGroupId] = true
				}
			}
		}

		if 0 < len(locationIds) {
			bringyour.CreateTempTableInTx(
				session.Ctx,
				tx,
				"temp_location_ids(location_id uuid)",
				maps.Keys(locationIds)...,
			)

			result, err := tx.Query(
				session.Ctx,
				`
                SELECT

                    network_client_location.client_id,
                    network_client_location.net_type_score

                FROM network_client_location

                INNER JOIN provide_key ON
                    provide_key.client_id = network_client_location.client_id AND
                    provide_key.provide_mode = $1

                INNER JOIN network_client_connection ON
                    network_client_connection.connection_id = network_client_location.connection_id

                INNER JOIN temp_location_ids ON 
                    temp_location_ids.location_id = network_client_location.city_location_id OR
                    temp_location_ids.location_id = network_client_location.region_location_id OR
                    temp_location_ids.location_id = network_client_location.country_location_id

                WHERE
                    network_client_connection.connected = true

                `,
				ProvideModePublic,
			)
			bringyour.WithPgResult(result, err, func() {
				for result.Next() {
					var clientId bringyour.Id
					var netTypeScore int
					bringyour.Raise(result.Scan(&clientId, &netTypeScore))
					clientScores[clientId] = &clientScore{
						netTypeScore: scoreScale * netTypeScore,
						priority:     mathrand.Int(),
					}
				}
			})
		}

		if 0 < len(locationGroupIds) {
			bringyour.CreateTempTableInTx(
				session.Ctx,
				tx,
				"temp_location_group_ids(location_group_id uuid)",
				maps.Keys(locationGroupIds)...,
			)

			result, err := tx.Query(
				session.Ctx,
				`
                    SELECT

                        network_client_location.client_id,
                        network_client_location.net_type_score

                    FROM network_client_location

                    INNER JOIN provide_key ON
                        provide_key.client_id = network_client_location.client_id AND
                        provide_key.provide_mode = $1

                    INNER JOIN network_client_connection ON
                        network_client_connection.connection_id = network_client_location.connection_id

                    LEFT JOIN location_group_member location_group_member_city ON
                        location_group_member_city.location_id = network_client_location.city_location_id

                    LEFT JOIN location_group_member location_group_member_region ON
                        location_group_member_region.location_id = network_client_location.region_location_id

                    LEFT JOIN location_group_member location_group_member_country ON
                        location_group_member_country.location_id = network_client_location.country_location_id

                    INNER JOIN temp_location_group_ids ON 
                        temp_location_group_ids.location_group_id = location_group_member_city.location_group_id OR
                        temp_location_group_ids.location_group_id = location_group_member_region.location_group_id OR
                        temp_location_group_ids.location_group_id = location_group_member_country.location_group_id

                    WHERE
                        network_client_connection.connected = true AND (
                            location_group_member_city.location_id IS NOT NULL OR
                            location_group_member_region.location_id IS NOT NULL OR
                            location_group_member_country.location_id IS NOT NULL
                        )
                `,
				ProvideModePublic,
			)
			bringyour.WithPgResult(result, err, func() {
				for result.Next() {
					var clientId bringyour.Id
					var netTypeScore int
					bringyour.Raise(result.Scan(&clientId, &netTypeScore))
					clientScores[clientId] = &clientScore{
						netTypeScore: scoreScale * netTypeScore,
						priority:     mathrand.Int(),
					}
				}
			})
		}
	})

	for _, clientId := range findProviders2.ExcludeClientIds {
		delete(clientScores, clientId)
	}
	// the final hop is excluded
	// intermediaries have net score incremented by scale/2
	for _, destination := range findProviders2.ExcludeDestinations {
		for _, clientId := range destination[:len(destination)-1] {
			if clientScore, ok := clientScores[clientId]; ok {
				// fmt.Printf("INCREMENT %s += %d\n", clientId, scoreScale / 2)
				clientScore.netTypeScore += scoreScale / 2
			}
		}
		delete(clientScores, destination[len(destination)-1])
	}

	clientIds := maps.Keys(clientScores)
	slices.SortFunc(clientIds, func(a bringyour.Id, b bringyour.Id) int {
		clientScoreA := clientScores[a]
		clientScoreB := clientScores[b]

		if c := clientScoreA.netTypeScore - clientScoreB.netTypeScore; c != 0 {
			return c
		}
		if c := clientScoreA.priority - clientScoreB.priority; c != 0 {
			return c
		}
		return 0
	})

	if findProviders2.Count < len(clientIds) {
		// keep the highest (score, priority)
		clientIds = clientIds[:findProviders2.Count]
	}

	providers := []*FindProvidersProvider{}
	for _, clientId := range clientIds {
		provider := &FindProvidersProvider{
			ClientId: clientId,
			// TODO
			EstimatedBytesPerSecond: 0,
		}
		providers = append(providers, provider)
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
	// FIXME return empty for now

	return &CreateProviderSpecResult{
		Specs: []*ProviderSpec{},
	}, nil
}

func GetProvidersForLocation(ctx context.Context, locationId bringyour.Id) []bringyour.Id {
	clientIds := []bringyour.Id{}

	bringyour.Db(ctx, func(conn bringyour.PgConn) {
		result, err := conn.Query(
			ctx,
			`
            SELECT
                DISTINCT network_client_location.client_id
            FROM network_client_location

            INNER JOIN provide_key ON
                provide_key.client_id = network_client_location.client_id AND
                provide_key.provide_mode = $1

            INNER JOIN network_client_connection ON
                network_client_connection.connection_id = network_client_location.connection_id

            WHERE
                network_client_connection.connected = true AND (
                    network_client_location.city_location_id = $2 OR
                    network_client_location.region_location_id = $2 OR
                    network_client_location.country_location_id = $2
                )
            `,
			ProvideModePublic,
			locationId,
		)
		bringyour.WithPgResult(result, err, func() {
			for result.Next() {
				var clientId bringyour.Id
				bringyour.Raise(result.Scan(&clientId))
				clientIds = append(clientIds, clientId)
			}
		})
	})

	return clientIds
}

func GetProvidersForLocationGroup(
	ctx context.Context,
	locationGroupId bringyour.Id,
) []bringyour.Id {
	clientIds := []bringyour.Id{}

	bringyour.Db(ctx, func(conn bringyour.PgConn) {
		result, err := conn.Query(
			ctx,
			`
                SELECT
                    DISTINCT network_client_location.client_id
                FROM network_client_location

                INNER JOIN provide_key ON
                    provide_key.client_id = network_client_location.client_id AND
                    provide_key.provide_mode = $1

                INNER JOIN network_client_connection ON
                    network_client_connection.connection_id = network_client_location.connection_id

                LEFT JOIN location_group_member location_group_member_city ON
                    location_group_member_city.location_group_id = $2 AND
                    location_group_member_city.location_id = network_client_location.city_location_id

                LEFT JOIN location_group_member location_group_member_region ON
                    location_group_member_region.location_group_id = $2 AND
                    location_group_member_region.location_id = network_client_location.region_location_id

                LEFT JOIN location_group_member location_group_member_country ON
                    location_group_member_country.location_group_id = $2 AND
                    location_group_member_country.location_id = network_client_location.country_location_id

                WHERE
                    network_client_connection.connected = true AND (
                        location_group_member_city.location_id IS NOT NULL OR
                        location_group_member_region.location_id IS NOT NULL OR
                        location_group_member_country.location_id IS NOT NULL
                    )
            `,
			ProvideModePublic,
			locationGroupId,
		)
		bringyour.WithPgResult(result, err, func() {
			for result.Next() {
				var clientId bringyour.Id
				bringyour.Raise(result.Scan(&clientId))
				clientIds = append(clientIds, clientId)
			}
		})
	})

	return clientIds
}

func findLocationDevices(ctx context.Context, findLocations *FindLocationsArgs) []*LocationDeviceResult {
	// if query is a udid, include a raw client id

	devices := []*LocationDeviceResult{}

	if clientId, err := bringyour.ParseId(findLocations.Query); err == nil {
		device := &LocationDeviceResult{
			ClientId:   clientId,
			DeviceName: fmt.Sprintf("%s", clientId),
		}
		devices = append(devices, device)
	}

	return devices
}

func getLocationDevices(ctx context.Context) []*LocationDeviceResult {
	return []*LocationDeviceResult{}
}

func GetLatestIpLocationLookupResult(
	ctx context.Context,
	ipStr string,
	earliestResultTime time.Time,
) string {
	var resultJson string
	bringyour.Db(ctx, func(conn bringyour.PgConn) {
		result, err := conn.Query(
			ctx,
			`
                SELECT
                    result_json
                FROM ip_location_lookup
                WHERE
                    ip_address = $1 AND
                    $2 <= lookup_time AND
                    valid
                ORDER BY lookup_time DESC
                LIMIT 1
            `,
			ipStr,
			earliestResultTime,
		)
		bringyour.WithPgResult(result, err, func() {
			if result.Next() {
				bringyour.Raise(result.Scan(&resultJson))
			}
		})
	})
	return resultJson
}

func SetIpLocationLookupResult(
	ctx context.Context,
	ipStr string,
	result string,
) {
	bringyour.Tx(ctx, func(tx bringyour.PgTx) {
		_, err := tx.Exec(
			ctx,
			`
                INSERT INTO ip_location_lookup (
                    ip_address,
                    result_json
                )
                VALUES ($1, $2)
            `,
			ipStr,
			result,
		)
		bringyour.Raise(err)
	})
}
