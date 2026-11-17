package controller

import (
	"cmp"
	"context"
	"math"
	"net/netip"
	"slices"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The extenders of a drift test's run (lag.dev/DRIFTTEST.md E2). The server
// chooses them, never the browser, so a client cannot steer the geometry.
// From the extenders the lag.dev sample offers now (A4), it takes 12 around
// the start address's place:
//
//   - near: the 4 nearest, preferring 2 inside the IP country, since near
//     extenders constrain the position most tightly (V3);
//   - mid: 4 between 300 and 2,500 km, spread by bearing;
//   - far: 4 between 2,500 and 9,000 km, spread the same way;
//   - spares: up to 4 more, in positions 12-15, opened by the browser only
//     for a session that fails to open within 4 s.
//
// The spread is greedy: each pick has the largest smallest angular gap to the
// band's picks so far, so the ellipse stays round. A retest prefers, within
// each band, extenders the window's runs have not used, since a new path
// brings independent route error and a repeated one only adds pings (V7), and
// falls back to repeats where a band has no others. A band short of
// extenders is made up from the nearest left, and an address placed nowhere
// gets the sample's spread across countries.

// The bands' bounds in km (E2).
const (
	lagDriftMidMinKm = 300.0
	lagDriftMidMaxKm = 2500.0
	lagDriftFarMaxKm = 9000.0
	// the near band's extenders inside the IP country, when it has them
	lagDriftNearInCountry = 2
)

// The mean earth radius for great-circle distances, as the drift model's
// (V2).
const lagDriftEarthRadiusKm = 6371.0

// A point on the earth, in degrees.
type lagDriftPoint struct {
	latitude  float64
	longitude float64
}

// The great-circle distance between two points in km, and the initial
// bearing from the first to the second in degrees clockwise from north.
func lagDriftDistanceBearing(from lagDriftPoint, to lagDriftPoint) (distanceKm float64, bearingDeg float64) {
	radians := math.Pi / 180
	lat1 := from.latitude * radians
	lat2 := to.latitude * radians
	dLat := lat2 - lat1
	dLon := (to.longitude - from.longitude) * radians
	a := math.Sin(dLat/2)*math.Sin(dLat/2) + math.Cos(lat1)*math.Cos(lat2)*math.Sin(dLon/2)*math.Sin(dLon/2)
	distanceKm = 2 * lagDriftEarthRadiusKm * math.Atan2(math.Sqrt(a), math.Sqrt(max(1-a, 0)))
	y := math.Sin(dLon) * math.Cos(lat2)
	x := math.Cos(lat1)*math.Sin(lat2) - math.Sin(lat1)*math.Cos(lat2)*math.Cos(dLon)
	bearingDeg = math.Mod(math.Atan2(y, x)/radians+360, 360)
	return
}

// The angle between two bearings, 0 to 180.
func lagDriftAngleBetween(a float64, b float64) float64 {
	angle := math.Mod(math.Abs(a-b), 360)
	return min(angle, 360-angle)
}

// The place of a start's address (P5, E1): the country, region and city
// GeoLite2 gives, with the city's own point when the place list holds it
// (lagCityPoint), else GeoLite2's own point. The point centers the radar and
// the extenders' bands; the track keeps only its city-level client fields
// (A5), and nothing of this outlives the request.
type lagDriftAddrPlace struct {
	CountryCode string
	CountryName string
	Region      string
	City        string
	// nil when GeoLite2 places the address nowhere
	Point *lagDriftPoint
}

func lagDriftStartPlace(addr netip.Addr) *lagDriftAddrPlace {
	place := &lagDriftAddrPlace{}
	server.HandleError(func() {
		ipInfo, err := server.GetIpInfo(addr.Unmap().WithZone(""))
		if err != nil || ipInfo.CountryCode == "" {
			return
		}
		place.CountryCode = ipInfo.CountryCode
		place.CountryName = ipInfo.Country
		place.Region = ipInfo.Region
		place.City = ipInfo.City
		place.Point = &lagDriftPoint{latitude: ipInfo.Latitude, longitude: ipInfo.Longitude}
		if ipInfo.City != "" {
			if latitude, longitude, ok := lagCityPoint(ipInfo.CityGeonameId); ok {
				place.Point = &lagDriftPoint{latitude: latitude, longitude: longitude}
			}
		}
	})
	return place
}

// The place as the start answers it (P5).
func (self *lagDriftAddrPlace) view() *LagDriftIpPlace {
	view := &LagDriftIpPlace{
		CountryCode: self.CountryCode,
		CountryName: self.CountryName,
		Region:      self.Region,
		City:        self.City,
	}
	if self.Point != nil {
		latitude := self.Point.latitude
		longitude := self.Point.longitude
		view.Latitude = &latitude
		view.Longitude = &longitude
	}
	return view
}

// One extender chosen for a run, at its position.
type lagDriftExtender struct {
	extender *lagServingExtender
	position int
	// from the start address's place; 0 without one
	distanceKm float64
	bearingDeg float64
	// a window run used it (E2)
	used         bool
	inCountry    bool
	placeErrorKm float64
}

// How many of the chosen are the run's own extenders, its spares apart.
func lagDriftMainCount(chosen []*lagDriftExtender, drift *LagDriftSettings) int {
	count := 0
	for _, choice := range chosen {
		if choice.position < drift.Extenders {
			count += 1
		}
	}
	return count
}

// The chosen extenders as the run keeps them (E2, V3).
func lagDriftRunExtenders(chosen []*lagDriftExtender) []*model.LagDriftRunExtender {
	extenders := []*model.LagDriftRunExtender{}
	for _, choice := range chosen {
		extenders = append(extenders, &model.LagDriftRunExtender{
			Position:     choice.position,
			ExtenderId:   choice.extender.ExtenderId,
			CountryCode:  choice.extender.Place.CountryCode,
			Latitude:     choice.extender.Place.Latitude,
			Longitude:    choice.extender.Place.Longitude,
			PlaceErrorKm: choice.placeErrorKm,
		})
	}
	return extenders
}

// Chooses a run's extenders from what the sample offers now (E2), drops any
// that no longer serve at `now`, as a track's dots are (lagStillServing),
// and places each with its error (V3).
func lagDriftChooseExtenders(
	ctx context.Context,
	now time.Time,
	settings *LagSettings,
	place *lagDriftAddrPlace,
	used map[server.Id]bool,
) []*lagDriftExtender {
	set := lagDriftWithout(getLagServing(ctx, now, settings.ExtenderSampleCache), settings.Drift.Excluded())
	chosen := lagDriftChoose(set, place, used, &settings.Drift)

	extenders := make([]*lagServingExtender, 0, len(chosen))
	for _, choice := range chosen {
		extenders = append(extenders, choice.extender)
	}
	serving := map[server.Id]bool{}
	for _, extender := range lagStillServing(ctx, extenders, now) {
		serving[extender.ExtenderId] = true
	}
	kept := []*lagDriftExtender{}
	placed := []*model.LagDriftPlacedExtender{}
	for _, choice := range chosen {
		if serving[choice.extender.ExtenderId] {
			kept = append(kept, choice)
			placed = append(placed, &model.LagDriftPlacedExtender{
				Place: choice.extender.Place,
				Ips:   choice.extender.Ips,
			})
		}
	}
	for i, placeErrorKm := range model.GetLagDriftExtenderPlaceErrorsKm(placed) {
		kept[i].placeErrorKm = placeErrorKm
	}
	return kept
}

// The serving set without the extenders an operator excluded
// (lag.yml drift.excluded_extenders).
func lagDriftWithout(set *lagServingSet, excluded map[server.Id]bool) *lagServingSet {
	if len(excluded) == 0 {
		return set
	}
	kept := &lagServingSet{loadTime: set.loadTime, byId: map[server.Id]*lagServingExtender{}}
	for _, extender := range set.extenders {
		if !excluded[extender.ExtenderId] {
			kept.extenders = append(kept.extenders, extender)
			kept.byId[extender.ExtenderId] = extender
		}
	}
	return kept
}

// The choice of E2 from a serving set, with positions: the run's extenders
// from 0, near then mid then far, and the spares from drift.Extenders. Pure.
func lagDriftChoose(
	set *lagServingSet,
	place *lagDriftAddrPlace,
	used map[server.Id]bool,
	drift *LagDriftSettings,
) []*lagDriftExtender {
	if place == nil || place.Point == nil {
		return lagDriftChooseUnplaced(set, used, drift)
	}

	candidates := []*lagDriftExtender{}
	seen := map[server.Id]bool{}
	for _, extender := range set.extenders {
		if seen[extender.ExtenderId] {
			continue
		}
		seen[extender.ExtenderId] = true
		distanceKm, bearingDeg := lagDriftDistanceBearing(
			*place.Point,
			lagDriftPoint{latitude: extender.Place.Latitude, longitude: extender.Place.Longitude},
		)
		candidates = append(candidates, &lagDriftExtender{
			extender:   extender,
			distanceKm: distanceKm,
			bearingDeg: bearingDeg,
			used:       used[extender.ExtenderId],
			inCountry:  place.CountryCode != "" && extender.Place.CountryCode == place.CountryCode,
		})
	}
	// nearest first; equal distances by id, so the choice is stable
	slices.SortStableFunc(candidates, func(a *lagDriftExtender, b *lagDriftExtender) int {
		if c := cmp.Compare(a.distanceKm, b.distanceKm); c != 0 {
			return c
		}
		return cmp.Compare(a.extender.ExtenderId.String(), b.extender.ExtenderId.String())
	})

	nearCount := drift.Extenders / 3
	midCount := drift.Extenders / 3
	farCount := drift.Extenders - nearCount - midCount

	taken := map[server.Id]bool{}
	chosen := []*lagDriftExtender{}
	take := func(picks ...*lagDriftExtender) {
		for _, pick := range picks {
			taken[pick.extender.ExtenderId] = true
			chosen = append(chosen, pick)
		}
	}
	remaining := func(keep func(candidate *lagDriftExtender) bool) []*lagDriftExtender {
		left := []*lagDriftExtender{}
		for _, candidate := range candidates {
			if !taken[candidate.extender.ExtenderId] && keep(candidate) {
				left = append(left, candidate)
			}
		}
		return left
	}

	// near: the band reaches the nearest nearCount, and at least the mid
	// band's start; unused extenders within it come first, then repeats,
	// then anything nearer than the rest
	nearLimitKm := lagDriftMidMinKm
	if 0 < nearCount && nearCount <= len(candidates) {
		nearLimitKm = max(nearLimitKm, candidates[nearCount-1].distanceKm)
	}
	nearOrder := func(candidates []*lagDriftExtender) []*lagDriftExtender {
		ordered := slices.Clone(candidates)
		slices.SortStableFunc(ordered, func(a *lagDriftExtender, b *lagDriftExtender) int {
			beyondA := nearLimitKm < a.distanceKm
			beyondB := nearLimitKm < b.distanceKm
			if beyondA != beyondB {
				if beyondA {
					return 1
				}
				return -1
			}
			if a.used != b.used {
				if a.used {
					return 1
				}
				return -1
			}
			return 0
		})
		return ordered
	}
	inCountry := nearOrder(remaining(func(candidate *lagDriftExtender) bool {
		return candidate.inCountry && candidate.distanceKm <= lagDriftMidMaxKm
	}))
	take(inCountry[:min(lagDriftNearInCountry, nearCount, len(inCountry))]...)
	near := nearOrder(remaining(func(*lagDriftExtender) bool { return true }))
	take(near[:min(nearCount-len(chosen), len(near))]...)

	// mid and far: spread by bearing, unused extenders first
	spreadBand := func(minKm float64, maxKm float64, count int) {
		band := remaining(func(candidate *lagDriftExtender) bool {
			return minKm <= candidate.distanceKm && candidate.distanceKm < maxKm
		})
		unused := []*lagDriftExtender{}
		repeats := []*lagDriftExtender{}
		for _, candidate := range band {
			if candidate.used {
				repeats = append(repeats, candidate)
			} else {
				unused = append(unused, candidate)
			}
		}
		picks := lagDriftSpread(unused, count, nil)
		bearings := []float64{}
		for _, pick := range picks {
			bearings = append(bearings, pick.bearingDeg)
		}
		picks = append(picks, lagDriftSpread(repeats, count-len(picks), bearings)...)
		take(picks...)
	}
	spreadBand(lagDriftMidMinKm, lagDriftMidMaxKm, midCount)
	spreadBand(lagDriftMidMaxKm, math.Nextafter(lagDriftFarMaxKm, math.Inf(1)), farCount)

	// a short band is made up from the nearest left, unused first
	byUse := func(candidates []*lagDriftExtender) []*lagDriftExtender {
		ordered := slices.Clone(candidates)
		slices.SortStableFunc(ordered, func(a *lagDriftExtender, b *lagDriftExtender) int {
			if a.used != b.used {
				if a.used {
					return 1
				}
				return -1
			}
			return 0
		})
		return ordered
	}
	fill := byUse(remaining(func(*lagDriftExtender) bool { return true }))
	take(fill[:min(drift.Extenders-len(chosen), len(fill))]...)
	for position, choice := range chosen {
		choice.position = position
	}

	// spares, nearest first, unused first
	spares := byUse(remaining(func(*lagDriftExtender) bool { return true }))
	spares = spares[:min(drift.Spares, len(spares))]
	for i, spare := range spares {
		spare.position = drift.Extenders + i
	}
	return append(chosen, spares...)
}

// The choice for an address placed nowhere: the sample's spread across
// countries (sampleLagServing), unused extenders first.
func lagDriftChooseUnplaced(set *lagServingSet, used map[server.Id]bool, drift *LagDriftSettings) []*lagDriftExtender {
	sample := sampleLagServing(set, len(set.extenders))
	ordered := []*lagDriftExtender{}
	for _, extender := range sample {
		ordered = append(ordered, &lagDriftExtender{extender: extender, used: used[extender.ExtenderId]})
	}
	slices.SortStableFunc(ordered, func(a *lagDriftExtender, b *lagDriftExtender) int {
		if a.used != b.used {
			if a.used {
				return 1
			}
			return -1
		}
		return 0
	})
	// the run's extenders from 0 and the spares after them, as for a placed
	// address
	chosen := ordered[:min(drift.Extenders+drift.Spares, len(ordered))]
	for i, choice := range chosen {
		choice.position = i
	}
	return chosen
}

// Up to count of the candidates, nearest first, spread by bearing: each pick
// has the largest smallest angle to the bearings already picked, the nearer on
// a tie. Pure.
func lagDriftSpread(candidates []*lagDriftExtender, count int, bearings []float64) []*lagDriftExtender {
	bearings = slices.Clone(bearings)
	left := slices.Clone(candidates)
	picks := []*lagDriftExtender{}
	for len(picks) < count && 0 < len(left) {
		bestIndex := 0
		bestGap := -1.0
		for i, candidate := range left {
			gap := 360.0
			for _, bearing := range bearings {
				gap = min(gap, lagDriftAngleBetween(candidate.bearingDeg, bearing))
			}
			if bestGap+1e-9 < gap {
				bestIndex = i
				bestGap = gap
			}
		}
		pick := left[bestIndex]
		picks = append(picks, pick)
		bearings = append(bearings, pick.bearingDeg)
		left = slices.Delete(left, bestIndex, bestIndex+1)
	}
	return picks
}
