

// https://github.com/mozillazg/go-unidecode


var locationSearch := search.NewSearch("location", search.SearchTypeSubstring)
var locationGroupSearch := search.NewSearch("location_group", search.SearchTypeSubstring)


// all location values and queries in the search index should use this
func NormalizeForSearch(value string) string {
	norm := strings.TrimSpace(value)
	// convert unicode chars to their latin1 equivalents
	norm = unidecode.Unidecode(value)
	norm = strings.ToLower(norm)
	// replace whitespace with a single space
	re := regexp.MustCompile("\s+")
	norm = re.ReplaceAllString(norm, " ")
	return norm
}


// called from db_migrations to add default locations and groups
func AddDefaultLocations(ctx context.Context) {
	ctx := context.Background()

	func createCountry(countryCode string, name string) {
		location := &Location{
			LocationType: Country,
			Country: name,
			CountryCode: countryCode,
		}
		CreateLocation(ctx, location)
	}

	func createCity(countryCode string, region string, name string) {
		location := &Location{
			LocationType: City,
			CountryCode: countryCode,
			Region: region,
			City: name,
		}
		CreateLocation(ctx, location)
	}

	func createLocationGroup(promoted bool, name string, ...members any) {
		// member can be a country code, Location, or *Location,

		memberLocationIds := []Id{}
		for _, member := range members {
			switch v := member.(type) {
			case string:
				// country code
				location := &Location{
					LocationType: Country,
					CountryCode: v,
				}
				CreateLocation(ctx, location)
				memberLocationIds = append(memberLocationIds, location.LocationId)
			case Location:
				CreateLocation(ctx, &v)
				memberLocationIds = append(memberLocationIds, location.LocationId)
			case *Location:
				CreateLocation(ctx, v)
				memberLocationIds = append(memberLocationIds, location.LocationId)
			}
		}

		locationGroup := &LocationGroup{
			Name: name,
			Promoted: promoted,
			MemberLocationIds: memberLocationIds,
		}
		CreateLocationGroup(locationGroup)
	}


	// country code -> name
	countries := env.ReadYml("iso-country-list.yml")
	for countryCode, name := range countries {
		createCountry(countryCode, name)
	}

	// cities
	// country code -> region -> city
	cities := env.ReadYml("city-list.yml")
	for countryCode, regions := range cities {
		for region, city := range regions {
			if _, ok := countries[countryCode]; !ok {
				panic(errors.New(fmt.Sprintf("Missing country for %s", countryCode)))
			}
			createCity(countryCode, region, city)
		}
	}


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
				LocationType: Region,
				Region: "California",
				Country: "United States",
				CountryCode: "us",
			},
			&Location{
				LocationType: Region,
				Region: "Colorado",
				Country: "United States",
				CountryCode: "us",
			},
			&Location{
				LocationType: Region,
				Region: "Connecticut",
				Country: "United States",
				CountryCode: "us",
			},
			&Location{
				LocationType: Region,
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


type LocationType string
const (
	City LocationType = "city"
	Region LocationType = "region"
	Country LocationType = "country"
)


type Location struct {
	LocationType LocationType
	City string
	Region string
	Country string
	CountryCode string
	LocationId Id
	CityLocationId Id
	RegionLocationId Id
	CountryLocationId Id
}

func (self *Location) String() string {
	// <city> (<region>, <country>)
	// <region> (<country>)
	// <country> (<code>)
	switch self.LocationType {
	case City:
		return fmt.Sprinf("%s (%s, %s)", self.City, self.Region, self.Country)
	case Region:
		return fmt.Sprintf("%s (%s)", self.Region, self.Country)
	case Country:
		return fmt.Sprintf("%s (%s)", self.Country, self.CountryCode)
	}
}


// FIXME
func (self *Location) SearchStrings() []string {
	// <city>, <country>
	// <city>, <code>
	// <city>, <region>
	// <region>, <country>
	// <region>, <code>
	// <country> (<code>)
	// <code>
	switch self.LocationType {
	case City:
		return []string{
			fmt.Sprinf("%s, %s", self.City, self.Country),
			fmt.Sprinf("%s, %s", self.City, self.CountryCode),
			fmt.Sprinf("%s, %s", self.City, self.Region),
		}
	case Region:
		return []string{
			fmt.Sprinf("%s, %s", self.Region, self.Country),
			fmt.Sprinf("%s, %s", self.Region, self.CountryCode),
		}
	case Country:
		return []string{
			fmt.Sprinf("%s (%s)", self.Country, self.CountryCode),
			fmt.Sprinf("%s", self.CountryCode),
		}
	}
}

func (self *Location) Country() (*Location, error) {
	switch self.LocationType {
	case City, Region, Country:
		return &Location{
			LocationType: Country,
			Country: self.Country,
			CountryCode: self.CountryCode,
			LocationId: self.CountryLocationId,
			CountryLocationId: self.CountryLocationId,
		}, nil
	default:
		return nil, errors.New("Cannot get country from %s.", self.LocationType)
	}
}

func (self *Location) Region() (*Location, error) {
	switch self.LocationType {
	case City, Region:
		return &Location{
			LocationType: Region,
			Region: self.Region,
			Country: self.Country,
			CountryCode: self.CountryCode,
			LocationId: self.RegionLocationId,
			RegionLocationId: self.RegionLocationId,
			CountryLocationId: self.CountryLocationId,
		}, nil
	default:
		return nil, errors.New("Cannot get region from %s.", self.LocationType)
	}
}

func (self *Location) City() (*Location, error) {
	switch self.LocationType {
	case City:
		return &Location{
			LocationType: City,
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
		return nil, errors.New("Cannot get city from %s.", self.LocationType)
	}
}


func CreateLocation(ctx context.Context, location *Location) {
	var countryLocation *Location
	var regionLocation *Location
	var cityLocation *Location

	if location.CountryCode != "" {
		countryCode = strings.ToLower(location.CountryCode)
	} else {
		// use the first two letters of the country
		countryCode = strings.ToLower(string([]rune(location.Country)[0:2]))
	}

	// country
	bringyour.Tx(ctx, func(tx bringyour.PgTx) {
		result, err := conn.Query(
			`
				SELECT
					location_id,
					country_code
				FROM location
				WHERE
					location_type = $1 AND
					country_code = $2
			`,
			Country,
			countryCode,
		)
		var locationId Id
		var countryCode string
		bringyour.WithDbResult(result, err, func() {
			if result.Next() {
				bringyour.Raise(result.Scan(&locationId, &countryCode))
				countryLocation = &Location{
					LocationType: Country,
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
			
		locationId = NewId()
		tag, err := conn.Exec(
			`
				INSERT INTO location (
					location_id,
					location_type,
					name,
					country_location_id,
					country_code
				)
				VALUES ($1, $2, $3, $1, $4)
			`,
			locationId,
			Country,
			location.Country,
			countryCode,
		)
		bringyour.Raise(err)

		countryLocation = &Location{
			LocationType: Country,
			Country: location.Country,
			CountryCode: countryCode,
			LocationId: locationId,
			CountryLocationId: locationId,
		}

		// add to the search
		for _, searchStr := range countryLocation.SearchStrings() {
			locationSearch.AddInTx(ctx, NormalizeForSearch(searchStr), locationId, tx)
		}
	}, bringyour.TxSerializable)

	if locationType == Country {
		*location = *countryLocation
		return
	}

	// region
	bringyour.Tx(ctx, func(tx bringyour.PgTx) {
		result, err := conn.Query(
			`
				SELECT
					location_id
				FROM location
				WHERE
					location_type = $1 AND
					country_code = $2 AND
					name = $3 AND
					country_location_id = $4
			`,
			Region,
			countryCode,
			location.Region,
			countryLocation.LocationId,
		)
		var locationId Id
		bringyour.WithDbResult(result, err, func() {
			if result.Next() {
				bringyour.Raise(result.Scan(&locationId))
				regionLocation = &Location{
					LocationType: Region,
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
		
		locationId = NewId()

		tag, err := conn.Exec(
			`
				INSERT INTO location (
					location_id,
					location_type,
					name,
					region_location_id,
					country_location_id,
					country_code
				)
				VALUES ($1, $2, $3, $1, $4, $5)
			`,
			locationId,
			Region,
			location.Country,
			countryLocation.LocationId,
			countryCode,
		)
		bringyour.Raise(err)

		regionLocation = &Location{
			LocationType: Region,
			Region: location.Region,
			Country: countryLocation.Country,
			CountryCode: countryCode,
			LocationId: locationId,
			RegionLocationId: locationId,
			CountryLocationId: countryLocation.LocationId,
		}

		// add to the search
		for _, searchStr := range regionLocation.SearchStrings() {
			locationSearch.AddInTx(ctx, NormalizeForSearch(searchStr), locationId, tx)
		}
	}, bringyour.TxSerializable)

	if locationType == Region {
		*location = *regionLocation
		return
	}

	// city
	bringyour.Tx(ctx, func(tx bringyour.PgTx) {
		result, err := conn.Query(
			`
				SELECT 
					location_id
				FROM location
				WHERE
					location_type = $1 AND
					country_code = $2 AND
					name = $3 AND
					country_location_id = $4 AND
					region_location_id = $5
			`,
			City,
			countryCode,
			location.City,
			regionLocation.LocationId,
			countryLocation.LocationId,
		)
		var locationId Id
		bringyour.WithDbResult(result, err, func() {
			if result.Next() {
				bringyour.Raise(result.Scan(&locationId))
				cityLocation = &Location{
					LocationType: City,
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
		
		locationId = NewId()

		tag, err := conn.Exec(
			`
				INSERT INTO location (
					location_id,
					location_type,
					name,
					city_location_id,
					region_location_id,
					country_location_id,
					country_code
				)
				VALUES ($1, $2, $3, $1, $4, $5, $6)
			`,
			locationId,
			City,
			location.City,
			regionLocation.LocationId,
			countryLocation.LocationId,
			countryCode,
		)
		bringyour.Raise(err)

		cityLocation = &Location{
			LocationType: City,
			City: location.City,
			Region: regionLocation.Region,
			Country: countryLocation.Country,
			CountryCode: countryLocation.CountryCode,
			LocationId: locationId,
			CityLocationId: locationId,
			RegionLocationId: regionLocation.LocationId,
			CountryLocationId: countryCode,
		}

		// add to the search
		for _, searchStr := range cityLocation.SearchStrings() {
			locationSearch.AddInTx(ctx, NormalizeForSearch(searchStr), locationId, tx)
		}
	}, bringyour.TxSerializable)

	*location = *cityLocation
}


type LocationGroup struct {
	LocationGroupId Id
	Name string
	Promoted bool
	MemberLocationIds []Id
}


func CreateLocationGroup(ctx context.Context, locationGroup *LocationGroup) {
	bringyour.Tx(ctx, func(tx bringyour.PgTx) {
		locationGroup.LocationGroupId = NewId()

		_, err := tx.Exec(
			ctx,
			`
				INSERT INTO location_group (
					location_group_id,
					name,
					promoted
				)
				VALUES ($1, $2, $3)
			`,
			locationGroup.LocationGroupId,
			locationGroup.Name,
			locationGroup.Promoted,
		)
		bringyour.Raise(err)

		bringyour.Batch(ctx, conn, func(batch bringyour.PgBatch) {
			for _, locationId := locationGroup.MemberLocationIds {
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
					name = $2,
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
		
		bringyour.Batch(ctx, conn, func(batch bringyour.PgBatch) {
			for _, locationId := locationGroup.MemberLocationIds {
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


func SetConnectionLocation(ctx context.Context, connectionId Id, locationId Id) {
	bringyour.Tx(ctx, func(conn bringyour.PgConn) {
		tag, err := conn.Exec(
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
				ON CONFLICT UPDATE
				SET
					/* connection_id is the conflicting key */
					client_id = network_client_connection.client_id
					city_location_id = location.city_location_id
					region_location_id = location.region_location_id
					country_location_id = location.country_location_id
			`,
			connectionId,
			locationId,
		)
		bringyour.Raise(err)
	})
}


type ProviderLocationType string
const (
	City ProviderLocationType = "city"
	Region ProviderLocationType = "region"
	Country ProviderLocationType = "country"
	Group ProviderLocationType = "group"
)


type LocationGroupResult struct {
	LocationGroupId Id
	Name string
	ProviderCount int
	Promoted bool
	MatchDistance int
}


type LocationResult struct {
	LocationId Id
	LocationType LocationType
	Name string
	CityId *Id
	RegionId *Id
	CountryId *Id
	CountrCode string
	ProviderCount int
	MatchDistance int
}


const DefaultMaxDistanceFraction = float32(0.6)


type FindLocationsArgs struct {
	Query string
	// the max search distance is `MaxDistanceFraction * len(Query)`
	// in other words `len(Query) * (1 - MaxDistanceFraction)` length the query must match
	MaxDistanceFraction *float32
}

type FindLocationsResult struct {
	// this includes groups that show up in the location results
	// all `ProviderCount` are from inside the location results
	// groups are suggestions that can be used to broaden the search
	Groups map[Id]*LocationGroupResult
	// this includes all parent locations that show up in the location results
	// every `CityId`, `RegionId`, `CountryId` will have an entry
	Locations map[Id]*LocationResult
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
	if findActiveProviderLocations.MaxDistanceFraction != nil {
		maxDistanceFraction = *findActiveProviderLocations.MaxDistanceFraction
	} else {
		maxDistanceFraction = DefaultMaxDistanceFraction
	}
	maxSearchDistance := int(math.Ceil(
		maxDistanceFraction * len(findActiveProviderLocations.Query),
	))
	locationSearchResults := locationSearch.AroundIds(
		NormalizeForSearch(findActiveProviderLocations.Query),
		maxSearchDistance,
	)
	locationGroupSearchResults := locationGroupSearch.AroundIds(
		NormalizeForSearch(findActiveProviderLocations.Query),
		maxSearchDistance,
	)

	locationResults := map[Id]*LocationResult{}
	locationGroupResults := map[Id]*LocationGroupResult{}
	
	bringyour.Db(session.Ctc, func(conn bringyour.PgConn) {
		searchLocationIds := []Id{}
		copy(searchLocationIds, maps.Keys(locationSearchResults))
		// extend the locations with the search group members
		bringyour.CreateTempTable(
			session.Ctx,
			conn,
			"find_location_group_ids(location_group_id)",
			maps.Keys(locationGroupSearchResults)...,
		)
		result, err := conn.Query(
			search.Ctx,
			`
				SELECT
					DISTINCT location_id,
				FROM location_group_member
				INNER JOIN find_location_group_ids ON
					find_location_group_ids.location_group_id = location_group_member.location_group_id
			`
		)
		bringyour.WithDbResult(result, err, func() {
			for result.Next() {
				var locationId Id
				bringyour.Raise(result.Scan(&locationId))
				searchLocationIds = append(searchLocationIds, locationId)
			}
		})

		bringyour.CreateTempTable(
			session.Ctx,
			conn,
			"find_location_ids(location_id)",
			searchLocationIds...,
		)

		// extend the locations with all regions and countries
		// this handles the case where the location searched for does not have matches,
		// but the parent locations do

		_, err := conn.Exec(
			`
				INSERT INTO find_location_ids
				SELECT
					region_location_id
				FROM location
				INNER JOIN find_location_ids ON find_location_ids.location_id = location.city_location_id
				ON CONFLICT DO NOTHING
			`
		)
		bringyour.Raise(err)

		_, err := conn.Exec(
			`
				INSERT INTO find_location_ids
				SELECT
					country_location_id
				FROM location
				INNER JOIN find_location_ids ON 
					find_location_ids.location_id = location.city_location_id OR
					find_location_ids.location_id = location.region_location_id
				ON CONFLICT DO NOTHING
			`
		)
		bringyour.Raise(err)

		result, err := conn.Query(
			session.Ctx,
			`
				SELECT
					COUNT(DISTINCT network_client_location.client_id) AS client_count,
					network_client_location.city_location_id,
					network_client_location.region_location_id,
					network_client_location.country_location_id

				FROM network_client_location

				INNER JOIN provide_config ON
					provide_config.client_id = network_client_location.client_id AND
					provide_config.provide_mode = $1

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
		providerCount := map[Id]int{}
		bringyour.WithDbResult(result, err, func() {
			for result.Next() {
				var clientCount int
				var cityLocationId *Id
				var regionLocationId *Id
				var countryLocationId *Id

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


		bringyour.CreateTempJoinTable(
			session.Ctx,
			conn,
			"result_location_ids(location_id, client_count)",
			providerCount,
		)
		result, err := conn.Query(
			session.Ctx,
			`
				SELECT
					location_id,
					location_type,
					name,
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
		bringyour.WithDbResult(result, err, func() {
			for result.Next() {
				locationResult := &LocationResult{}
				bringyour.Raise(bringyour.Scan(
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

		result, err := bringyour.Query(
			session.Ctx,
			`
				SELECT
					location_group_id,
					name,
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
		bringyour.WithDbResult(result, err, func() {
			for result.Next() {
				locationGroupResult := &LocationGroupResult{}
				bringyour.Raise(bringyour.Scan(
					&locationGroupResult.LocationGroupId,
					&locationGroupResult.Name,
					&locationGroupResult.Promoted,
					&locationGroupResult.ProviderCount,
				))
				// find the match score
				if searchResult, ok := locationGroupSearchResults[locationGroupResult.locationGroupId]; ok {
					locationGroupResult.MatchDistance = searchResult.ValueDistance
				}
				locationGroupResults[locationGroupResult.LocationGroupId] = locationGroupResult
			}
		})
	})

	return &FindActiveProviderLocationsResult{
		Locations: locationResults,
		Groups: locationGroupResults,
	}
}


func GetActiveProviderLocations(
	session *session.ClientSession,
) *FindLocationsResult {
	locationResults := map[Id]*LocationResult{}
	locationGroupResults := map[Id]*LocationGroupResult{}

	bringyour.Db(session.Ctx, func(conn bringyour.PgConn) {
		result, err := conn.Query(
			session.Ctx,
			`
				SELECT
					COUNT(DISTINCT network_client_location.client_id) AS client_count,
					network_client_location.city_location_id,
					network_client_location.region_location_id,
					network_client_location.country_location_id

				FROM network_client_location

				INNER JOIN provide_config ON
					provide_config.client_id = network_client_location.client_id AND
					provide_config.provide_mode = $1

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
		providerCount := map[Id]int{}
		bringyour.WithDbResult(result, err, func() {
			for result.Next() {
				var clientCount int
				var cityLocationId *Id
				var regionLocationId *Id
				var countryLocationId *Id

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

		bringyour.CreateTempJoinTable(
			session.Ctx,
			conn,
			"result_location_ids(location_id, client_count)",
			providerCount,
		)
		result, err := conn.Query(
			session.Ctx,
			`
				SELECT
					location_id,
					location_type,
					name,
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
		bringyour.WithDbResult(result, err, func() {
			for result.Next() {
				locationResult := &LocationResult{}
				bringyour.Raise(bringyour.Scan(
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

		result, err := bringyour.Query(
			session.Ctx,
			`
				SELECT
					location_group_id,
					name,
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
		bringyour.WithDbResult(result, err, func() {
			for result.Next() {
				locationGroupResult := &LocationGroupResult{}
				bringyour.Raise(bringyour.Scan(
					&locationGroupResult.LocationGroupId,
					&locationGroupResult.Name,
					&locationGroupResult.Promoted,
					&locationGroupResult.ProviderCount,
				))
				locationGroupResults[locationGroupResult.LocationGroupId] = locationGroupResult
			}
		})
	})

	return &FindActiveProviderLocationsResult{
		Locations: locationResults,
		Groups: locationGroupResults,
	}
}


// this just finds locations and groups regardless of whether there are active providers there
// these are locations where there could be providers
func FindLocations(
	ctx context.Context,
	findLocations *FindLocationsArgs
) *FindLocationsResult {
	var maxDistanceFraction float32
	if findActiveProviderLocations.MaxDistanceFraction != nil {
		maxDistanceFraction = *findActiveProviderLocations.MaxDistanceFraction
	} else {
		maxDistanceFraction = DefaultMaxDistanceFraction
	}
	maxSearchDistance := int(math.Ceil(
		maxDistanceFraction * len(findActiveProviderLocations.Query),
	))
	locationSearchResults := locationSearch.AroundIds(
		NormalizeForSearch(findActiveProviderLocations.Query),
		maxSearchDistance,
	)
	locationGroupSearchResults := locationGroupSearch.AroundIds(
		NormalizeForSearch(findActiveProviderLocations.Query),
		maxSearchDistance,
	)

	locationResults := map[Id]*LocationResult{}
	locationGroupResults := map[Id]*LocationGroupResult{}
	
	bringyour.Db(session.Ctc, func(conn bringyour.PgConn) {
		searchLocationIds := []Id{}
		copy(searchLocationIds, maps.Keys(locationSearchResults))
		// extend the locations with the search group members
		bringyour.CreateTempTable(
			session.Ctx,
			conn,
			"find_location_group_ids(location_group_id)",
			maps.Keys(locationGroupSearchResults)...,
		)
		result, err := conn.Query(
			search.Ctx,
			`
				SELECT
					DISTINCT location_id,
				FROM location_group_member
				INNER JOIN find_location_group_ids ON
					find_location_group_ids.location_group_id = location_group_member.location_group_id
			`
		)
		bringyour.WithDbResult(result, err, func() {
			for result.Next() {
				var locationId Id
				bringyour.Raise(result.Scan(&locationId))
				searchLocationIds = append(searchLocationIds, locationId)
			}
		})

		bringyour.CreateTempTable(
			session.Ctx,
			conn,
			"find_location_ids(location_id)",
			searchLocationIds...,
		)

		// extend the locations with all regions and countries
		// this handles the case where the location searched for does not have matches,
		// but the parent locations do

		_, err := conn.Exec(
			`
				INSERT INTO find_location_ids
				SELECT
					region_location_id
				FROM location
				INNER JOIN find_location_ids ON find_location_ids.location_id = location.city_location_id
				ON CONFLICT DO NOTHING
			`
		)
		bringyour.Raise(err)

		_, err := conn.Exec(
			`
				INSERT INTO find_location_ids
				SELECT
					country_location_id
				FROM location
				INNER JOIN find_location_ids ON 
					find_location_ids.location_id = location.city_location_id OR
					find_location_ids.location_id = location.region_location_id
				ON CONFLICT DO NOTHING
			`
		)
		bringyour.Raise(err)

		result, err := conn.Query(
			session.Ctx,
			`
				SELECT
					location_id,
					location_type,
					name,
					city_location_id,
					region_location_id,
					country_location_id,
					country_code,
				FROM location
				INNER JOIN result_location_ids ON
					find_location_ids.location_id = location.location_id
			`,
		)
		bringyour.WithDbResult(result, err, func() {
			for result.Next() {
				locationResult := &LocationResult{}
				bringyour.Raise(bringyour.Scan(
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

		result, err := bringyour.Query(
			session.Ctx,
			`
				SELECT
					location_group_id,
					name,
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
		bringyour.WithDbResult(result, err, func() {
			for result.Next() {
				locationGroupResult := &LocationGroupResult{}
				bringyour.Raise(bringyour.Scan(
					&locationGroupResult.LocationGroupId,
					&locationGroupResult.Name,
					&locationGroupResult.Promoted,
				))
				// find the match score
				if searchResult, ok := locationGroupSearchResults[locationGroupResult.locationGroupId]; ok {
					locationGroupResult.MatchDistance = searchResult.ValueDistance
				}
				locationGroupResults[locationGroupResult.LocationGroupId] = locationGroupResult
			}
		})
	})

	return &FindActiveProviderLocationsResult{
		Locations: locationResults,
		Groups: locationGroupResults,
	}
}


func GetActiveProvidersForLocation(ctx context.Context, locationId Id) []Id {
	clientIds := []Id{}

	bringyour.Db(ctx, func(conn bringyour.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				DISTINCT network_client_location.client_id
			FROM network_client_location

			INNER JOIN provide_config ON
				provide_config.client_id = network_client_location.client_id AND
				provide_config.provide_mode = $1

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
		bringyour.WithDbResult(result, err, func() {
			for result.Next {
				var clientId Id
				bringyour.Raise(bringyour.Scan(&clientId))
				clientIds = append(clientIds, clientId)
			}
		})
	})

	return clientIds
}


func GetActiveProvidersForLocationGroup(locationGroupId Id) []Id {
	clientIds := []Id{}

	bringyour.Db(ctx, func(conn bringyour.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT
					DISTINCT network_client_location.client_id
				FROM network_client_location

				INNER JOIN provide_config ON
					provide_config.client_id = network_client_location.client_id AND
					provide_config.provide_mode = $1

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
		bringyour.WithDbResult(result, err, func() {
			for result.Next {
				var clientId Id
				bringyour.Raise(bringyour.Scan(&clientId))
				clientIds = append(clientIds, clientId)
			}
		})
	})

	return clientIds
}

