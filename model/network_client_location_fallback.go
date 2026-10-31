// The location fallback of FindProviders2 (owner decision 2026-10-31).
//
// A city target the score export published without an admitted provider is
// answered from its region, then from its country if the region has none
// either; a region target from its country. Only a lack of supply climbs: the
// request climbs while every target it has read was published empty, which is
// the cache_empty boundary of the selection observation
// (targetsWithoutSupply). A target whose admitted providers the request's own
// exclusions or filters removed is not climbed past, since a client that
// excludes them already holds them, nor is a target with missing or partial
// cache evidence. A level never leaves the target's country.
//
// Each level is one more attempt over the same request, so the request's IP
// family, rank mode, exclusions, network-only access, capability filters,
// caps and counts all hold there. A provider a level answers carries the
// level as its FindProvidersProvider.Match; the request's own targets answer
// without one, so an answer that did not climb is unchanged on the wire. A
// strict request, or egress_index.location_fallback false in provider.yml,
// keeps the exact answer.
//
// The parents come from the location directory, which holds every location a
// connected provider names: a target the directory does not hold, or holds
// without its parents, does not climb.
package model

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/server"
)

// FindProviders2 answers to a request whose city or region targets were
// published without an admitted provider, by the request's target kind and
// the level that answered it, or none when no level had a provider for the
// request. Both labels are fixed vocabularies, never a target identity. A
// strict request does not climb and is not counted. The outcome counters
// count the same answers by their final size, so a climbed answer with
// providers is not an empty one there.
var findProviders2LocationFallbackTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "urnetwork_findproviders2_location_fallback_total",
	Help: "FindProviders2 answers to city or region targets published without an admitted provider, by requested target kind and the level that answered (region, country, or none when no level had a provider for the request)",
}, []string{"target_kind", "level"})

// The level label of an answer no level had a provider for.
const findProviders2LocationFallbackNone = "none"

func init() {
	prometheus.MustRegister(findProviders2LocationFallbackTotal)
	// the series a dashboard reads exist from the first scrape
	for _, level := range []string{LocationTypeRegion, LocationTypeCountry, findProviders2LocationFallbackNone} {
		findProviders2LocationFallbackTotal.WithLabelValues(LocationTypeCity, level)
	}
	for _, level := range []string{LocationTypeCountry, findProviders2LocationFallbackNone} {
		findProviders2LocationFallbackTotal.WithLabelValues(LocationTypeRegion, level)
	}
}

// One level of a request's location fallback: the targets it reads and the
// match its providers carry.
type findProviders2FallbackLevel struct {
	match       LocationType
	locationIds map[server.Id]bool
}

// The levels one request climbs, nearest first. Owned by one request.
type findProviders2LocationFallback struct {
	// whether the request names a city or region target the directory holds
	climbable bool
	levels    []*findProviders2FallbackLevel
}

// The fallback of a request's location targets: the regions of its city
// targets, then the countries of its city and region targets. A parent is
// taken only when the directory types it as that level, in the target's own
// country. A level leaves out the targets the request or a nearer level
// already reads, which were read without supply, and an empty level is left
// out. Country, group and best-available targets have no level above them.
func newFindProviders2LocationFallback(
	locationIds map[server.Id]bool,
	directory map[server.Id]*locationDirectoryEntry,
) *findProviders2LocationFallback {
	self := &findProviders2LocationFallback{}
	// a parent the request does not already read
	addParent := func(entry *locationDirectoryEntry, parentLocationId *server.Id, parentLocationType LocationType, levelLocationIds map[server.Id]bool) {
		if parentLocationId == nil || locationIds[*parentLocationId] {
			return
		}
		parentEntry := directory[*parentLocationId]
		if parentEntry == nil || parentEntry.LocationType != parentLocationType || parentEntry.CountryCode != entry.CountryCode {
			return
		}
		levelLocationIds[*parentLocationId] = true
	}
	regionLocationIds := map[server.Id]bool{}
	countryLocationIds := map[server.Id]bool{}
	for locationId := range locationIds {
		entry := directory[locationId]
		if entry == nil || (entry.LocationType != LocationTypeCity && entry.LocationType != LocationTypeRegion) {
			continue
		}
		self.climbable = true
		if entry.LocationType == LocationTypeCity {
			addParent(entry, entry.RegionLocationId, LocationTypeRegion, regionLocationIds)
		}
		addParent(entry, entry.CountryLocationId, LocationTypeCountry, countryLocationIds)
	}
	for _, level := range []*findProviders2FallbackLevel{
		{match: LocationTypeRegion, locationIds: regionLocationIds},
		{match: LocationTypeCountry, locationIds: countryLocationIds},
	} {
		if 0 < len(level.locationIds) {
			self.levels = append(self.levels, level)
		}
	}
	return self
}

// The next level to read, nearest first, or false when none is left.
func (self *findProviders2LocationFallback) nextLevel() (*findProviders2FallbackLevel, bool) {
	if len(self.levels) == 0 {
		return nil, false
	}
	level := self.levels[0]
	self.levels = self.levels[1:]
	return level, true
}

// Counts the request's answer once, when its targets could climb. match is
// the level of the attempt that answered, and answered whether its discovery
// found a provider.
func (self *findProviders2LocationFallback) record(targetKind string, match LocationType, answered bool) {
	if !self.climbable {
		return
	}
	level := findProviders2LocationFallbackNone
	if answered && match != "" {
		level = match
	}
	findProviders2LocationFallbackTotal.WithLabelValues(targetKind, level).Inc()
}
