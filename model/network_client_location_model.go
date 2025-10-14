package model

import (
	"context"
	// "encoding/hex"
	"strings"
	"sync"
	"time"

	// "errors"
	"bytes"
	"encoding/gob"
	"fmt"
	// "math"
	mathrand "math/rand"
	"slices"

	"github.com/golang/glog"

	"golang.org/x/exp/maps"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/search"
	"github.com/urnetwork/server/session"
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
		// server.Logger().Printf("Create promoted group %s\n", name)
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
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
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

		server.RaisePgResult(tx.Exec(
			ctx,
			`
            DELETE FROM location_group
            WHERE location_group_name = $1
            `,
			locationGroup.Name,
		))

		locationGroupId := server.NewId()
		locationGroup.LocationGroupId = locationGroupId

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
			locationGroup.LocationGroupId,
			locationGroup.Name,
			locationGroup.Promoted,
		))

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
		            network_id
                )
                VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
                ON CONFLICT (connection_id) DO UPDATE
                SET
                    client_id = $2,
                    city_location_id = $3,
                    region_location_id = $4,
                    country_location_id = $5,
                    net_type_hosting = $6,
                    net_type_privacy = $7,
                    net_type_virtual = $8,
                    network_id = $9
            `,
			connectionId,
			clientId,
			cityLocationId,
			regionLocationId,
			countryLocationId,
			connectionLocationScores.NetTypeHosting,
			connectionLocationScores.NetTypePrivacy,
			connectionLocationScores.NetTypeVirtual,
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
	return fmt.Sprintf("client_location_l_%s", locationId)
}

func initialClientLocationsKey() string {
	return fmt.Sprintf("initial_client_locations")
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
	        	client_connection_reliability_score.client_id = network_client_location_reliability.client_id

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
                    location.location_id,
                    location.location_type,
                    location.location_name,
                    location.city_location_id,
                    location.region_location_id,
                    location.country_location_id,
                    location.country_code
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

		result, err = tx.Query(
			ctx,
			`
                SELECT
                    location_group.location_group_id,
                    location_group.location_group_name,
                    location_group.promoted
                FROM location_group
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
	})

	glog.Info("[nclm]updated %d client locations, removed %d, and updated initial\n", len(clientLocations), len(removeClientLocations))

	return
}

