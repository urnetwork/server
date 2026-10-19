package geo

import (
	"cmp"
	"math"
	"slices"
	"strconv"
	"strings"
)

// The containment hinges of the location solver over the place list
// (connect/GEOMAP.md §5.2): the keys a genesis region and country go by, and
// the latitude-ordered city sets each hinge is searched in.

// The key a genesis region goes by in the containment terms of the location
// solver (connect/GEOMAP.md §5.2): the country code and the region's GeoNames
// id, or the region's name when it has no id (a city GeoLite2 files under no
// subdivision, whose region is named for its country). The two forms use
// different separators, so a name can never be read as an id.
func ContainmentRegionKey(countryCode string, regionGeonameId uint32, region string) string {
	countryCode = strings.ToLower(countryCode)
	if regionGeonameId != 0 {
		return countryCode + "/" + strconv.FormatUint(uint64(regionGeonameId), 10)
	}
	return countryCode + ":" + region
}

// The key a genesis country goes by in the containment terms: its ISO code, in
// lower case.
func ContainmentCountryKey(countryCode string) string {
	return strings.ToLower(countryCode)
}

// The containment of the location solver over a place list: the same city set
// the reverse geocoder maps a derived point with, so the hinge prices exactly
// the crossings the mapping will report (§6).
//
// A hinge is max(0, d_in − d_out): the distance from the point to the nearest
// city inside the genesis region (country) less the distance to the nearest
// city outside it. The nearest city overall is the nearer of those two, so
// the hinge is also d_in − d_nearest, which is how it is computed: one search
// restricted to the region (country) and one unrestricted, and no search
// "outside" at all. Both are continuous in the point, so the hinge is
// continuous, zero while the nearest city is inside, and growing with the
// margin by which an outside city has become nearer once one has.
//
// The solver asks for hinges millions of times a run, near cities, so the
// searches are not NearestCity's: that one scans every city of the 1° cells
// under a 64 km cap, hundreds a cell in a dense country, at tens of µs a
// call. Here every city set -- all cities, each country's, each region's -- is
// kept ordered by latitude as unit vectors and scanned outward from the
// point's latitude (nearestInLatitudeOrder), comparing squared chords, which
// near a city touches the few dozen cities of a band a few km wide.
//
// Immutable after NewPlaceContainment and safe for concurrent use.
type PlaceContainment struct {
	// each ordered by latitude
	cities        []containmentCity
	countryCities map[string][]containmentCity
	regionCities  map[string][]containmentCity
}

// a city, or a query point, as its latitude in radians and its unit vector
type containmentCity struct {
	latitude float64
	x        float64
	y        float64
	z        float64
}

// A coordinate in degrees as a containment city.
func newContainmentCity(latitude float64, longitude float64) containmentCity {
	const radiansPerDegree = math.Pi / 180
	sinLatitude, cosLatitude := math.Sincos(latitude * radiansPerDegree)
	sinLongitude, cosLongitude := math.Sincos(longitude * radiansPerDegree)
	return containmentCity{
		latitude: latitude * radiansPerDegree,
		x:        cosLatitude * cosLongitude,
		y:        cosLatitude * sinLongitude,
		z:        sinLatitude,
	}
}

// the squared chord between two unit vectors, which orders cities by distance
// exactly as the great circle does
func (self containmentCity) squaredChord(other containmentCity) float64 {
	dx := self.x - other.x
	dy := self.y - other.y
	dz := self.z - other.z
	return dx*dx + dy*dy + dz*dz
}

// The great-circle distance, as atan2(|a×b|, a·b): full precision at every
// separation.
func (self containmentCity) surfaceKm(other containmentCity) float64 {
	crossX := self.y*other.z - self.z*other.y
	crossY := self.z*other.x - self.x*other.z
	crossZ := self.x*other.y - self.y*other.x
	dot := self.x*other.x + self.y*other.y + self.z*other.z
	return EarthRadiusKm * math.Atan2(math.Sqrt(crossX*crossX+crossY*crossY+crossZ*crossZ), dot)
}

