package model

import (
    "context"
    "time"
    "strings"
    // "errors"
    "fmt"
    "math"

    "golang.org/x/exp/maps"

    "bringyour.com/bringyour"
    "bringyour.com/bringyour/search"
    "bringyour.com/bringyour/session"
)


const DefaultMaxDistanceFraction = float32(0.6)


var locationSearch = search.NewSearch("location", search.SearchTypeSubstring)
var locationGroupSearch = search.NewSearch("location_group", search.SearchTypeSubstring)


// called from db_migrations to add default locations and groups
func AddDefaultLocations(ctx context.Context, limit int) {
    createCountry := func(countryCode string, country string) {
        location := &Location{
            LocationType: LocationTypeCountry,
            Country: country,
            CountryCode: countryCode,
        }
        CreateLocation(ctx, location)
    }

    createCity := func(countryCode string, country string, region string, city string) {
        location := &Location{
            LocationType: LocationTypeCity,
            Country: country,
            CountryCode: countryCode,
            Region: region,
            City: city,
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
                    CountryCode: v,
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
            Name: name,
            Promoted: promoted,
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
            bringyour.Logger().Printf("Missing country for %s", countryCode)
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
            bringyour.Logger().Printf("[%d/%d] %s, %s\n", countryIndex, countryCount, countryCode, country)
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
                    if 0 < limit && limit < cityIndex {
                        return
                    }
                    bringyour.Logger().Printf("[%d/%d] %s, %s, %s\n", cityIndex, cityCount, countryCode, region, city)
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
        "Nordic": nordic,
        "Strong Privacy Laws and Internet Freedom": []any{
            eu,
            nordic,
            "jp",
            "ca",
            "au",
            // https://www.ncsl.org/technology-and-communication/state-laws-related-to-digital-privacy
            &Location{
                LocationType: LocationTypeRegion,
                Region: "California",
                Country: "United States",
                CountryCode: "us",
            },
            &Location{
                LocationType: LocationTypeRegion,
                Region: "Colorado",
                Country: "United States",
                CountryCode: "us",
            },
            &Location{
                LocationType: LocationTypeRegion,
                Region: "Connecticut",
                Country: "United States",
                CountryCode: "us",
            },
            &Location{
                LocationType: LocationTypeRegion,
                Region: "Virginia",
                Country: "United States",
                CountryCode: "us",
            },
        },
    }
    for name, members := range promotedRegions {
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
        createLocationGroup(false, name, members...)    
    }
}


type LocationType = string
const (
    LocationTypeCity LocationType = "city"
    LocationTypeRegion LocationType = "region"
    LocationTypeCountry LocationType = "country"
)


type Location struct {
    LocationType LocationType
    City string
    Region string
    Country string
    CountryCode string
    LocationId bringyour.Id
    CityLocationId bringyour.Id
    RegionLocationId bringyour.Id
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

func (self *Location) String() string {
    switch self.LocationType {
    case LocationTypeCity:
        return fmt.Sprintf("%s (%s, %s)", self.City, self.Region, self.Country)
    case LocationTypeRegion:
        return fmt.Sprintf("%s (%s)", self.Region, self.Country)
    default:
        return fmt.Sprintf("%s (%s)", self.Country, self.CountryCode)
    }
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
        LocationType: LocationTypeCountry,
        Country: self.Country,
        CountryCode: self.CountryCode,
        LocationId: self.CountryLocationId,
        CountryLocationId: self.CountryLocationId,
    }, nil
}

func (self *Location) RegionLocation() (*Location, error) {
    switch self.LocationType {
    case LocationTypeCity, LocationTypeRegion:
        return &Location{
            LocationType: LocationTypeRegion,
            Region: self.Region,
            Country: self.Country,
            CountryCode: self.CountryCode,
            LocationId: self.RegionLocationId,
            RegionLocationId: self.RegionLocationId,
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
            LocationType: LocationTypeCity,
            City: self.City,
            Region: self.Region,
            Country: self.Country,
            CountryCode: self.CountryCode,
            LocationId: self.CityLocationId,
            CityLocationId: self.CityLocationId,
            RegionLocationId: self.RegionLocationId,
            CountryLocationId: self.CountryLocationId,
        }, nil
    default:
        return nil, fmt.Errorf("Cannot get city from %s.", self.LocationType)
    }
}


func CreateLocation(ctx context.Context, location *Location) {
    var countryLocation *Location
    var regionLocation *Location
    var cityLocation *Location

    var countryCode string
    if location.CountryCode != "" {
        countryCode = strings.ToLower(location.CountryCode)
    } else {
        // use the first two letters of the country
        countryCode = strings.ToLower(string([]rune(location.Country)[0:2]))
    }

    // country
    bringyour.Tx(ctx, func(tx bringyour.PgTx) {
        result, err := tx.Query(
            ctx,
            `
                SELECT
                    location_id,
                    country_code
                FROM location
                WHERE
                    location_type = $1 AND
                    country_code = $2
            `,
            LocationTypeCountry,
            countryCode,
        )
        var locationId bringyour.Id
        var countryCode string
        bringyour.WithPgResult(result, err, func() {
            if result.Next() {
                bringyour.Raise(result.Scan(&locationId, &countryCode))
                countryLocation = &Location{
                    LocationType: LocationTypeCountry,
                    Country: location.Country,
                    CountryCode: countryCode,
                    LocationId: locationId,
                    CountryLocationId: locationId,
                }
            }
        })

        if countryLocation != nil {
            return
        }
        // else create a new location
            
        locationId = bringyour.NewId()
        _, err = tx.Exec(
            ctx,
            `
                INSERT INTO location (
                    location_id,
                    location_type,
                    location_name,
                    country_location_id,
                    country_code
                )
                VALUES ($1, $2, $3, $1, $4)
            `,
            locationId,
            LocationTypeCountry,
            location.Country,
            countryCode,
        )
        bringyour.Raise(err)

        countryLocation = &Location{
            LocationType: LocationTypeCountry,
            Country: location.Country,
            CountryCode: countryCode,
            LocationId: locationId,
            CountryLocationId: locationId,
        }

        // add to the search
        for i, searchStr := range countryLocation.SearchStrings() {
            locationSearch.AddInTx(ctx, search.NormalizeForSearch(searchStr), locationId, i, tx)
        }
    }, bringyour.TxSerializable)

    if location.LocationType == LocationTypeCountry {
        *location = *countryLocation
        return
    }

    // region
    bringyour.Tx(ctx, func(tx bringyour.PgTx) {
        result, err := tx.Query(
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
        var locationId bringyour.Id
        bringyour.WithPgResult(result, err, func() {
            if result.Next() {
                bringyour.Raise(result.Scan(&locationId))
                regionLocation = &Location{
                    LocationType: LocationTypeRegion,
                    Region: location.Region,
                    Country: countryLocation.Country,
                    CountryCode: countryCode,
                    LocationId: locationId,
                    RegionLocationId: locationId,
                    CountryLocationId: countryLocation.LocationId,
                }
            }
        })

        if regionLocation != nil {
            return
        }
        // else create a new location
        
        locationId = bringyour.NewId()

        _, err = tx.Exec(
            ctx,
            `
                INSERT INTO location (
                    location_id,
                    location_type,
                    location_name,
                    region_location_id,
                    country_location_id,
                    country_code
                )
                VALUES ($1, $2, $3, $1, $4, $5)
            `,
            locationId,
            LocationTypeRegion,
            location.Region,
            countryLocation.LocationId,
            countryCode,
        )
        bringyour.Raise(err)

        regionLocation = &Location{
            LocationType: LocationTypeRegion,
            Region: location.Region,
            Country: countryLocation.Country,
            CountryCode: countryCode,
            LocationId: locationId,
            RegionLocationId: locationId,
            CountryLocationId: countryLocation.LocationId,
        }

        // add to the search
        for i, searchStr := range regionLocation.SearchStrings() {
            locationSearch.AddInTx(ctx, search.NormalizeForSearch(searchStr), locationId, i, tx)
        }
    }, bringyour.TxSerializable)

    if location.LocationType == LocationTypeRegion {
        *location = *regionLocation
        return
    }

    // city
    bringyour.Tx(ctx, func(tx bringyour.PgTx) {
        result, err := tx.Query(
            ctx,
            `
                SELECT 
                    location_id
                FROM location
                WHERE
                    location_type = $1 AND
                    country_code = $2 AND
                    location_name = $3 AND
                    country_location_id = $4 AND
                    region_location_id = $5
            `,
            LocationTypeCity,
            countryCode,
            location.City,
            regionLocation.LocationId,
            countryLocation.LocationId,
        )
        var locationId bringyour.Id
        bringyour.WithPgResult(result, err, func() {
            if result.Next() {
                bringyour.Raise(result.Scan(&locationId))
                cityLocation = &Location{
                    LocationType: LocationTypeCity,
                    City: location.City,
                    Region: regionLocation.Region,
                    Country: countryLocation.Country,
                    CountryCode: countryCode,
                    LocationId: locationId,
                    CityLocationId: locationId,
                    RegionLocationId: regionLocation.LocationId,
                    CountryLocationId: countryLocation.LocationId,
                }
            }
        })

        if cityLocation != nil {
            return
        }
        // else create a new location
        
        locationId = bringyour.NewId()

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
                    country_code
                )
                VALUES ($1, $2, $3, $1, $4, $5, $6)
            `,
            locationId,
            LocationTypeCity,
            location.City,
            regionLocation.LocationId,
            countryLocation.LocationId,
            countryCode,
        )
        bringyour.Raise(err)

        cityLocation = &Location{
            LocationType: LocationTypeCity,
            City: location.City,
            Region: regionLocation.Region,
            Country: countryLocation.Country,
            CountryCode: countryLocation.CountryCode,
            LocationId: locationId,
            CityLocationId: locationId,
            RegionLocationId: regionLocation.LocationId,
            CountryLocationId: countryLocation.LocationId,
        }

        // add to the search
        for i, searchStr := range cityLocation.SearchStrings() {
            locationSearch.AddInTx(ctx, search.NormalizeForSearch(searchStr), locationId, i, tx)
        }
    }, bringyour.TxSerializable)

    *location = *cityLocation
}


type LocationGroup struct {
    LocationGroupId bringyour.Id
    Name string
    Promoted bool
    MemberLocationIds []bringyour.Id
}


func CreateLocationGroup(ctx context.Context, locationGroup *LocationGroup) {
    bringyour.Tx(ctx, func(tx bringyour.PgTx) {
        locationGroup.LocationGroupId = bringyour.NewId()

        _, err := tx.Exec(
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
    }, bringyour.TxSerializable)
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
    }, bringyour.TxSerializable)

    return success
}


func SetConnectionLocation(
    ctx context.Context,
    connectionId bringyour.Id,
    locationId bringyour.Id,
) {
    bringyour.Tx(ctx, func(tx bringyour.PgTx) {
        _, err := tx.Exec(
            ctx,
            `
                INSERT INTO network_client_location
                SELECT
                    network_client_connection.connection_id
                    network_client_connection.client_id
                    location.city_location_id
                    location.region_location_id
                    location.country_location_id
                FROM network_client_connection
                WHERE connection_id = $1
                INNER JOIN location ON location.location_id = $2
                ON CONFLICT (connection_id) DO UPDATE
                SET
                    client_id = network_client_connection.client_id,
                    city_location_id = location.city_location_id,
                    region_location_id = location.region_location_id,
                    country_location_id = location.country_location_id
            `,
            connectionId,
            locationId,
        )
        bringyour.Raise(err)
    })
}


type LocationGroupResult struct {
    LocationGroupId bringyour.Id
    Name string
    ProviderCount int
    Promoted bool
    MatchDistance int
}


type LocationResult struct {
    LocationId bringyour.Id `json:"location_id"`
    LocationType LocationType `json:"location_type"`
    Name string `json:"name"`
    CityLocationId *bringyour.Id `json:"city_location_id,omitempty"`
    RegionLocationId *bringyour.Id `json:"region_location_id,omitempty"`
    CountryLocationId *bringyour.Id `json:"country_location_id,omitempty"`
    CountryCode string `json:"country_code"`
    ProviderCount int `json:"provider_count,omitempty"`
    MatchDistance int `json:"match_distance,omitempty"`
}


type FindLocationsArgs struct {
    Query string `json:"query"`
    // the max search distance is `MaxDistanceFraction * len(Query)`
    // in other words `len(Query) * (1 - MaxDistanceFraction)` length the query must match
    MaxDistanceFraction *float32 `json:"max_distance_fraction,omitempty"`
}

type FindLocationsResult struct {
    // this includes groups that show up in the location results
    // all `ProviderCount` are from inside the location results
    // groups are suggestions that can be used to broaden the search
    Groups map[bringyour.Id]*LocationGroupResult `json:"groups"`
    // this includes all parent locations that show up in the location results
    // every `CityId`, `RegionId`, `CountryId` will have an entry
    Locations map[bringyour.Id]*LocationResult `json:"locations"`
}

// search for locations that match query
// match clients for those locations with provide enabled available to `clientId`
// sum number of unique client ids
// (this selects at least one connection because location is joined to the connection)
// result is a list of: location name, location type, location id, match score, number active providers
// args have query and count
// args have location types, which would typically be all (city, region, country, group)
// args have min search threshold
func FindActiveProviderLocations(
    findLocations *FindLocationsArgs,
    session *session.ClientSession,
) *FindLocationsResult {
    var maxDistanceFraction float32
    if findLocations.MaxDistanceFraction != nil {
        maxDistanceFraction = *findLocations.MaxDistanceFraction
    } else {
        maxDistanceFraction = DefaultMaxDistanceFraction
    }
    maxSearchDistance := int(math.Ceil(
        float64(maxDistanceFraction) * float64(len(findLocations.Query)),
    ))
    locationSearchResults := locationSearch.AroundIds(
        session.Ctx,
        search.NormalizeForSearch(findLocations.Query),
        maxSearchDistance,
    )
    locationGroupSearchResults := locationGroupSearch.AroundIds(
        session.Ctx,
        search.NormalizeForSearch(findLocations.Query),
        maxSearchDistance,
    )

    locationResults := map[bringyour.Id]*LocationResult{}
    locationGroupResults := map[bringyour.Id]*LocationGroupResult{}
    
    bringyour.Tx(session.Ctx, func(tx bringyour.PgTx) {
        searchLocationIds := []bringyour.Id{}
        copy(searchLocationIds, maps.Keys(locationSearchResults))
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
                    DISTINCT location_id,
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
                    region_location_id
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
                    country_location_id
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

                INNER JOIN client_provide ON
                    client_provide.client_id = network_client_location.client_id AND
                    client_provide.provide_mode = $1

                INNER JOIN network_client_connection ON
                    network_client_connection.connection_id = network_client_location.connection_id

                LEFT JOIN temp_location_ids find_location_ids_city ON
                    find_location_ids_city.location_id = network_client_location.city_location_id
                LEFT JOIN temp_location_ids find_location_ids_region ON
                    find_location_ids_region.location_id = network_client_location.region_location_id
                LEFT JOIN temp_location_ids find_location_ids_country ON
                    find_location_ids_country.location_id = network_client_location.country_location_id

                WHERE
                    network_client_connection.connected = true AND (
                        find_location_ids_city.location_id IS NO NULL OR
                        find_location_ids_region.location_id IS NO NULL OR
                        find_location_ids_country.location_id IS NO NULL
                    )

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
            "result_location_ids(location_id uuid, client_count int)",
            providerCount,
        )
        result, err = tx.Query(
            session.Ctx,
            `
                SELECT
                    location_id,
                    location_type,
                    location_name,
                    city_location_id,
                    region_location_id,
                    country_location_id,
                    country_code,
                    result_location_ids.client_count,
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
                }
                locationResults[locationResult.LocationId] = locationResult
            }
        })

        result, err = tx.Query(
            session.Ctx,
            `
                SELECT
                    location_group_id,
                    location_group_name,
                    promoted,
                    t.client_count,
                FROM location_group
                INNER JOIN (
                    SELECT
                        location_group_id,
                        SUM(result_location_ids.client_count) AS client_count,
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
                }
                locationGroupResults[locationGroupResult.LocationGroupId] = locationGroupResult
            }
        })
    })

    return &FindLocationsResult{
        Locations: locationResults,
        Groups: locationGroupResults,
    }
}


func GetActiveProviderLocations(
    session *session.ClientSession,
) *FindLocationsResult {
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

                INNER JOIN client_provide ON
                    client_provide.client_id = network_client_location.client_id AND
                    client_provide.provide_mode = $1

                INNER JOIN network_client_connection ON
                    network_client_connection.connection_id = network_client_location.connection_id

                WHERE
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
            "result_location_ids(location_id uuid, client_count int)",
            providerCount,
        )
        result, err = tx.Query(
            session.Ctx,
            `
                SELECT
                    location_id,
                    location_type,
                    location_name,
                    city_location_id,
                    region_location_id,
                    country_location_id,
                    country_code,
                    result_location_ids.client_count,
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
                    location_group_id,
                    location_group_name,
                    promoted,
                    t.client_count,
                FROM location_group
                INNER JOIN (
                    SELECT
                        location_group_id,
                        SUM(result_location_ids.client_count) AS client_count,
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
                locationGroupResults[locationGroupResult.LocationGroupId] = locationGroupResult
            }
        })
    })

    return &FindLocationsResult{
        Locations: locationResults,
        Groups: locationGroupResults,
    }
}


