// Package geo is the canonical place list (connect/GEOMAP.md §4): the cities,
// regions and countries exported from the same GeoLite2-City database that
// answers every ip lookup, and a reverse geocoder over them.
//
// The package depends on nothing in the server, so the root `server` package
// can import it without a cycle.
package geo

import (
	"fmt"
	"iter"
	"math"
	"slices"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// The `version` of the places.yml this package reads and writes. A loader
// refuses any other version rather than guess at its shape.
const PlacesVersion = 1

// The document places.yml holds. The export writes it (export.go) and
// LoadPlaces reads it; the two share these types so the field names cannot
// drift apart.
type placesDocument struct {
	Version    int    `yaml:"version"`
	Source     string `yaml:"source"`
	BuildEpoch uint64 `yaml:"build_epoch"`
	// country code -> country
	Countries map[string]*countryDocument `yaml:"countries"`
	// country code -> region -> city -> place
	Places map[string]map[string]map[string]*placeDocument `yaml:"places"`
}

// One country of places.yml, keyed by its code.
type countryDocument struct {
	Name          string `yaml:"name"`
	GeonameId     uint32 `yaml:"geoname_id"`
	ContinentCode string `yaml:"continent_code"`
	Continent     string `yaml:"continent"`
}

// One city of places.yml, keyed by its country, region and name.
type placeDocument struct {
	GeonameId       uint32  `yaml:"geoname_id"`
	RegionGeonameId uint32  `yaml:"region_geoname_id"`
	Latitude        float64 `yaml:"latitude"`
	Longitude       float64 `yaml:"longitude"`
	SpreadKm        float64 `yaml:"spread_km"`
	TimeZone        string  `yaml:"time_zone"`
}

// One country of the place list.
type Country struct {
	// ISO 3166-1 alpha-2, lower case
	Code string
	Name string
	// GeoNames id, 0 when GeoLite2 gives none
	GeonameId uint32
	// lower case, e.g. `eu`
	ContinentCode string
	Continent     string
}

// One city of the place list.
type Place struct {
	// the city's English name. When another city of the same region has the
	// same name, the later one (by geoname id) carries a " (geoname <id>)"
	// suffix, which is the name the seeder stores
	City string
	// the first subdivision's English name, or the country's name for a city
	// GeoLite2 files under no subdivision
	Region string
	// ISO 3166-1 alpha-2, lower case
	CountryCode string
	TimeZone    string
	// the representative coordinate: the one most of the city's networks carry
	Latitude  float64
	Longitude float64
	// the largest great-circle distance from the representative coordinate to
	// any other coordinate the city's networks carry; 0 for a city with one
	SpreadKm  float64
	GeonameId uint32
	// 0 for a city filed under no subdivision
	RegionGeonameId uint32
}

// A region by its lower case country code and its name in the list.
type regionKey struct {
	countryCode string
	region      string
}

// A loaded place list. It is immutable after LoadPlaces and safe for
// concurrent use; the *Place and *Country values it returns are shared and must
// not be modified.
type Places struct {
	Version    int
	Source     string
	BuildEpoch uint64

	countries    map[string]*Country
	countryCodes []string

	// every city in the export's order: country, region, city, geoname id
	places []Place
	// indexes into `places`, ordered by geoname id
	placeIndexesByGeonameId []int32
	countryPlaceCounts      map[string]int
	regionGeonameIds        map[regionKey]uint32

	// The 1° grid (gridRows x gridColumns cells, row-major from the south pole
	// and the antimeridian), stored compressed: the places of cell c are
	// cellPlaceIndexes[cellStarts[c]:cellStarts[c+1]]. Two flat arrays cost
	// ~0.6 MB for the whole list, where a slice per cell would cost a header
	// for each of the 64,800 cells and an allocation for each occupied one.
	cellStarts       []int32
	cellPlaceIndexes []int32
}

const (
	gridRows    = 180
	gridColumns = 360
)

// Parses a places.yml written by the export, validates it, and indexes its
// cities in the export's order, by geoname id, and by 1° cell.
func LoadPlaces(placesBytes []byte) (*Places, error) {
	document := &placesDocument{}
	if err := yaml.Unmarshal(placesBytes, document); err != nil {
		return nil, fmt.Errorf("places: %w", err)
	}
	if document.Version != PlacesVersion {
		return nil, fmt.Errorf("places: version %d is not the supported version %d", document.Version, PlacesVersion)
	}

	self := &Places{
		Version:            document.Version,
		Source:             document.Source,
		BuildEpoch:         document.BuildEpoch,
		countries:          map[string]*Country{},
		countryPlaceCounts: map[string]int{},
		regionGeonameIds:   map[regionKey]uint32{},
	}

	for code, countryDocument := range document.Countries {
		if countryDocument == nil {
			return nil, fmt.Errorf("places: country %q has no fields", code)
		}
		code = strings.ToLower(code)
		if _, ok := self.countries[code]; ok {
			return nil, fmt.Errorf("places: country %q is listed twice", code)
		}
		self.countries[code] = &Country{
			Code:          code,
			Name:          countryDocument.Name,
			GeonameId:     countryDocument.GeonameId,
			ContinentCode: strings.ToLower(countryDocument.ContinentCode),
			Continent:     countryDocument.Continent,
		}
		self.countryCodes = append(self.countryCodes, code)
	}
	slices.Sort(self.countryCodes)

	placeCount := 0
	for _, regions := range document.Places {
		for _, cities := range regions {
			placeCount += len(cities)
		}
	}
	self.places = make([]Place, 0, placeCount)

	// every city of a country shares one time zone string per zone rather
	// than one decoded copy per city
	timeZones := map[string]string{}
	intern := func(timeZone string) string {
		if interned, ok := timeZones[timeZone]; ok {
			return interned
		}
		timeZones[timeZone] = timeZone
		return timeZone
	}

	for _, countryCode := range sortedKeys(document.Places) {
		regions := document.Places[countryCode]
		lowerCountryCode := strings.ToLower(countryCode)
		if _, ok := self.countries[lowerCountryCode]; !ok {
			return nil, fmt.Errorf("places: country %q has places but is not in the country list", countryCode)
		}
		for _, region := range sortedKeys(regions) {
			if region == "" {
				return nil, fmt.Errorf("places: country %q has a region with no name", countryCode)
			}
			cities := regions[region]
			for _, city := range sortedKeys(cities) {
				placeDocument := cities[city]
				if city == "" {
					return nil, fmt.Errorf("places: %q, %q has a city with no name", countryCode, region)
				}
				if placeDocument == nil {
					return nil, fmt.Errorf("places: %q, %q, %q has no fields", countryCode, region, city)
				}
				if placeDocument.GeonameId == 0 {
					return nil, fmt.Errorf("places: %q, %q, %q has no geoname id", countryCode, region, city)
				}
				if !validCoordinate(placeDocument.Latitude, placeDocument.Longitude) {
					return nil, fmt.Errorf(
						"places: %q, %q, %q has an invalid coordinate (%v, %v)",
						countryCode,
						region,
						city,
						placeDocument.Latitude,
						placeDocument.Longitude,
					)
				}
				self.places = append(self.places, Place{
					City:            city,
					Region:          region,
					CountryCode:     lowerCountryCode,
					TimeZone:        intern(placeDocument.TimeZone),
					Latitude:        placeDocument.Latitude,
					Longitude:       placeDocument.Longitude,
					SpreadKm:        placeDocument.SpreadKm,
					GeonameId:       placeDocument.GeonameId,
					RegionGeonameId: placeDocument.RegionGeonameId,
				})
				self.countryPlaceCounts[lowerCountryCode] += 1
				if placeDocument.RegionGeonameId != 0 {
					key := regionKey{countryCode: lowerCountryCode, region: region}
					if _, ok := self.regionGeonameIds[key]; !ok {
						self.regionGeonameIds[key] = placeDocument.RegionGeonameId
					}
				}
			}
		}
	}

	self.placeIndexesByGeonameId = make([]int32, len(self.places))
	for i := range self.places {
		self.placeIndexesByGeonameId[i] = int32(i)
	}
	sort.Slice(self.placeIndexesByGeonameId, func(i int, j int) bool {
		return self.places[self.placeIndexesByGeonameId[i]].GeonameId < self.places[self.placeIndexesByGeonameId[j]].GeonameId
	})
	for i := 1; i < len(self.placeIndexesByGeonameId); i += 1 {
		a := &self.places[self.placeIndexesByGeonameId[i-1]]
		b := &self.places[self.placeIndexesByGeonameId[i]]
		if a.GeonameId == b.GeonameId {
			return nil, fmt.Errorf(
				"places: geoname id %d names both %q, %q, %q and %q, %q, %q",
				a.GeonameId,
				a.CountryCode,
				a.Region,
				a.City,
				b.CountryCode,
				b.Region,
				b.City,
			)
		}
	}

	// bucket the places by 1° cell with a counting sort into the two flat
	// cell arrays
	self.cellStarts = make([]int32, gridRows*gridColumns+1)
	cells := make([]int32, len(self.places))
	for i := range self.places {
		column := int(math.Floor(normalizeLongitude(self.places[i].Longitude) + 180))
		// normalizeLongitude(x) + 180 can round up to exactly 360
		column = min(gridColumns-1, max(0, column))
		cell := cellIndex(cellRow(self.places[i].Latitude), column)
		cells[i] = int32(cell)
		self.cellStarts[cell+1] += 1
	}
	for cell := 1; cell < len(self.cellStarts); cell += 1 {
		self.cellStarts[cell] += self.cellStarts[cell-1]
	}
	self.cellPlaceIndexes = make([]int32, len(self.places))
	next := slices.Clone(self.cellStarts[:gridRows*gridColumns])
	for i, cell := range cells {
		self.cellPlaceIndexes[next[cell]] = int32(i)
		next[cell] += 1
	}
	return self, nil
}

// The country for an ISO 3166-1 alpha-2 code in either case, or nil.
func (self *Places) Country(code string) *Country {
	return self.countries[strings.ToLower(code)]
}

// Every country, ordered by code.
func (self *Places) Countries() []*Country {
	countries := make([]*Country, 0, len(self.countryCodes))
	for _, code := range self.countryCodes {
		countries = append(countries, self.countries[code])
	}
	return countries
}

// The number of cities in the list.
func (self *Places) CityCount() int {
	return len(self.places)
}

// Iterates every city in the export's order: country, region, city, geoname
// id.
func (self *Places) Cities() iter.Seq[*Place] {
	return func(yield func(*Place) bool) {
		for i := range self.places {
			if !yield(&self.places[i]) {
				return
			}
		}
	}
}

// The city with a GeoNames id, or nil.
func (self *Places) CityByGeonameId(geonameId uint32) *Place {
	i := sort.Search(len(self.placeIndexesByGeonameId), func(i int) bool {
		return geonameId <= self.places[self.placeIndexesByGeonameId[i]].GeonameId
	})
	if i < len(self.placeIndexesByGeonameId) {
		place := &self.places[self.placeIndexesByGeonameId[i]]
		if place.GeonameId == geonameId {
			return place
		}
	}
	return nil
}

// The GeoNames id of a region by its name in the list, or 0 when the list has
// no such region or files its cities under no subdivision.
func (self *Places) RegionGeonameId(countryCode string, region string) uint32 {
	return self.regionGeonameIds[regionKey{countryCode: strings.ToLower(countryCode), region: region}]
}

// The first search covers a cap of this radius. Cities in a populated area
// are a few km to tens of km apart, so most searches finish in the one to four
// cells around the point; a sparse area widens from here.
const initialSearchRadiusKm = 64.0

// any two points on the sphere are at most half a great circle apart
const maxSearchRadiusKm = math.Pi * EarthRadiusKm

// The city nearest a point by great-circle distance, and that distance in km.
// A non-empty countryCode restricts the search to that country's cities; an
// empty one searches every country. The result is nil (and 0) when there is
// no such city, or for a non-finite coordinate.
//
// Latitude is clamped to [-90, 90] and longitude taken modulo 360. Two cities
// at exactly the same distance resolve to the smaller geoname id, so the
// answer never depends on the scan order.
func (self *Places) NearestCity(latitude float64, longitude float64, countryCode string) (*Place, float64) {
	if math.IsNaN(latitude) || math.IsInf(latitude, 0) || math.IsNaN(longitude) || math.IsInf(longitude, 0) {
		return nil, 0
	}
	latitude = min(90, max(-90, latitude))
	longitude = normalizeLongitude(longitude)
	countryCode = strings.ToLower(countryCode)
	if countryCode == "" {
		if len(self.places) == 0 {
			return nil, 0
		}
	} else if self.countryPlaceCounts[countryCode] == 0 {
		return nil, 0
	}

	// Scans every cell that intersects the bounding box of the cap of
	// radiusKm around the point, and returns the nearest place in those cells
	// (which may lie outside the cap).
	nearestWithin := func(radiusKm float64) (*Place, float64) {
		// widen the box a hair so a place exactly on the cap's edge cannot be
		// lost to rounding in the box arithmetic
		const marginDegrees = 1e-6

		angularRadius := radiusKm / EarthRadiusKm
		latitudeDelta := angularRadius*180/math.Pi + marginDegrees
		minLatitude := latitude - latitudeDelta
		maxLatitude := latitude + latitudeDelta

		allColumns := false
		var longitudeDelta float64
		if 90 <= maxLatitude || minLatitude <= -90 || math.Pi/2 <= angularRadius {
			// the cap reaches a pole, so it spans every meridian
			allColumns = true
		} else {
			// The cap's widest meridians touch it where they are tangent to its
			// edge. The pole, the centre and a tangent point make a spherical
			// triangle with a right angle at the tangent point, hypotenuse the
			// centre's colatitude (90° - latitude), the side opposite the pole the
			// angular radius, and the angle at the pole the half width in
			// longitude. Napier's rule for that triangle gives
			// sin(radius) = cos(latitude) * sin(half width).
			sinHalfWidth := math.Sin(angularRadius) / math.Cos(latitude*math.Pi/180)
			if 1 <= sinHalfWidth {
				allColumns = true
			} else {
				longitudeDelta = math.Asin(sinHalfWidth)*180/math.Pi + marginDegrees
				if 180 <= longitudeDelta {
					allColumns = true
				}
			}
		}

		minRow := cellRow(max(-90, minLatitude))
		maxRow := cellRow(min(90, maxLatitude))
		var firstColumn int
		var columnCount int
		if allColumns {
			firstColumn = 0
			columnCount = gridColumns
		} else {
			// may run past either edge of [0, gridColumns); the scan wraps, which
			// is what carries a search across the antimeridian
			firstColumn = int(math.Floor(longitude - longitudeDelta + 180))
			lastColumn := int(math.Floor(longitude + longitudeDelta + 180))
			columnCount = min(gridColumns, lastColumn-firstColumn+1)
		}

		var best *Place
		bestDistanceKm := math.Inf(1)
		for row := minRow; row <= maxRow; row += 1 {
			for i := 0; i < columnCount; i += 1 {
				column := ((firstColumn+i)%gridColumns + gridColumns) % gridColumns
				cell := cellIndex(row, column)
				for _, placeIndex := range self.cellPlaceIndexes[self.cellStarts[cell]:self.cellStarts[cell+1]] {
					place := &self.places[placeIndex]
					if countryCode != "" && place.CountryCode != countryCode {
						continue
					}
					distanceKm := DistanceKm(latitude, longitude, place.Latitude, place.Longitude)
					if best == nil || distanceKm < bestDistanceKm || (distanceKm == bestDistanceKm && place.GeonameId < best.GeonameId) {
						best = place
						bestDistanceKm = distanceKm
					}
				}
			}
		}
		if best == nil {
			return nil, 0
		}
		return best, bestDistanceKm
	}

	// Search the cells that can hold a point within radiusKm and widen until
	// the best hit is inside the radius. A hit only counts once it is: the
	// cells beyond the radius are unvisited, and near a pole a neighbouring
	// cell a whole degree of longitude away can be only metres away. A hit
	// outside the radius instead bounds the answer, so one more search at its
	// distance is complete.
	radiusKm := initialSearchRadiusKm
	for {
		place, distanceKm := nearestWithin(radiusKm)
		if place != nil && distanceKm <= radiusKm {
			return place, distanceKm
		}
		if place != nil {
			radiusKm = distanceKm
			continue
		}
		if maxSearchRadiusKm <= radiusKm {
			// the last search covered every cell
			return nil, 0
		}
		radiusKm = min(2*radiusKm, maxSearchRadiusKm)
	}
}

// The mean earth radius the distances use; the server's other distance
// helpers use the same value.
const EarthRadiusKm = 6371.0

// The great-circle distance between two points in degrees.
//
// This is the haversine form: with hav(x) = sin²(x/2), the central angle c
// between the points satisfies hav(c) = hav(Δlatitude) +
// cos(latitude1)·cos(latitude2)·hav(Δlongitude), so c = 2·asin(√hav(c)). It
// stays accurate for small separations, where the spherical law of cosines
// loses precision to cos(c) ≈ 1.
func DistanceKm(latitude1 float64, longitude1 float64, latitude2 float64, longitude2 float64) float64 {
	const radiansPerDegree = math.Pi / 180
	sinHalfLatitudeDelta := math.Sin((latitude2 - latitude1) * radiansPerDegree / 2)
	sinHalfLongitudeDelta := math.Sin((longitude2 - longitude1) * radiansPerDegree / 2)
	h := sinHalfLatitudeDelta*sinHalfLatitudeDelta +
		math.Cos(latitude1*radiansPerDegree)*math.Cos(latitude2*radiansPerDegree)*sinHalfLongitudeDelta*sinHalfLongitudeDelta
	// rounding can carry h just outside [0, 1] for antipodal points
	h = min(1, max(0, h))
	return 2 * EarthRadiusKm * math.Asin(math.Sqrt(h))
}

// Whether a coordinate is in degrees on the sphere, as places.yml must hold it.
func validCoordinate(latitude float64, longitude float64) bool {
	return -90 <= latitude && latitude <= 90 && -180 <= longitude && longitude <= 180
}

// Maps any finite longitude into [-180, 180). One already in range is returned
// as is: (x + 180) - 180 need not round back to x.
func normalizeLongitude(longitude float64) float64 {
	if -180 <= longitude && longitude < 180 {
		return longitude
	}
	longitude = math.Mod(longitude+180, 360)
	if longitude < 0 {
		longitude += 360
	}
	return longitude - 180
}

// The grid row of a latitude, from the south pole.
func cellRow(latitude float64) int {
	return min(gridRows-1, max(0, int(math.Floor(latitude+90))))
}

// The row-major index of a grid cell.
func cellIndex(row int, column int) int {
	return row*gridColumns + column
}

// The keys of a map in order, so a walk of it does not depend on map order.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}