// Indexes a place list's cities by country and region. A region with a
// GeoNames id also answers to its name form of ContainmentRegionKey, so a
// genesis row that lost its region's id still finds the region; names are
// unique within a country in the list, so the two forms cannot collide.
func NewPlaceContainment(places *Places) *PlaceContainment {
	self := &PlaceContainment{
		cities:        make([]containmentCity, 0, places.CityCount()),
		countryCities: map[string][]containmentCity{},
		regionCities:  map[string][]containmentCity{},
	}
	// name form -> id form, for the regions that have an id
	aliasRegionKeys := map[string]string{}
	for place := range places.Cities() {
		city := newContainmentCity(place.Latitude, place.Longitude)
		self.cities = append(self.cities, city)
		countryKey := ContainmentCountryKey(place.CountryCode)
		self.countryCities[countryKey] = append(self.countryCities[countryKey], city)
		// the region's id as the list knows it, so every city of a region
		// shares one key even if some were filed without the id
		regionGeonameId := places.RegionGeonameId(place.CountryCode, place.Region)
		regionKey := ContainmentRegionKey(place.CountryCode, regionGeonameId, place.Region)
		self.regionCities[regionKey] = append(self.regionCities[regionKey], city)
		if regionGeonameId != 0 {
			aliasRegionKeys[ContainmentRegionKey(place.CountryCode, 0, place.Region)] = regionKey
		}
	}
	byLatitude := func(a containmentCity, b containmentCity) int {
		return cmp.Compare(a.latitude, b.latitude)
	}
	slices.SortFunc(self.cities, byLatitude)
	for _, cities := range self.countryCities {
		slices.SortFunc(cities, byLatitude)
	}
	for _, cities := range self.regionCities {
		slices.SortFunc(cities, byLatitude)
	}
	for alias, regionKey := range aliasRegionKeys {
		self.regionCities[alias] = self.regionCities[regionKey]
	}
	return self
}

// The region hinge at p, in km. A region the list has no city for, or a point
// that is not finite, gives no bias.
func (self *PlaceContainment) RegionHinge(p LatLon, regionKey string) float64 {
	if self == nil {
		return 0
	}
	return self.hinge(p, self.regionCities[regionKey])
}

// The country hinge at p, in km. A country the list has no city for, or a
// point that is not finite, gives no bias.
func (self *PlaceContainment) CountryHinge(p LatLon, countryKey string) float64 {
	if self == nil {
		return 0
	}
	return self.hinge(p, self.countryCities[ContainmentCountryKey(countryKey)])
}

// The hinge at p for the cities inside one region or country, d_in −
// d_nearest (see the type); 0 when there is no inside city or p is not finite.
func (self *PlaceContainment) hinge(p LatLon, inside []containmentCity) float64 {
	if len(inside) == 0 {
		return 0
	}
	// normalized the way NearestCity normalizes its query, so a hinge and a
	// reverse-geocoded distance are measured from the same point
	if math.IsNaN(p.Latitude) || math.IsInf(p.Latitude, 0) || math.IsNaN(p.Longitude) || math.IsInf(p.Longitude, 0) {
		return 0
	}
	query := newContainmentCity(min(90, max(-90, p.Latitude)), normalizeLongitude(p.Longitude))
	insideCity, insideSquaredChord := nearestInLatitudeOrder(inside, query)
	nearestCity, nearestSquaredChord := nearestInLatitudeOrder(self.cities, query)
	// The nearest city overall is inside, or as near as one inside: exactly
	// zero, not the rounding between two distances.
	if insideSquaredChord <= nearestSquaredChord {
		return 0
	}
	return max(0, query.surfaceKm(insideCity)-query.surfaceKm(nearestCity))
}

// The nearest of cities ordered by latitude to a query, and its squared
// chord. A great-circle path covers at least the difference in latitude of its
// ends, so the scan walks out from the query's latitude both ways and stops
// each way at the first city whose latitude alone is farther than the best
// found. The scan is exact; only its cost depends on the city density, and a
// point far from any city (an ocean, a desert) widens it. cities is not empty.
func nearestInLatitudeOrder(cities []containmentCity, query containmentCity) (containmentCity, float64) {
	start, _ := slices.BinarySearchFunc(cities, query.latitude, func(city containmentCity, latitude float64) int {
		return cmp.Compare(city.latitude, latitude)
	})
	best := cities[min(start, len(cities)-1)]
	bestSquaredChord := query.squaredChord(best)
	// the angle the best chord spans, the bound a latitude gap is held to
	bestAngle := 2 * math.Asin(min(1, math.Sqrt(bestSquaredChord)/2))
	consider := func(city containmentCity) {
		if squaredChord := query.squaredChord(city); squaredChord < bestSquaredChord {
			best = city
			bestSquaredChord = squaredChord
			bestAngle = 2 * math.Asin(min(1, math.Sqrt(squaredChord)/2))
		}
	}
	for i := start; i < len(cities); i += 1 {
		if bestAngle < cities[i].latitude-query.latitude {
			break
		}
		consider(cities[i])
	}
	for i := start - 1; 0 <= i; i -= 1 {
		if bestAngle < query.latitude-cities[i].latitude {
			break
		}
		consider(cities[i])
	}
	return best, bestSquaredChord
}
