package geo

import (
	"strings"
)

// The place list indexed by normalized region and city names, for resolving
// stored location rows against it (connect/GEOMAP.md §4.2).

// Indexes a place list by a normalized form of its region and city names: the
// anchors a stored location row, which may predate geoname ids, is resolved
// against (connect/GEOMAP.md §4.2). A row whose normalized name is the
// normalized name of a place in its region is that place; only a name that is
// no place's may be matched loosely, and then only to a single place. The
// normalization belongs to the server's location matcher, which passes it in,
// so that there is one definition of it; it is applied here once per name.
type PlaceNames struct {
	countries map[string]*CountryNames
}

// One country's regions and cities.
type CountryNames struct {
	// ISO 3166-1 alpha-2, lower case
	Code string
	// in the list's order
	Regions []*RegionNames
	// every city of the country, in the list's order: the scope of a city whose
	// own region resolves to none of the country's regions
	Cities []*NamedPlace

	regionsByName           map[string]*RegionNames
	regionsByGeonameId      map[uint32]*RegionNames
	regionsByNormalizedName map[string][]*RegionNames
	citiesByNormalizedName  map[string][]*NamedPlace
}

// One region of the list and its cities.
type RegionNames struct {
	// as the list names it
	Name string
	// the subdivision's GeoNames id, 0 for the region named for the country
	// that files the cities GeoLite2 puts under no subdivision
	GeonameId  uint32
	Normalized string
	// in the list's order
	Cities []*NamedPlace

	citiesByNormalizedName map[string][]*NamedPlace
}

// A city of the list with its normalized name.
type NamedPlace struct {
	Place      *Place
	Normalized string
}

// Indexes every region and city of a place list by the given normalization of
// its name.
func NewPlaceNames(places *Places, normalize func(string) string) *PlaceNames {
	self := &PlaceNames{
		countries: map[string]*CountryNames{},
	}
	for place := range places.Cities() {
		country, ok := self.countries[place.CountryCode]
		if !ok {
			country = &CountryNames{
				Code:                    place.CountryCode,
				regionsByName:           map[string]*RegionNames{},
				regionsByGeonameId:      map[uint32]*RegionNames{},
				regionsByNormalizedName: map[string][]*RegionNames{},
				citiesByNormalizedName:  map[string][]*NamedPlace{},
			}
			self.countries[place.CountryCode] = country
		}
		region, ok := country.regionsByName[place.Region]
		if !ok {
			region = &RegionNames{
				Name:                   place.Region,
				Normalized:             normalize(place.Region),
				citiesByNormalizedName: map[string][]*NamedPlace{},
			}
			country.regionsByName[place.Region] = region
			country.Regions = append(country.Regions, region)
			country.regionsByNormalizedName[region.Normalized] = append(country.regionsByNormalizedName[region.Normalized], region)
		}
		// A real subdivision named like its country shares the country-named
		// region's key in the list, as its rows share one region row; the region
		// takes the subdivision's id.
		if region.GeonameId == 0 && place.RegionGeonameId != 0 {
			region.GeonameId = place.RegionGeonameId
			country.regionsByGeonameId[region.GeonameId] = region
		}
		namedPlace := &NamedPlace{
			Place:      place,
			Normalized: normalize(place.City),
		}
		region.Cities = append(region.Cities, namedPlace)
		region.citiesByNormalizedName[namedPlace.Normalized] = append(region.citiesByNormalizedName[namedPlace.Normalized], namedPlace)
		country.Cities = append(country.Cities, namedPlace)
		country.citiesByNormalizedName[namedPlace.Normalized] = append(country.citiesByNormalizedName[namedPlace.Normalized], namedPlace)
	}
	return self
}

// A country's names by its code in either case, or nil when the list has no
// city in it.
func (self *PlaceNames) Country(code string) *CountryNames {
	return self.countries[strings.ToLower(code)]
}

// The region the list names exactly so, or nil.
func (self *CountryNames) Region(name string) *RegionNames {
	return self.regionsByName[name]
}

// The region of a subdivision's GeoNames id, or nil.
func (self *CountryNames) RegionByGeonameId(geonameId uint32) *RegionNames {
	if geonameId == 0 {
		return nil
	}
	return self.regionsByGeonameId[geonameId]
}

// The regions of the country with a normalized name.
func (self *CountryNames) RegionsNamed(normalized string) []*RegionNames {
	return self.regionsByNormalizedName[normalized]
}

// The cities of the country with a normalized name.
func (self *CountryNames) CitiesNamed(normalized string) []*NamedPlace {
	return self.citiesByNormalizedName[normalized]
}

// The cities of the region with a normalized name.
func (self *RegionNames) CitiesNamed(normalized string) []*NamedPlace {
	return self.citiesByNormalizedName[normalized]
}