// this just finds locations and groups regardless of whether there are active providers there
// these are locations where there could be providers
func FindLocations(
    ctx context.Context,
    findLocations *FindLocationsArgs,
) *FindLocationsResult {
    var maxDistanceFraction float32
    if findLocations.MaxDistanceFraction != nil {
        maxDistanceFraction = *findLocations.MaxDistanceFraction
    } else {
        maxDistanceFraction = DefaultMaxDistanceFraction
    }
    maxSearchDistance := int(math.Ceil(
        float64(maxDistanceFraction) * float64(len(findLocations.Query)),
    ))
    locationSearchResults := locationSearch.AroundIds(
        ctx,
        search.NormalizeForSearch(findLocations.Query),
        maxSearchDistance,
    )
    locationGroupSearchResults := locationGroupSearch.AroundIds(
        ctx,
        search.NormalizeForSearch(findLocations.Query),
        maxSearchDistance,
    )

    locationResults := map[bringyour.Id]*LocationResult{}
    locationGroupResults := map[bringyour.Id]*LocationGroupResult{}
    
    bringyour.Tx(ctx, func(tx bringyour.PgTx) {
        searchLocationIds := []bringyour.Id{}
        copy(searchLocationIds, maps.Keys(locationSearchResults))
        // extend the locations with the search group members
        bringyour.CreateTempTableInTx(
            ctx,
            tx,
            "find_location_group_ids(location_group_id uuid)",
            maps.Keys(locationGroupSearchResults)...,
        )
        result, err := tx.Query(
            ctx,
            `
                SELECT
                    DISTINCT location_id,
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
            ctx,
            tx,
            "find_location_ids(location_id uuid)",
            searchLocationIds...,
        )

        // extend the locations with all regions and countries
        // this handles the case where the location searched for does not have matches,
        // but the parent locations do

        _, err = tx.Exec(
            ctx,
            `
                INSERT INTO find_location_ids
                SELECT
                    region_location_id
                FROM location
                INNER JOIN find_location_ids ON find_location_ids.location_id = location.city_location_id
                ON CONFLICT DO NOTHING
            `,
        )
        bringyour.Raise(err)

        _, err = tx.Exec(
            ctx,
            `
                INSERT INTO find_location_ids
                SELECT
                    country_location_id
                FROM location
                INNER JOIN find_location_ids ON 
                    find_location_ids.location_id = location.city_location_id OR
                    find_location_ids.location_id = location.region_location_id
                ON CONFLICT DO NOTHING
            `,
        )
        bringyour.Raise(err)

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
                    country_code,
                FROM location
                INNER JOIN result_location_ids ON
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
            ctx,
            `
                SELECT
                    location_group_id,
                    location_group_name,
                    promoted,
                FROM location_group
                INNER JOIN (
                    SELECT
                        DISTINCT location_group_id,
                    FROM location_group_member
                    INNER JOIN result_location_ids ON
                        result_location_ids.location_id = location_group_member.location_id
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
        Locations: locationResults,
        Groups: locationGroupResults,
    }
}


func GetActiveProvidersForLocation(ctx context.Context, locationId bringyour.Id) []bringyour.Id {
    clientIds := []bringyour.Id{}

    bringyour.Db(ctx, func(conn bringyour.PgConn) {
        result, err := conn.Query(
            ctx,
            `
            SELECT
                DISTINCT network_client_location.client_id
            FROM network_client_location

            INNER JOIN client_provide ON
                client_provide.client_id = network_client_location.client_id AND
                client_provide.provide_mode = $1

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


func GetActiveProvidersForLocationGroup(
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

                INNER JOIN client_provide ON
                    client_provide.client_id = network_client_location.client_id AND
                    client_provide.provide_mode = $1

                INNER JOIN network_client_connection ON
                    network_client_connection.connection_id = network_client_location.connection_id

                LEFT JOIN location_group_member location_group_member_city ON
                    location_group_member_city.location_group_id = $2 AND
                    location_group_member_city.location_id = network_client_location.city_location_id

                LEFT JOIN location_group_member location_group_member_region ON
                    location_group_member_city.location_group_id = $2 AND
                    location_group_member_city.location_id = network_client_location.region_location_id

                LEFT JOIN location_group_member location_group_member_country ON
                    location_group_member_city.location_group_id = $2 AND
                    location_group_member_city.location_id = network_client_location.country_location_id

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
                    $2 <= lookup_time
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

