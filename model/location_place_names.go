package model

import (
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/geo"
)

// The canonical place list as the location matcher uses it, and the
// process's one list for matching at create time, loaded on first use.

// The canonical place list as the location matcher uses it
// (location_match.go): the list, its names normalized (geo.PlaceNames), and
// the matcher's scopes. Safe for concurrent use: each scope is built on first
// use under the state lock and immutable after, so it is resolved against
// without the lock.
type locationPlaceNames struct {
	places *geo.Places
	names  *geo.PlaceNames

	stateLock           sync.Mutex
	countryRegionScopes map[*geo.CountryNames]*geoNameScope
	regionCityScopes    map[*geo.RegionNames]*geoNameScope
	countryCityScopes   map[*geo.CountryNames]*geoNameScope
	// one candidate per city, shared by its region's scope and its country's
	cityCandidates map[*geo.NamedPlace]*geoNameCandidate
}

// Indexes a place list's names for the matcher; the scopes come later, as
// lookups touch them.
func newLocationPlaceNames(places *geo.Places) *locationPlaceNames {
	return &locationPlaceNames{
		places:              places,
		names:               geo.NewPlaceNames(places, normalizePlaceName),
		countryRegionScopes: map[*geo.CountryNames]*geoNameScope{},
		regionCityScopes:    map[*geo.RegionNames]*geoNameScope{},
		countryCityScopes:   map[*geo.CountryNames]*geoNameScope{},
		cityCandidates:      map[*geo.NamedPlace]*geoNameCandidate{},
	}
}

// Resolves a region row's name among the regions of its country.
func (self *locationPlaceNames) resolveRegion(countryCode string, name string) placeResolution {
	country := self.names.Country(countryCode)
	if country == nil {
		return placeResolution{kind: placeUnresolved}
	}
	var scope *geoNameScope
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		var ok bool
		scope, ok = self.countryRegionScopes[country]
		if !ok {
			candidates := make([]*geoNameCandidate, 0, len(country.Regions))
			for _, region := range country.Regions {
				candidate := newGeoNameCandidate(region.Name, region.Normalized, region.GeonameId)
				candidate.region = region
				candidates = append(candidates, candidate)
			}
			scope = newGeoNameScope(candidates)
			self.countryRegionScopes[country] = scope
		}
	}()
	return scope.resolve(name)
}

// Resolves a city row's name among the cities of a region of the list, or of
// its whole country when its region row resolves to none (region nil).
func (self *locationPlaceNames) resolveCity(countryCode string, region *geo.RegionNames, name string) placeResolution {
	country := self.names.Country(countryCode)
	if country == nil {
		return placeResolution{kind: placeUnresolved}
	}
	var scope *geoNameScope
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		var ok bool
		if region != nil {
			scope, ok = self.regionCityScopes[region]
			if !ok {
				scope = self.newCityScopeWithLock(region.Cities)
				self.regionCityScopes[region] = scope
			}
		} else {
			scope, ok = self.countryCityScopes[country]
			if !ok {
				scope = self.newCityScopeWithLock(country.Cities)
				self.countryCityScopes[country] = scope
			}
		}
	}()
	return scope.resolve(name)
}

// Indexes cities for resolution, sharing each city's candidate between the
// scopes it is in.
func (self *locationPlaceNames) newCityScopeWithLock(cities []*geo.NamedPlace) *geoNameScope {
	candidates := make([]*geoNameCandidate, 0, len(cities))
	for _, city := range cities {
		candidate, ok := self.cityCandidates[city]
		if !ok {
			candidate = newGeoNameCandidate(city.Place.City, city.Normalized, city.Place.GeonameId)
			candidate.city = city
			self.cityCandidates[city] = candidate
		}
		candidates = append(candidates, candidate)
	}
	return newGeoNameScope(candidates)
}

// The list's region a stored region row is: the region of its geoname id,
// else the one its name resolves to, else nil.
func (self *locationPlaceNames) regionOfRow(countryCode string, name string, geonameId uint32) *geo.RegionNames {
	country := self.names.Country(countryCode)
	if country == nil {
		return nil
	}
	if geonameId != 0 {
		return country.RegionByGeonameId(geonameId)
	}
	if resolution := self.resolveRegion(countryCode, name); resolution.resolved() {
		return resolution.candidate.region
	}
	return nil
}

