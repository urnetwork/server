package geo

import (
	"math"
	"strings"
)

// The representative city and width of every region and country of the place
// list, the anchor of a location that names no finer place
// (connect/GEOMAP.md §5.1).

// Where a region or a country stands for a location that names no finer place
// (connect/GEOMAP.md §5.1). GeoLite2 places a network it cannot put in a city
// at a point of its region or country, and the location rows keep only the
// region or country that point was in, never the point: a region or country
// row carries no coordinates. The derive phase still has to anchor such a node
// somewhere, and the anchor has to be as wide as the region or country is, or
// a node the lookup could only put "somewhere in the country" would be held to
// one corner of it.
//
// The point is the city of the region (country) nearest the spherical mean of
// its cities -- a city, so the stand-in is always inside the region and the
// country the location names, which the mean of a curved or scattered region
// need not be -- and the width is the RMS great-circle distance of the region's
// cities from that city.
type Representative struct {
	Place    *Place
	SpreadKm float64
}

// Indexes the representative of every region and country of a place list.
// Immutable after NewRepresentatives and safe for concurrent use.
type Representatives struct {
	countryRepresentatives map[string]Representative
	// by the region's name within its country
	regionRepresentatives map[regionKey]Representative
	// by the region's GeoNames id, for the regions that have one
	regionGeonameIdRepresentatives map[uint32]Representative
}

// the cities of one region or country, as indexes into Places.places, and the
// sum of their unit vectors
type representativeGroup struct {
	x            float64
	y            float64
	z            float64
	placeIndexes []int
}

// Adds the city at a place index, with its unit vector.
func (self *representativeGroup) add(placeIndex int, vector [3]float64) {
	self.x += vector[0]
	self.y += vector[1]
	self.z += vector[2]
	self.placeIndexes = append(self.placeIndexes, placeIndex)
}

// Finds every region's and country's representative: one pass over the list's
// cities sums each group's unit vectors, and then each group is scanned once
// for its city nearest the mean and once for its spread.
func NewRepresentatives(places *Places) *Representatives {
	// the unit vector of a coordinate in degrees
	unitVector := func(latitude float64, longitude float64) [3]float64 {
		const radiansPerDegree = math.Pi / 180
		sinLatitude, cosLatitude := math.Sincos(latitude * radiansPerDegree)
		sinLongitude, cosLongitude := math.Sincos(longitude * radiansPerDegree)
		return [3]float64{cosLatitude * cosLongitude, cosLatitude * sinLongitude, sinLatitude}
	}
	vectors := make([][3]float64, len(places.places))
	countryGroups := map[string]*representativeGroup{}
	regionGroups := map[regionKey]*representativeGroup{}
	for i := range places.places {
		place := &places.places[i]
		vectors[i] = unitVector(place.Latitude, place.Longitude)

		countryGroup, ok := countryGroups[place.CountryCode]
		if !ok {
			countryGroup = &representativeGroup{}
			countryGroups[place.CountryCode] = countryGroup
		}
		countryGroup.add(i, vectors[i])

		key := regionKey{countryCode: place.CountryCode, region: place.Region}
		regionGroup, ok := regionGroups[key]
		if !ok {
			regionGroup = &representativeGroup{}
			regionGroups[key] = regionGroup
		}
		regionGroup.add(i, vectors[i])
	}

	representative := func(group *representativeGroup) Representative {
		norm := math.Sqrt(group.x*group.x + group.y*group.y + group.z*group.z)
		best := -1
		bestCosine := 0.0
		for _, i := range group.placeIndexes {
			// Members spread evenly round the whole sphere have no mean
			// direction; every cosine is then 0 and the tie below picks the
			// smallest geoname id, which is still a member.
			cosine := 0.0
			if 0 < norm {
				cosine = (vectors[i][0]*group.x + vectors[i][1]*group.y + vectors[i][2]*group.z) / norm
			}
			if best < 0 ||
				bestCosine < cosine ||
				(cosine == bestCosine && places.places[i].GeonameId < places.places[best].GeonameId) {
				best = i
				bestCosine = cosine
			}
		}
		place := &places.places[best]
		var sumSquares float64
		for _, i := range group.placeIndexes {
			member := &places.places[i]
			distanceKm := DistanceKm(place.Latitude, place.Longitude, member.Latitude, member.Longitude)
			sumSquares += distanceKm * distanceKm
		}
		return Representative{
			Place:    place,
			SpreadKm: math.Sqrt(sumSquares / float64(len(group.placeIndexes))),
		}
	}

	self := &Representatives{
		countryRepresentatives:         make(map[string]Representative, len(countryGroups)),
		regionRepresentatives:          make(map[regionKey]Representative, len(regionGroups)),
		regionGeonameIdRepresentatives: map[uint32]Representative{},
	}
	for countryCode, group := range countryGroups {
		self.countryRepresentatives[countryCode] = representative(group)
	}
	for key, group := range regionGroups {
		regionRepresentative := representative(group)
		self.regionRepresentatives[key] = regionRepresentative
		if regionGeonameId := places.RegionGeonameId(key.countryCode, key.region); regionGeonameId != 0 {
			self.regionGeonameIdRepresentatives[regionGeonameId] = regionRepresentative
		}
	}
	return self
}

// The representative of a country by its code, in either case.
func (self *Representatives) Country(countryCode string) (Representative, bool) {
	representative, ok := self.countryRepresentatives[strings.ToLower(countryCode)]
	return representative, ok
}

// The representative of a region of a country: by its GeoNames id when the
// list knows the id, else by its name. A stored region row may spell its name
// the way an older source did; its id still finds it, and the name finds a row
// that has no id.
func (self *Representatives) Region(countryCode string, regionGeonameId uint32, region string) (Representative, bool) {
	countryCode = strings.ToLower(countryCode)
	if regionGeonameId != 0 {
		if representative, ok := self.regionGeonameIdRepresentatives[regionGeonameId]; ok && representative.Place.CountryCode == countryCode {
			return representative, true
		}
	}
	representative, ok := self.regionRepresentatives[regionKey{countryCode: countryCode, region: region}]
	return representative, ok
}
