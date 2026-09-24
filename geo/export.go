package geo

import (
	"bytes"
	"cmp"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// The export of the place list from GeoLite2-City network records
// (connect/GEOMAP.md §4.1): the network-weighted aggregation of each city and
// country, the disambiguation of colliding names, and the deterministic
// places.yml it marshals to.

// The part of one GeoLite2-City network record the export reads. The export
// tool decodes each record into one; tests build them directly.
type ExportNetwork struct {
	// 0 when the record has no city
	CityGeonameId uint32
	// English names, empty when the record has none
	City string
	// the first subdivision, the one a location is filed under
	RegionGeonameId  uint32
	Region           string
	CountryCode      string
	CountryGeonameId uint32
	Country          string
	ContinentCode    string
	Continent        string
	// false when the record carries no coordinates
	HasCoordinates bool
	Latitude       float64
	Longitude      float64
	// 0 when the record gives none
	AccuracyRadiusKm uint16
	TimeZone         string
}

// Counts what an export saw and wrote.
type ExportSummary struct {
	// networks added, and those of them with a city
	NetworkCount     int
	CityNetworkCount int
	// cities and countries written
	CityCount    int
	CountryCount int
	// cities whose networks carry more than one coordinate
	MultiCoordinateCityCount int
	// cities written with a " (geoname <id>)" suffix because another city of
	// the same region has the same name
	NameCollisionCount int
	// the same, for regions of one country
	RegionNameCollisionCount int
	// cities GeoLite2 files under no subdivision, written under a region named
	// for the country
	RegionlessCityCount int
	// cities left out for want of a name, a country or a coordinate
	SkippedCityCount int
	// the widest city
	MaxSpreadKm float64
}

// A network-weighted tally over a handful of values. Almost every tally here
// has one value (a city's name, its time zone), so a slice with a linear scan
// costs far less than a map per city across ~78k cities.
type counts[T comparable] []countedValue[T]

// One value of a tally and the networks that carry it.
type countedValue[T comparable] struct {
	value        T
	networkCount int
}

// Counts networkCount more networks for a value.
func (self *counts[T]) add(value T, networkCount int) {
	for i := range *self {
		if (*self)[i].value == value {
			(*self)[i].networkCount += networkCount
			return
		}
	}
	*self = append(*self, countedValue[T]{value: value, networkCount: networkCount})
}

// The value with the most networks, ties to the least by compare, so the
// answer does not depend on the order the records were added in.
func (self counts[T]) mode(compare func(a T, b T) int) (value T, ok bool) {
	best := -1
	for i, counted := range self {
		if best < 0 ||
			self[best].networkCount < counted.networkCount ||
			(counted.networkCount == self[best].networkCount && compare(counted.value, self[best].value) < 0) {
			best = i
		}
	}
	if best < 0 {
		return value, false
	}
	return self[best].value, true
}

// A city's first subdivision, by id and English name; the zero value is no
// subdivision.
type exportRegion struct {
	geonameId uint32
	name      string
}

// one distinct coordinate of a city
type exportVariant struct {
	latitude     float64
	longitude    float64
	networkCount int
	// the smallest accuracy radius of the networks at this coordinate, 0 when
	// none of them gave one
	accuracyRadiusKm uint16
}

// The tallies of one city across the records that name it.
type exportCity struct {
	names        counts[string]
	regions      counts[exportRegion]
	countryCodes counts[string]
	timeZones    counts[string]
	variants     []exportVariant
}

// The tallies of one country across the records that name it.
type exportCountry struct {
	names          counts[string]
	geonameIds     counts[uint32]
	continentCodes counts[string]
	continents     counts[string]
}

// Aggregates GeoLite2-City network records into the place list: every city
// keyed by geoname id, with the coordinate most of its networks carry, and
// every country the database names.
type Exporter struct {
	networkCount     int
	cityNetworkCount int
	cities           map[uint32]*exportCity
	countries        map[string]*exportCountry
}

// An exporter with nothing added.
func NewExporter() *Exporter {
	return &Exporter{
		cities:    map[uint32]*exportCity{},
		countries: map[string]*exportCountry{},
	}
}

// Counts `networkCount` networks that share one record. The export tool
// passes each distinct record once with the number of networks that point at
// it: GeoLite2 shares records between networks, so this is ~340k calls rather
// than ~5.8M.
func (self *Exporter) Add(network *ExportNetwork, networkCount int) {
	if networkCount <= 0 {
		return
	}
	self.networkCount += networkCount

	countryCode := strings.ToLower(network.CountryCode)
	if countryCode != "" {
		// every country the database names is exported, including those whose
		// networks never resolve below the country
		country, ok := self.countries[countryCode]
		if !ok {
			country = &exportCountry{}
			self.countries[countryCode] = country
		}
		if name := exportName(network.Country); name != "" {
			country.names.add(name, networkCount)
		}
		if network.CountryGeonameId != 0 {
			country.geonameIds.add(network.CountryGeonameId, networkCount)
		}
		if network.ContinentCode != "" {
			country.continentCodes.add(strings.ToLower(network.ContinentCode), networkCount)
		}
		if name := exportName(network.Continent); name != "" {
			country.continents.add(name, networkCount)
		}
	}

	if network.CityGeonameId == 0 {
		return
	}
	self.cityNetworkCount += networkCount
	city, ok := self.cities[network.CityGeonameId]
	if !ok {
		city = &exportCity{}
		self.cities[network.CityGeonameId] = city
	}
	if name := exportName(network.City); name != "" {
		city.names.add(name, networkCount)
	}
	if countryCode != "" {
		city.countryCodes.add(countryCode, networkCount)
	}
	// A subdivision without an English name cannot be filed by name, so it
	// counts as no subdivision. A lookup sees the same record the same way:
	// its region is empty and resolves to the region named for the country.
	region := exportRegion{}
	if name := exportName(network.Region); name != "" {
		region = exportRegion{
			geonameId: network.RegionGeonameId,
			name:      name,
		}
	}
	city.regions.add(region, networkCount)
	if network.TimeZone != "" {
		city.timeZones.add(network.TimeZone, networkCount)
	}
	if network.HasCoordinates && validCoordinate(network.Latitude, network.Longitude) {
		// Variants are distinct coordinates. Networks at the same coordinate
		// with different accuracy radii (a city centroid carries broadband and
		// mobile networks alike) weigh on one coordinate together; the
		// smallest radius is kept for the tie-break.
		func() {
			for i := range city.variants {
				variant := &city.variants[i]
				if variant.latitude == network.Latitude && variant.longitude == network.Longitude {
					variant.networkCount += networkCount
					if network.AccuracyRadiusKm != 0 && (variant.accuracyRadiusKm == 0 || network.AccuracyRadiusKm < variant.accuracyRadiusKm) {
						variant.accuracyRadiusKm = network.AccuracyRadiusKm
					}
					return
				}
			}
			city.variants = append(city.variants, exportVariant{
				latitude:         network.Latitude,
				longitude:        network.Longitude,
				networkCount:     networkCount,
				accuracyRadiusKm: network.AccuracyRadiusKm,
			})
		}()
	}
}

// Keeps a name as GeoLite2 spells it, so a seeded row and a lookup of the same
// place agree byte for byte. Only invalid UTF-8, which YAML cannot carry as a
// string, is dropped.
func exportName(name string) string {
	if utf8.ValidString(name) {
		return name
	}
	return strings.ToValidUTF8(name, "")
}

// One resolved country, ready to marshal.
type exportedCountry struct {
	code     string
	document countryDocument
}

// One resolved city under its final names, ready to marshal.
type exportedPlace struct {
	countryCode string
	region      string
	city        string
	document    placeDocument
}

// The place list an Exporter resolved, ready to marshal.
type Export struct {
	Summary ExportSummary

	source     string
	buildEpoch uint64
	// by code
	countries []exportedCountry
	// by country, region, city, geoname id
	places []exportedPlace
}

// The suffix that tells apart two places of the same name in the same parent.
// The seeder stores the suffixed name, and CreateLocation composes the same
// suffix when a lookup meets the collision first (network_client_location_model.go).
func GeonameSuffixedName(name string, geonameId uint32) string {
	return fmt.Sprintf("%s (geoname %d)", name, geonameId)
}

// Resolves every city and country. It fails only when a disambiguated name
// would itself collide, which would silently merge two places.
func (self *Exporter) Export(source string, buildEpoch uint64) (*Export, error) {
	export := &Export{
		source:     source,
		buildEpoch: buildEpoch,
	}
	summary := &export.Summary
	summary.NetworkCount = self.networkCount
	summary.CityNetworkCount = self.cityNetworkCount

	countryNames := map[string]string{}
	for code, country := range self.countries {
		name, _ := country.names.mode(cmp.Compare[string])
		geonameId, _ := country.geonameIds.mode(cmp.Compare[uint32])
		continentCode, _ := country.continentCodes.mode(cmp.Compare[string])
		continent, _ := country.continents.mode(cmp.Compare[string])
		export.countries = append(export.countries, exportedCountry{
			code: code,
			document: countryDocument{
				Name:          name,
				GeonameId:     geonameId,
				ContinentCode: continentCode,
				Continent:     continent,
			},
		})
		countryNames[code] = name
	}
	slices.SortFunc(export.countries, func(a exportedCountry, b exportedCountry) int {
		return cmp.Compare(a.code, b.code)
	})
	summary.CountryCount = len(export.countries)

	// orders regions by geoname id, then name
	compareRegions := func(a exportRegion, b exportRegion) int {
		if c := cmp.Compare(a.geonameId, b.geonameId); c != 0 {
			return c
		}
		return cmp.Compare(a.name, b.name)
	}
	// The mode of a city's coordinates weighted by network count, ties to the
	// smallest accuracy radius (an unknown radius counts as the largest), then
	// to the smallest latitude, then longitude.
	representativeVariant := func(city *exportCity) exportVariant {
		sortRadius := func(variant exportVariant) int {
			if variant.accuracyRadiusKm == 0 {
				return math.MaxUint16 + 1
			}
			return int(variant.accuracyRadiusKm)
		}
		return slices.MinFunc(city.variants, func(a exportVariant, b exportVariant) int {
			if c := cmp.Compare(b.networkCount, a.networkCount); c != 0 {
				return c
			}
			if c := cmp.Compare(sortRadius(a), sortRadius(b)); c != 0 {
				return c
			}
			if c := cmp.Compare(a.latitude, b.latitude); c != 0 {
				return c
			}
			return cmp.Compare(a.longitude, b.longitude)
		})
	}

	// a city with its tallies resolved, before name collisions are settled
	type resolvedCity struct {
		geonameId   uint32
		name        string
		countryCode string
		region      exportRegion
		regionName  string
		document    placeDocument
	}
	resolvedCities := make([]*resolvedCity, 0, len(self.cities))
	for geonameId, city := range self.cities {
		name, ok := city.names.mode(cmp.Compare[string])
		if !ok {
			summary.SkippedCityCount += 1
			continue
		}
		countryCode, ok := city.countryCodes.mode(cmp.Compare[string])
		if !ok {
			summary.SkippedCityCount += 1
			continue
		}
		if len(city.variants) == 0 {
			summary.SkippedCityCount += 1
			continue
		}
		region, _ := city.regions.mode(compareRegions)
		regionName := region.name
		if regionName == "" {
			// The convention the blank-region backfill established (server
			// db_migrations.go): a location GeoLite2 files under no
			// subdivision is "the whole of this country", so its region is
			// named for the country. The row is not the country, so it carries
			// no geoname id of its own.
			regionName = countryNames[countryCode]
			region = exportRegion{}
			if regionName == "" {
				summary.SkippedCityCount += 1
				continue
			}
			summary.RegionlessCityCount += 1
		}

		representative := representativeVariant(city)
		spreadKm := 0.0
		for _, variant := range city.variants {
			spreadKm = max(spreadKm, DistanceKm(
				representative.latitude,
				representative.longitude,
				variant.latitude,
				variant.longitude,
			))
		}
		if 1 < len(city.variants) {
			summary.MultiCoordinateCityCount += 1
		}
		summary.MaxSpreadKm = max(summary.MaxSpreadKm, spreadKm)
		timeZone, _ := city.timeZones.mode(cmp.Compare[string])

		resolvedCities = append(resolvedCities, &resolvedCity{
			geonameId:   geonameId,
			name:        name,
			countryCode: countryCode,
			region:      region,
			regionName:  regionName,
			document: placeDocument{
				GeonameId:       geonameId,
				RegionGeonameId: region.geonameId,
				Latitude:        representative.latitude,
				Longitude:       representative.longitude,
				SpreadKm:        spreadKm,
				TimeZone:        timeZone,
			},
		})
	}

	// Two subdivisions of one country with the same English name would share
	// one region key. The smallest geoname id keeps the name and every other
	// takes the suffix, the rule CreateLocation applies to the rows. The
	// region named for the country (id 0) shares the name of a real
	// subdivision of that name, as its rows do.
	type regionNameKey struct {
		countryCode string
		name        string
	}
	regionNameGeonameIds := map[regionNameKey][]uint32{}
	for _, city := range resolvedCities {
		if city.region.geonameId == 0 {
			continue
		}
		key := regionNameKey{countryCode: city.countryCode, name: city.regionName}
		if !slices.Contains(regionNameGeonameIds[key], city.region.geonameId) {
			regionNameGeonameIds[key] = append(regionNameGeonameIds[key], city.region.geonameId)
		}
	}
	regionNames := map[regionNameKey]bool{}
	for _, city := range resolvedCities {
		regionNames[regionNameKey{countryCode: city.countryCode, name: city.regionName}] = true
	}
	regionSuffixedNames := map[exportRegion]string{}
	for key, geonameIds := range regionNameGeonameIds {
		if len(geonameIds) < 2 {
			continue
		}
		slices.Sort(geonameIds)
		for _, geonameId := range geonameIds[1:] {
			suffixedName := GeonameSuffixedName(key.name, geonameId)
			if regionNames[regionNameKey{countryCode: key.countryCode, name: suffixedName}] {
				return nil, fmt.Errorf("region %q of %q collides with its own disambiguated name", suffixedName, key.countryCode)
			}
			regionSuffixedNames[exportRegion{geonameId: geonameId, name: key.name}] = suffixedName
			summary.RegionNameCollisionCount += 1
		}
	}
	for _, city := range resolvedCities {
		if suffixedName, ok := regionSuffixedNames[city.region]; ok {
			city.regionName = suffixedName
		}
	}

	// The same for cities of one region: the smallest geoname id keeps the
	// name, the rest take the suffix.
	type cityNameKey struct {
		countryCode string
		region      string
		name        string
	}
	cityNameGeonameIds := map[cityNameKey][]uint32{}
	for _, city := range resolvedCities {
		key := cityNameKey{countryCode: city.countryCode, region: city.regionName, name: city.name}
		cityNameGeonameIds[key] = append(cityNameGeonameIds[key], city.geonameId)
	}
	geonameIdSuffixedNames := map[uint32]string{}
	for key, geonameIds := range cityNameGeonameIds {
		if len(geonameIds) < 2 {
			continue
		}
		slices.Sort(geonameIds)
		for _, geonameId := range geonameIds[1:] {
			suffixedName := GeonameSuffixedName(key.name, geonameId)
			if _, ok := cityNameGeonameIds[cityNameKey{countryCode: key.countryCode, region: key.region, name: suffixedName}]; ok {
				return nil, fmt.Errorf("city %q of %q, %q collides with its own disambiguated name", suffixedName, key.region, key.countryCode)
			}
			geonameIdSuffixedNames[geonameId] = suffixedName
			summary.NameCollisionCount += 1
		}
	}

	export.places = make([]exportedPlace, 0, len(resolvedCities))
	for _, city := range resolvedCities {
		name := city.name
		if suffixedName, ok := geonameIdSuffixedNames[city.geonameId]; ok {
			name = suffixedName
		}
		export.places = append(export.places, exportedPlace{
			countryCode: city.countryCode,
			region:      city.regionName,
			city:        name,
			document:    city.document,
		})
	}
	slices.SortFunc(export.places, func(a exportedPlace, b exportedPlace) int {
		if c := cmp.Compare(a.countryCode, b.countryCode); c != 0 {
			return c
		}
		if c := cmp.Compare(a.region, b.region); c != 0 {
			return c
		}
		if c := cmp.Compare(a.city, b.city); c != 0 {
			return c
		}
		return cmp.Compare(a.document.GeonameId, b.document.GeonameId)
	})
	summary.CityCount = len(export.places)

	return export, nil
}

// The head comment of places.yml: where it comes from, and the attribution
// the GeoLite2 license asks for.
const exportHeader = `The canonical place list (connect/GEOMAP.md §4), generated by
server/cli/geolite2export from the GeoLite2-City database beside it. Do not
edit: rerun the export.

This product includes GeoLite2 data created by MaxMind, available from
https://www.maxmind.com. GeoLite2 incorporates GeoNames data (CC BY 4.0).`

// Writes places.yml. The output depends only on the export's content, never on
// map order, so re-running on the same database diffs clean and a new
// database diffs by what changed.
func (self *Export) Marshal() ([]byte, error) {
	root := &yaml.Node{Kind: yaml.MappingNode}
	root.Content = append(
		root.Content,
		yamlString("version"), yamlNumber(strconv.Itoa(PlacesVersion)),
		yamlString("source"), yamlString(self.source),
		yamlString("build_epoch"), yamlNumber(strconv.FormatUint(self.buildEpoch, 10)),
	)

	countriesNode := &yaml.Node{Kind: yaml.MappingNode}
	for _, country := range self.countries {
		countriesNode.Content = append(
			countriesNode.Content,
			yamlString(country.code),
			yamlFlowMapping(
				yamlString("name"), yamlString(country.document.Name),
				yamlString("geoname_id"), yamlNumber(strconv.FormatUint(uint64(country.document.GeonameId), 10)),
				yamlString("continent_code"), yamlString(country.document.ContinentCode),
				yamlString("continent"), yamlString(country.document.Continent),
			),
		)
	}
	root.Content = append(root.Content, yamlString("countries"), countriesNode)

	placesNode := &yaml.Node{Kind: yaml.MappingNode}
	var countryPlacesNode *yaml.Node
	var regionPlacesNode *yaml.Node
	var countryCode string
	var region string
	for i, place := range self.places {
		if i == 0 || place.countryCode != countryCode {
			countryCode = place.countryCode
			countryPlacesNode = &yaml.Node{Kind: yaml.MappingNode}
			placesNode.Content = append(placesNode.Content, yamlString(countryCode), countryPlacesNode)
			regionPlacesNode = nil
		}
		if regionPlacesNode == nil || place.region != region {
			region = place.region
			regionPlacesNode = &yaml.Node{Kind: yaml.MappingNode}
			countryPlacesNode.Content = append(countryPlacesNode.Content, yamlString(region), regionPlacesNode)
		}
		fieldNodes := []*yaml.Node{
			yamlString("geoname_id"), yamlNumber(strconv.FormatUint(uint64(place.document.GeonameId), 10)),
		}
		// a city under no subdivision has no region id to write
		if place.document.RegionGeonameId != 0 {
			fieldNodes = append(
				fieldNodes,
				yamlString("region_geoname_id"), yamlNumber(strconv.FormatUint(uint64(place.document.RegionGeonameId), 10)),
			)
		}
		fieldNodes = append(
			fieldNodes,
			yamlString("latitude"), yamlNumber(formatCoordinate(place.document.Latitude)),
			yamlString("longitude"), yamlNumber(formatCoordinate(place.document.Longitude)),
			// to 100 m: finer is noise from the variants' own 4-decimal precision
			yamlString("spread_km"), yamlNumber(strconv.FormatFloat(math.Round(place.document.SpreadKm*10)/10, 'f', -1, 64)),
		)
		if place.document.TimeZone != "" {
			fieldNodes = append(fieldNodes, yamlString("time_zone"), yamlString(place.document.TimeZone))
		}
		regionPlacesNode.Content = append(regionPlacesNode.Content, yamlString(place.city), yamlFlowMapping(fieldNodes...))
	}
	root.Content = append(root.Content, yamlString("places"), placesNode)

	document := &yaml.Node{
		Kind:        yaml.DocumentNode,
		HeadComment: exportHeader,
		Content:     []*yaml.Node{root},
	}
	var out bytes.Buffer
	encoder := yaml.NewEncoder(&out)
	encoder.SetIndent(2)
	if err := encoder.Encode(document); err != nil {
		return nil, err
	}
	if err := encoder.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// The shortest decimal that reads back as the same float64, which for the
// database's coordinates is the 4 decimals it stores.
func formatCoordinate(value float64) string {
	return strconv.FormatFloat(value, 'f', -1, 64)
}

// yaml.v3 quotes a string that would read back as another type (true, 1.5,
// null, ...), but not the YAML 1.1 booleans (yes, no, on, off) or base 60
// numbers (1:20), which yaml.v3 reads back as strings and a YAML 1.1 reader
// does not. Those are quoted here so the file reads the same everywhere.
var yaml11Base60 = regexp.MustCompile(`^[-+]?[0-9][0-9_]*(?::[0-5]?[0-9])+(?:\.[0-9_]*)?$`)

// A string scalar, quoted when a YAML 1.1 reader would read it otherwise.
func yamlString(value string) *yaml.Node {
	node := &yaml.Node{
		Kind:  yaml.ScalarNode,
		Tag:   "!!str",
		Value: value,
	}
	// whether a YAML 1.1 reader would read the plain string as another type
	yaml11Ambiguous := func() bool {
		switch value {
		case "y", "Y", "yes", "Yes", "YES", "on", "On", "ON",
			"n", "N", "no", "No", "NO", "off", "Off", "OFF":
			return true
		}
		return yaml11Base60.MatchString(value)
	}
	if yaml11Ambiguous() {
		node.Style = yaml.DoubleQuotedStyle
	}
	return node
}

// a number written plain, so it reads back by its own form (int or float)
func yamlNumber(value string) *yaml.Node {
	return &yaml.Node{
		Kind:  yaml.ScalarNode,
		Value: value,
	}
}

// A mapping written on one line, as each place and country is.
func yamlFlowMapping(content ...*yaml.Node) *yaml.Node {
	return &yaml.Node{
		Kind:    yaml.MappingNode,
		Style:   yaml.FlowStyle,
		Content: content,
	}
}