func loadClientLocations(
	ctx context.Context,
	locationIds map[server.Id]bool,
) (clientLocations map[server.Id]*ClientLocation, returnErr error) {
	server.Redis(ctx, func(r server.RedisClient) {
		load := func(locationIds map[server.Id]bool, clientLocations map[server.Id]*ClientLocation) error {
			pipe := r.TxPipeline()

			clientLocationCmds := map[server.Id]*redis.StringCmd{}

			for locationId, _ := range locationIds {
				v := pipe.Get(ctx, clientLocationKey(locationId))
				clientLocationCmds[locationId] = v
			}

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

		maxSearchDistance := 2
		locationSearchResults := locationSearch().AroundIds(
			session.Ctx,
			findLocations.Query,
			maxSearchDistance,
			search.OptMostLikley(30),
		)

		locationIds := map[server.Id]bool{}
		for locationId, _ := range locationSearchResults {
			locationIds[locationId] = true
		}
		clientLocations, err := loadClientLocations(session.Ctx, locationIds)
		if err != nil {
			return nil, err
		}

		locationResults := []*LocationResult{}

		for locationId, clientLocation := range clientLocations {
			locationResult := &LocationResult{
				LocationId:        clientLocation.LocationId,
				LocationType:      clientLocation.LocationType,
				Name:              clientLocation.Name,
				CityLocationId:    clientLocation.CityLocationId,
				RegionLocationId:  clientLocation.RegionLocationId,
				CountryLocationId: clientLocation.CountryLocationId,
				CountryCode:       clientLocation.CountryCode,
				ProviderCount:     clientLocation.ClientCount,
			}

			if r, ok := locationSearchResults[locationId]; ok {
				locationResult.MatchDistance = r.ValueDistance
			} else {
				locationResult.MatchDistance = maxSearchDistance + 1
			}

			locationResults = append(locationResults, locationResult)
		}

		return &FindLocationsResult{
			Locations: locationResults,
			Groups:    []*LocationGroupResult{},
			Devices:   []*LocationDeviceResult{},
		}, nil
	}
}

func GetProviderLocations(
	session *session.ClientSession,
) (*FindLocationsResult, error) {
	initialClientLocations, err := loadInitialClientLocations(session.Ctx)
	if err != nil {
		return nil, err
	}

	locationResults := []*LocationResult{}
	locationGroupResults := []*LocationGroupResult{}

	for _, clientLocation := range initialClientLocations.Locations {
		locationResult := &LocationResult{
			LocationId:        clientLocation.LocationId,
			LocationType:      clientLocation.LocationType,
			Name:              clientLocation.Name,
			CityLocationId:    clientLocation.CityLocationId,
			RegionLocationId:  clientLocation.RegionLocationId,
			CountryLocationId: clientLocation.CountryLocationId,
			CountryCode:       clientLocation.CountryCode,
			ProviderCount:     clientLocation.ClientCount,
		}
		locationResults = append(locationResults, locationResult)
	}
	for _, clientLocationGroup := range initialClientLocations.LocationGroups {
		locationGroupResult := &LocationGroupResult{
			LocationGroupId: clientLocationGroup.LocationGroupId,
			Name:            clientLocationGroup.Name,
			Promoted:        clientLocationGroup.Promoted,
		}
		locationGroupResults = append(locationGroupResults, locationGroupResult)
	}

	return &FindLocationsResult{
		Locations: locationResults,
		Groups:    locationGroupResults,
		Devices:   []*LocationDeviceResult{},
	}, nil
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
	BestAvailable   *bool      `json:"best_available,omitempty"`
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
}

type FindProviders2Result struct {
	Providers []*FindProvidersProvider `json:"providers"`
}

type FindProvidersProvider struct {
	ClientId                   server.Id `json:"client_id"`
	EstimatedBytesPerSecond    ByteCount `json:"estimated_bytes_per_second"`
	HasEstimatedBytesPerSecond bool      `json:"has_estimated_bytes_per_second"`
	Tier                       int       `json:"tier"`
}

type ClientScore struct {
	ClientId                 server.Id
	NetworkId                server.Id
	Scores                   map[string]int
	ReliabilityWeight        float32
	Tiers                    map[string]int
	MinRelativeLatencyMillis int
	MaxBytesPerSecond        ByteCount
	HasLatencyTest           bool
	HasSpeedTest             bool
}

// scores are [0, max], where 0 is best
const MaxClientScore = 50
const ClientScoreSampleCount = 200

// FIXME store gob lists randomly allocated to bucket sizes N
// FIXME store the list[i] and total bucket size per location_id, location_group_id

func clientScoreLocationCountsKey(locationId server.Id) string {
	return fmt.Sprintf("client_score_l_%s", locationId)
}

func clientScoreLocationGroupCountsKey(locationGroupId server.Id) string {
	return fmt.Sprintf("client_score_g_%s", locationGroupId)
}

func clientScoreLocationSampleKey(locationId server.Id, index int) string {
	return fmt.Sprintf("client_score_g_%s_%d", locationId, index)
}

func clientScoreLocationGroupSampleKey(locationGroupId server.Id, index int) string {
	return fmt.Sprintf("client_score_g_%s_%d", locationGroupId, index)
}

func UpdateClientScores(ctx context.Context, ttl time.Duration) (returnErr error) {
	locationClientScores := map[server.Id]map[server.Id]*ClientScore{}
	locationGroupClientScores := map[server.Id]map[server.Id]*ClientScore{}

	scorePerTier := 20
	relativeLatencyMillisThreshold := 100
	relativeLatencyMillisPerScore := 20
	bytesPerSecondThreshold := 1 * Mib
	bytesPerSecondPerScore := 200 * Kib
	missingLatencyScore := 10
	missingSpeedScore := 10

	setScore := func(
		clientScore *ClientScore,
		netTypeScore int,
		netTypeScoreSpeed int,
		minRelativeLatencyMillis int,
		maxBytesPerSecond ByteCount,
		hasLatencyTest bool,
		hasSpeedTest bool,
	) {
		scoreAdjust := 0

		if hasLatencyTest {
			if d := minRelativeLatencyMillis - relativeLatencyMillisThreshold; 0 < d {
				scoreAdjust += (d + relativeLatencyMillisPerScore/2) / relativeLatencyMillisPerScore
			}
		} else {
			scoreAdjust += missingLatencyScore
		}

		if hasSpeedTest {
			if d := bytesPerSecondThreshold - maxBytesPerSecond; 0 < d {
				scoreAdjust += int((d + bytesPerSecondPerScore/2) / bytesPerSecondPerScore)
			}
		} else {
			scoreAdjust += missingSpeedScore
		}

		clientScore.Tiers[RankModeSpeed] = netTypeScoreSpeed
		clientScore.Scores[RankModeSpeed] = min(
			scorePerTier*netTypeScoreSpeed+scoreAdjust,
			MaxClientScore,
		)

		clientScore.Tiers[RankModeQuality] = netTypeScore
		clientScore.Scores[RankModeQuality] = min(
			scorePerTier*netTypeScore+scoreAdjust,
			MaxClientScore,
		)
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
	            client_connection_reliability_score.reliability_weight

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
				var cityLocationId server.Id
				var regionLocationId server.Id
				var countryLocationId server.Id
				var clientId server.Id
				var networkId server.Id
				var netTypeScore int
				var netTypeScoreSpeed int
				var minRelativeLatencyMillis int
				var maxBytesPerSecond ByteCount
				var hasLatencyTest bool
				var hasSpeedTest bool
				var reliabilityWeight float32
				server.Raise(result.Scan(
					&cityLocationId,
					&regionLocationId,
					&countryLocationId,
					&clientId,
					&networkId,
					&netTypeScore,
					&netTypeScoreSpeed,
					&minRelativeLatencyMillis,
					&maxBytesPerSecond,
					&hasLatencyTest,
					&hasSpeedTest,
					&reliabilityWeight,
				))
				clientScore := &ClientScore{
					ClientId:                 clientId,
					NetworkId:                networkId,
					ReliabilityWeight:        reliabilityWeight,
					MinRelativeLatencyMillis: minRelativeLatencyMillis,
					MaxBytesPerSecond:        maxBytesPerSecond,
					HasLatencyTest:           hasLatencyTest,
					HasSpeedTest:             hasSpeedTest,
					Scores:                   map[string]int{},
					Tiers:                    map[string]int{},
				}

				setScore(
					clientScore,
					netTypeScore,
					netTypeScoreSpeed,
					minRelativeLatencyMillis,
					maxBytesPerSecond,
					hasLatencyTest,
					hasSpeedTest,
				)

				clientScores, ok := locationClientScores[cityLocationId]
				if !ok {
					clientScores = map[server.Id]*ClientScore{}
					locationClientScores[cityLocationId] = clientScores
				}
				clientScores[clientId] = clientScore

				clientScores, ok = locationClientScores[regionLocationId]
				if !ok {
					clientScores = map[server.Id]*ClientScore{}
					locationClientScores[regionLocationId] = clientScores
				}
				clientScores[clientId] = clientScore

				clientScores, ok = locationClientScores[countryLocationId]
				if !ok {
					clientScores = map[server.Id]*ClientScore{}
					locationClientScores[countryLocationId] = clientScores
				}
				clientScores[clientId] = clientScore

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
	                client_connection_reliability_score.reliability_weight

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
				var cityLocationGroupId *server.Id
				var regionLocationGroupId *server.Id
				var countryLocationGroupId *server.Id
				var clientId server.Id
				var networkId server.Id
				var netTypeScore int
				var netTypeScoreSpeed int
				var minRelativeLatencyMillis int
				var maxBytesPerSecond ByteCount
				var hasLatencyTest bool
				var hasSpeedTest bool
				var reliabilityWeight float32
				server.Raise(result.Scan(
					&cityLocationGroupId,
					&regionLocationGroupId,
					&countryLocationGroupId,
					&clientId,
					&networkId,
					&netTypeScore,
					&netTypeScoreSpeed,
					&minRelativeLatencyMillis,
					&maxBytesPerSecond,
					&hasLatencyTest,
					&hasSpeedTest,
					&reliabilityWeight,
				))
				clientScore := &ClientScore{
					ClientId:                 clientId,
					NetworkId:                networkId,
					ReliabilityWeight:        reliabilityWeight,
					MinRelativeLatencyMillis: minRelativeLatencyMillis,
					MaxBytesPerSecond:        maxBytesPerSecond,
					Scores:                   map[string]int{},
					Tiers:                    map[string]int{},
				}

				setScore(
					clientScore,
					netTypeScore,
					netTypeScoreSpeed,
					minRelativeLatencyMillis,
					maxBytesPerSecond,
					hasLatencyTest,
					hasSpeedTest,
				)

				if cityLocationGroupId != nil {
					clientScores, ok := locationGroupClientScores[*cityLocationGroupId]
					if !ok {
						clientScores = map[server.Id]*ClientScore{}
						locationGroupClientScores[*cityLocationGroupId] = clientScores
					}
					clientScores[clientId] = clientScore
				}

				if regionLocationGroupId != nil {
					clientScores, ok := locationGroupClientScores[*regionLocationGroupId]
					if !ok {
						clientScores = map[server.Id]*ClientScore{}
						locationGroupClientScores[*regionLocationGroupId] = clientScores
					}
					clientScores[clientId] = clientScore
				}

				if countryLocationGroupId != nil {
					clientScores, ok := locationGroupClientScores[*countryLocationGroupId]
					if !ok {
						clientScores = map[server.Id]*ClientScore{}
						locationGroupClientScores[*countryLocationGroupId] = clientScores
					}
					clientScores[clientId] = clientScore
				}
			}
		})
	})

	exportClientScores := func(s map[server.Id]*ClientScore) (
		countsBytes []byte,
		samplesBytes [][]byte,
		counts []int,
		samples [][]*ClientScore,
	) {
		clientScores := maps.Values(s)
		mathrand.Shuffle(len(clientScores), func(i int, j int) {
			clientScores[i], clientScores[j] = clientScores[j], clientScores[i]
		})

		n := (len(clientScores) + ClientScoreSampleCount - 1) / ClientScoreSampleCount
		c := (len(clientScores) + n - 1) / n

		counts = make([]int, n)
		samples = make([][]*ClientScore, n)
		samplesBytes = make([][]byte, n)

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

		b := bytes.NewBuffer(nil)
		e := gob.NewEncoder(b)
		e.Encode(counts)
		countsBytes = b.Bytes()

		return
	}

	server.Redis(ctx, func(r server.RedisClient) {
		pipe := r.TxPipeline()

		for locationId, clientScores := range locationClientScores {
			countsBytes, samplesBytes, counts, _ := exportClientScores(clientScores)
			pipe.Set(ctx, clientScoreLocationCountsKey(locationId), countsBytes, ttl)
			for i, sampleBytes := range samplesBytes {
				pipe.Set(ctx, clientScoreLocationSampleKey(locationId, i), sampleBytes, ttl)
			}
			glog.V(2).Infof("[nclm]update client scores location samples(%s)[%d] = %v\n", locationId, len(counts), counts)
		}
		for locationGroupId, clientScores := range locationGroupClientScores {
			countsBytes, samplesBytes, counts, _ := exportClientScores(clientScores)
			pipe.Set(ctx, clientScoreLocationGroupCountsKey(locationGroupId), countsBytes, ttl)
			for i, sampleBytes := range samplesBytes {
				pipe.Set(ctx, clientScoreLocationGroupSampleKey(locationGroupId, i), sampleBytes, ttl)
			}
			glog.V(2).Infof("[nclm]update client scores location group samples(%s)[%d] = %v\n", locationGroupId, len(counts), counts)
		}

		_, returnErr = pipe.Exec(ctx)
	})

	glog.Infof("[nclm]update %d location scores, %d location group scores\n", len(locationClientScores), len(locationGroupClientScores))

	return
}