// The list's region of one of its cities.
func (self *locationPlaceNames) cityRegion(place *geo.Place) *geo.RegionNames {
	country := self.names.Country(place.CountryCode)
	if country == nil {
		return nil
	}
	return country.Region(place.Region)
}

// The process's place list for matching at create time. It is read from
// placesResource on first real use -- about a second, and ~300 MB of
// transient parse that the runtime keeps resident until later collections,
// for a list that keeps ~10 MB, its name index ~18 MB, and up to ~31 MB more
// as the scopes lookups touch are built -- and is nil when the resource is
// unavailable, in which case a lookup matches stored rows by geoname id and
// exact name only. The first real use is the seeder, the derive phase, the
// de-duplication, or a lookup whose place is not yet stored under its geoname
// id (createLocation); a process that does none of these never reads it. Every
// use shares the one load, so a process holds one copy. A test that pushes its
// own list resets this so the next use reads it (pushTestPlaces).
var matchingPlaceNames = &locationPlaceNamesState{
	load: &locationPlaceNamesLoad{},
}

// The process's place list, as the one load that yields it. The load reads
// and parses placesResource outside the state lock: concurrent first uses
// share one load, and a reset swaps in another, so it never waits for a load
// in flight.
type locationPlaceNamesState struct {
	stateLock sync.Mutex
	load      *locationPlaceNamesLoad
}

// One load of placesResource. It runs once, on the first get, and every get
// shares the list it yields; peek reports it without running it. Safe for
// concurrent use.
type locationPlaceNamesLoad struct {
	once       sync.Once
	loaded     atomic.Bool
	placeNames *locationPlaceNames
	err        error
}

// The list, loading it on the first call; nil and the reason when the
// resource is unavailable or does not parse.
func (self *locationPlaceNamesLoad) get() (*locationPlaceNames, error) {
	self.once.Do(func() {
		self.placeNames, self.err = func() (*locationPlaceNames, error) {
			resource, err := server.Config.SimpleResource(placesResource)
			if err != nil {
				return nil, err
			}
			placesBytes, err := resource.BytesE()
			if err != nil {
				return nil, err
			}
			places, err := geo.LoadPlaces(placesBytes)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", placesResource, err)
			}
			return newLocationPlaceNames(places), nil
		}()
		if self.err != nil {
			glog.Infof("[loc]no place list to resolve stored locations against; matching by geoname id and exact name only: %s\n", self.err)
		}
		self.loaded.Store(true)
	})
	return self.placeNames, self.err
}

// The list the load yielded and whether it has run, without running it.
func (self *locationPlaceNamesLoad) peek() (*locationPlaceNames, bool) {
	if !self.loaded.Load() {
		return nil, false
	}
	return self.placeNames, true
}

// The process's load in effect.
func currentLocationPlaceNamesLoad() *locationPlaceNamesLoad {
	var load *locationPlaceNamesLoad
	func() {
		matchingPlaceNames.stateLock.Lock()
		defer matchingPlaceNames.stateLock.Unlock()
		load = matchingPlaceNames.load
	}()
	return load
}

// The process's place list for matching, loaded on first use; nil when the
// deployment has none.
func currentLocationPlaceNames() *locationPlaceNames {
	placeNames, _ := currentLocationPlaceNamesLoad().get()
	return placeNames
}

// The process's canonical place list, the one CreateLocation resolves stored
// rows against, or nil when the deployment has none. The derive phase maps
// derived points into this list (connect/GEOMAP.md §6), so a mapped place and
// the location row it is stored under come from the same list and agree by
// construction.
func CurrentPlaces() *geo.Places {
	placeNames := currentLocationPlaceNames()
	if placeNames == nil {
		return nil
	}
	return placeNames.places
}

// Stands a place list in for the deployment's, for CreateLocation's matching
// and CurrentPlaces alike, and returns the function that restores the
// deployment's. Both sides reset the process's list, so the next use reads
// whichever is current. Test only.
func Testing_PushPlaces(placesYaml string) func() {
	pop := server.Config.PushSimpleResource(placesResource, []byte(placesYaml))
	resetLocationPlaceNames()
	return func() {
		pop()
		resetLocationPlaceNames()
	}
}

// Drops the process's list, so the next use loads placesResource again.
func resetLocationPlaceNames() {
	load := &locationPlaceNamesLoad{}
	func() {
		matchingPlaceNames.stateLock.Lock()
		defer matchingPlaceNames.stateLock.Unlock()
		matchingPlaceNames.load = load
	}()
}