func loadClientScores(
	ctx context.Context,
	locationIds map[server.Id]bool,
	locationGroupIds map[server.Id]bool,
	n int,
) (clientScores map[server.Id]*ClientScore, returnErr error) {
	server.Redis(ctx, func(r server.RedisClient) {
		pipe := r.TxPipeline()

		locationCounts := map[server.Id]*redis.StringCmd{}
		for locationId, _ := range locationIds {
			v := pipe.Get(ctx, clientScoreLocationCountsKey(locationId))
			locationCounts[locationId] = v
		}
		locationGroupCounts := map[server.Id]*redis.StringCmd{}
		for locationGroupId, _ := range locationGroupIds {
			v := pipe.Get(ctx, clientScoreLocationGroupCountsKey(locationGroupId))
			locationGroupCounts[locationGroupId] = v
		}

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
				sampleKeyCounts[clientScoreLocationSampleKey(locationId, i)] = count
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
				sampleKeyCounts[clientScoreLocationGroupSampleKey(locationGroupId, i)] = count
			}
		}

		keys := maps.Keys(sampleKeyCounts)
		mathrand.Shuffle(len(keys), func(i int, j int) {
			keys[i], keys[j] = keys[j], keys[i]
		})

		pipe = r.TxPipeline()

		samples := []*redis.StringCmd{}
		netCount := 0
		for _, key := range keys {
			if n <= netCount {
				break
			}
			c := sampleKeyCounts[key]
			v := pipe.Get(ctx, key)
			samples = append(samples, v)
			netCount += c
		}

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

	for _, spec := range findProviders2.Specs {
		if spec.LocationId != nil {
			locationIds[*spec.LocationId] = true
		}
		if spec.LocationGroupId != nil {
			locationGroupIds[*spec.LocationGroupId] = true
		}
		if spec.ClientId != nil {
			provider := &FindProvidersProvider{
				ClientId: *(spec.ClientId),
			}
			providers = append(providers, provider)
		}
		if spec.BestAvailable != nil && *spec.BestAvailable {
			// homeLocationId, found := findHomeLocationIdInTx(tx, session)
			homeLocationId, ok := countryCodeLocationIds()["us"]
			if ok {
				locationIds[homeLocationId] = true
			}
		}
	}

	if 0 < len(locationIds) || 0 < len(locationGroupIds) {
		// use a min block size to reduce db activity
		var count int
		var n int
		if findProviders2.ForceCount {
			count = findProviders2.Count
			n = count
		} else {
			count = max(findProviders2.Count, 20)
			n = max(count*10, 1000)
		}

		rankMode := RankModeQuality
		if findProviders2.RankMode != "" {
			rankMode = findProviders2.RankMode
		}

		loadStartTime := time.Now()
		clientScores, err := loadClientScores(
			session.Ctx,
			locationIds,
			locationGroupIds,
			n,
		)
		if err != nil {
			return nil, err
		}
		loadEndTime := time.Now()
		loadMillis := float64(loadEndTime.Sub(loadStartTime)/time.Nanosecond) / (1000.0 * 1000.0)
		if 50.0 <= loadMillis {
			glog.Infof("[nclm]findproviders2 load %.2fms (%d)\n", loadMillis, len(clientScores))
		}

		for _, clientId := range findProviders2.ExcludeClientIds {
			delete(clientScores, clientId)
		}
		// the final hop is excluded
		// intermediaries have net score incremented
		intermediaryScore := MaxClientScore / 10
		for _, destination := range findProviders2.ExcludeDestinations {
			for _, clientId := range destination[:len(destination)-1] {
				if clientScore, ok := clientScores[clientId]; ok {
					clientScore.Scores[rankMode] += intermediaryScore
				}
			}
			delete(clientScores, destination[len(destination)-1])
		}

		clientIds := maps.Keys(clientScores)
		n = min(n, len(clientIds))

		connect.WeightedSelectFunc(clientIds, n, func(clientId server.Id) float32 {
			clientScore := clientScores[clientId]
			return float32(max(0, MaxClientScore-clientScore.Scores[rankMode]))
		})
		clientIds = clientIds[:n]

		connect.WeightedShuffleFunc(clientIds, func(clientId server.Id) float32 {
			clientScore := clientScores[clientId]
			return clientScore.ReliabilityWeight
		})

		// band by tier
		slices.SortStableFunc(clientIds, func(a server.Id, b server.Id) int {
			clientScoreA := clientScores[a]
			clientScoreB := clientScores[b]

			if d := clientScoreA.Tiers[rankMode] - clientScoreB.Tiers[rankMode]; d != 0 {
				return d
			}

			return 0
		})

		// the caller ip is used to match against provider excluded lists
		clientIp, _, err := session.ParseClientIpPort()
		if err != nil {
			return nil, err
		}

		ipInfo, err := server.GetIpInfo(clientIp)
		if err != nil {
			return nil, err
		}

		clientLocationId, ok := countryCodeLocationIds()[ipInfo.CountryCode]
		if !ok {
			glog.Warningf("[nclm]country code \"%s\" is not mapped to a location id.\n", ipInfo.CountryCode)
		}

		activeClientIds := map[server.Id]bool{}
		filterActive := func(clientNetworks map[server.Id]server.Id) {
			server.Tx(session.Ctx, func(tx server.PgTx) {
				server.CreateTempJoinTableInTx(
					session.Ctx,
					tx,
					"temp_client_networks(client_id uuid -> network_id uuid)",
					clientNetworks,
				)

				result, err := tx.Query(
					session.Ctx,
					`
					SELECT
						temp_client_networks.client_id
					FROM temp_client_networks
					INNER JOIN provide_key ON
			            provide_key.provide_mode = $1 AND
			            provide_key.client_id = temp_client_networks.client_id
			        INNER JOIN network_client_connection ON
			        	network_client_connection.connected = true AND
			        	network_client_connection.client_id = temp_client_networks.client_id
			        LEFT JOIN exclude_network_client_location ON
		            	exclude_network_client_location.client_location_id = $2 AND
		                exclude_network_client_location.network_id = temp_client_networks.network_id
		            WHERE
		            	exclude_network_client_location.network_id IS NULL
					`,
					ProvideModePublic,
					clientLocationId,
				)
				server.WithPgResult(result, err, func() {
					for result.Next() {
						var clientId server.Id
						server.Raise(result.Scan(&clientId))
						activeClientIds[clientId] = true
					}
				})
			})
		}

		// filter the list to include only clients that are actively connected and providing
		i := 0
		for i < len(clientIds) && len(activeClientIds) < count {
			clientNetworks := map[server.Id]server.Id{}
			j1 := min(i+2*count, len(clientIds))
			for j := i; j < j1; j += 1 {
				clientId := clientIds[j]
				clientScore := clientScores[clientId]
				clientNetworks[clientId] = clientScore.NetworkId
			}
			filterActive(clientNetworks)
			i = j1
		}

		// output in order of `clientIds`
		for _, clientId := range clientIds[:i] {
			if activeClientIds[clientId] {
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
