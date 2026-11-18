package controller

import (
	"cmp"
	"context"
	"math"
	"net/netip"
	"slices"
	"time"

	"github.com/urnetwork/connect/lagdrift"

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
	// it answers to its name pinned to the run's family (K6)
	pinned bool
}

// Whether an extender answers to its name pinned to IP family 4 or 6 (K6);
// never for a run that names no family.
func lagDriftPinned(extender *lagServingExtender, family int) bool {
	return family != 0 && extender.FamilyName(family) != ""
}

// The name a run's session connects to (K6): the extender's name pinned to
// the run's family when it answers to one, else its measurement name.
func lagDriftHost(extender *lagServingExtender, family int) string {
	if lagDriftPinned(extender, family) {
		return extender.FamilyName(family)
	}
	return extender.Name
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

// Chooses a run's extenders from what the sample offers now (E2), for a run
// started over IP family 4 or 6 (K6), with the model of the calibration in
// force (V3, V4), drops any that no longer serve at `now`, as a track's dots
// are (lagStillServing), and places each with its error (V3).
func lagDriftChooseExtenders(
	ctx context.Context,
	now time.Time,
	settings *LagSettings,
	place *lagDriftAddrPlace,
	used map[server.Id]bool,
	family int,
) []*lagDriftExtender {
	set := lagDriftWithout(getLagServing(ctx, now, settings.ExtenderSampleCache), settings.Drift.Excluded())
	driftModel := lagDriftCalibrationFor(ctx, &settings.Drift).Model()
	chosen := lagDriftChoose(set, place, used, &settings.Drift, family, driftModel, lagDriftPlaceErrorsKm)

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

// The place error of each extender, in order (V3, model.GetLagDriftExtenderPlaceErrorsKm).
func lagDriftPlaceErrorsKm(extenders []*lagServingExtender) []float64 {
	placed := make([]*model.LagDriftPlacedExtender, 0, len(extenders))
	for _, extender := range extenders {
		placed = append(placed, &model.LagDriftPlacedExtender{
			Place: extender.Place,
			Ips:   extender.Ips,
		})
	}
	return model.GetLagDriftExtenderPlaceErrorsKm(placed)
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
// from 0, near then mid then far, and the spares from drift.Extenders. The
// mid band is chosen by the information it is expected to give a visitor on
// a ring around the IP place, from the model's variances and each
// candidate's place error (placeErrorsKm, nil for the derived 50 km). Within
// each band the extenders with names pinned to the run's family come first,
// and plain-named ones fill only what they leave (K6). Pure, and the same for
// the same pool, place, family and model.
func lagDriftChoose(
	set *lagServingSet,
	place *lagDriftAddrPlace,
	used map[server.Id]bool,
	drift *LagDriftSettings,
	family int,
	driftModel lagdrift.Model,
	placeErrorsKm func(extenders []*lagServingExtender) []float64,
) []*lagDriftExtender {
	if place == nil || place.Point == nil {
		return lagDriftChooseUnplaced(set, used, drift, family)
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
			pinned:     lagDriftPinned(extender, family),
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
	// band's start; within it unused extenders first, pinned names first
	// within those, then repeats, then anything nearer than the rest
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
			if a.pinned != b.pinned {
				if a.pinned {
					return -1
				}
				return 1
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

	// mid: one at a time, the candidate that most shrinks the region for a
	// visitor who is really on a ring around the IP place (E2, T3's
	// hybrid), pinned names first, unused extenders first within them; the
	// nearer on a tie
	midBand := remaining(func(candidate *lagDriftExtender) bool {
		return lagDriftMidMinKm <= candidate.distanceKm && candidate.distanceKm < lagDriftMidMaxKm
	})
	lagDriftSetPlaceErrors(append(slices.Clone(chosen), midBand...), placeErrorsKm)
	ringScore := lagDriftRingScore(*place.Point, driftModel)
	midPicked := 0
	for _, group := range lagDriftPreferenceGroups(midBand) {
		for midPicked < midCount {
			var best *lagDriftExtender
			bestScore := math.Inf(1)
			for _, candidate := range group {
				if taken[candidate.extender.ExtenderId] {
					continue
				}
				if score := ringScore(append(slices.Clone(chosen), candidate)); score < bestScore {
					best = candidate
					bestScore = score
				}
			}
			if best == nil {
				break
			}
			take(best)
			midPicked += 1
		}
	}

	// far: spread by bearing, pinned names first, unused extenders first
	// within them, each group spread from the bearings picked before; the
	// slots a short mid band leaves pass to it
	band := remaining(func(candidate *lagDriftExtender) bool {
		return lagDriftMidMaxKm <= candidate.distanceKm && candidate.distanceKm <= lagDriftFarMaxKm
	})
	farWant := farCount + midCount - midPicked
	farPicks := []*lagDriftExtender{}
	bearings := []float64{}
	for _, group := range lagDriftPreferenceGroups(band) {
		for _, pick := range lagDriftSpread(group, farWant-len(farPicks), bearings) {
			farPicks = append(farPicks, pick)
			bearings = append(bearings, pick.bearingDeg)
		}
	}
	take(farPicks...)

	// a short band is made up from the nearest left, pinned names first,
	// unused first
	fill := lagDriftByPreference(remaining(func(*lagDriftExtender) bool { return true }))
	take(fill[:min(drift.Extenders-len(chosen), len(fill))]...)
	for position, choice := range chosen {
		choice.position = position
	}

	// spares, nearest first, pinned names first, unused first
	spares := lagDriftByPreference(remaining(func(*lagDriftExtender) bool { return true }))
	spares = spares[:min(drift.Spares, len(spares))]
	for i, spare := range spares {
		spare.position = drift.Extenders + i
	}
	return append(chosen, spares...)
}

// The candidates in four groups, each in the order given: pinned names
// unused, plain names unused, pinned names used, plain names used (K6, E2). A
// new path adds independent evidence (V7), and K4 checks a plain-named
// session's family, so a retest takes an unused plain name over a pinned one
// it has used.
func lagDriftPreferenceGroups(candidates []*lagDriftExtender) [4][]*lagDriftExtender {
	groups := [4][]*lagDriftExtender{}
	for _, candidate := range candidates {
		group := 0
		if candidate.used {
			group += 2
		}
		if !candidate.pinned {
			group += 1
		}
		groups[group] = append(groups[group], candidate)
	}
	return groups
}

// The candidates in order of preference, otherwise as they were: names
// pinned to the run's family first (K6), then extenders the window has not
// used (E2).
func lagDriftByPreference(candidates []*lagDriftExtender) []*lagDriftExtender {
	ordered := []*lagDriftExtender{}
	for _, group := range lagDriftPreferenceGroups(candidates) {
		ordered = append(ordered, group...)
	}
	return ordered
}

// Sets each candidate's place error (V3), the derived 50 km without a
// lookup.
func lagDriftSetPlaceErrors(candidates []*lagDriftExtender, placeErrorsKm func(extenders []*lagServingExtender) []float64) {
	if placeErrorsKm == nil {
		for _, candidate := range candidates {
			candidate.placeErrorKm = lagdrift.DerivedPlaceErrorKm
		}
		return
	}
	extenders := make([]*lagServingExtender, 0, len(candidates))
	for _, candidate := range candidates {
		extenders = append(extenders, candidate.extender)
	}
	for i, placeErrorKm := range placeErrorsKm(extenders) {
		candidates[i].placeErrorKm = placeErrorKm
	}
}

// The rings of plausible true places around the IP place that E2's mid band
// is chosen for, km, with lagDriftRingPlaces places on each (T3's hybrid).
var lagDriftRingKm = []float64{800, 2000}

const lagDriftRingPlaces = 12

// The measurement's own noise in the ring's variances, ms (lagdriftsim's).
const lagDriftRingMeasurementMs = 0.3

// A set of extenders' score for E2's mid band, lower better: the weighted
// log of the region's area the set is expected to give a visitor at each
// plausible place, the IP place at weight 1 and each ring place at 1/24. At a
// place, each extender adds to the information on the visitor's east, north
// and last mile b by its round trip's sensitivity to each, over the model's
// variance for the path: measurement, σ₀, κ times the distance, and the place
// error held (V3). The area is the determinant of the position's block of the
// information's inverse. Ported from lagdriftsim's informationRingScore
// (connect 984a349b, b3f8f6eb).
func lagDriftRingScore(ipPoint lagDriftPoint, driftModel lagdrift.Model) func(picked []*lagDriftExtender) float64 {
	type weightedPlace struct {
		at     lagdrift.Point
		weight float64
	}
	center := lagdrift.Point{Lat: ipPoint.latitude, Lon: ipPoint.longitude}
	places := []weightedPlace{{at: center, weight: 1}}
	ringWeight := 1 / float64(len(lagDriftRingKm)*lagDriftRingPlaces)
	for _, ringKm := range lagDriftRingKm {
		for i := range lagDriftRingPlaces {
			places = append(places, weightedPlace{
				at:     lagDriftDestination(center, float64(i)*360/lagDriftRingPlaces, ringKm),
				weight: ringWeight,
			})
		}
	}
	return func(picked []*lagDriftExtender) float64 {
		total := 0.0
		for _, place := range places {
			var information [3][3]float64
			for _, choice := range picked {
				at := lagdrift.Point{Lat: choice.extender.Place.Latitude, Lon: choice.extender.Place.Longitude}
				distanceKm := lagdrift.DistanceKm(place.at, at)
				bearing := lagdrift.BearingDeg(place.at, at) * math.Pi / 180
				// an extender at the place says little of the direction
				fade := min(1, distanceKm/10)
				row := [3]float64{
					-math.Sin(bearing) * lagdrift.MsPerKm * fade,
					-math.Cos(bearing) * lagdrift.MsPerKm * fade,
					1,
				}
				routeMs := driftModel.Kappa * lagdrift.MsPerKm * distanceKm
				placeMs := lagdrift.MsPerKm * lagdrift.HeldPlaceErrorKm(choice.placeErrorKm)
				variance := lagDriftRingMeasurementMs*lagDriftRingMeasurementMs +
					driftModel.Sigma0Ms*driftModel.Sigma0Ms +
					routeMs*routeMs +
					placeMs*placeMs
				for u := range 3 {
					for v := range 3 {
						information[u][v] += row[u] * row[v] / variance
					}
				}
			}
			total += place.weight * lagDriftPositionLogDet(information)
		}
		return total
	}
}

// The log determinant of the position block of a 3 × 3 information's
// inverse, or a large number when it is singular (lagdriftsim's).
func lagDriftPositionLogDet(a [3][3]float64) float64 {
	det := a[0][0]*(a[1][1]*a[2][2]-a[1][2]*a[2][1]) -
		a[0][1]*(a[1][0]*a[2][2]-a[1][2]*a[2][0]) +
		a[0][2]*(a[1][0]*a[2][1]-a[1][1]*a[2][0])
	if !(1e-30 < math.Abs(det)) {
		return 1e6
	}
	// the inverse's position block is the adjugate's over det
	c00 := (a[1][1]*a[2][2] - a[1][2]*a[2][1]) / det
	c01 := -(a[0][1]*a[2][2] - a[0][2]*a[2][1]) / det
	c10 := -(a[1][0]*a[2][2] - a[1][2]*a[2][0]) / det
	c11 := (a[0][0]*a[2][2] - a[0][2]*a[2][0]) / det
	block := c00*c11 - c01*c10
	if !(0 < block) {
		return 1e6
	}
	return math.Log(block)
}

// The point distanceKm from p along bearingDeg, on the sphere.
func lagDriftDestination(p lagdrift.Point, bearingDeg float64, distanceKm float64) lagdrift.Point {
	radians := math.Pi / 180
	lat1 := p.Lat * radians
	lon1 := p.Lon * radians
	theta := bearingDeg * radians
	delta := distanceKm / lagdrift.EarthRadiusKm
	sinLat2 := math.Sin(lat1)*math.Cos(delta) + math.Cos(lat1)*math.Sin(delta)*math.Cos(theta)
	lat2 := math.Asin(max(-1, min(1, sinLat2)))
	y := math.Sin(theta) * math.Sin(delta) * math.Cos(lat1)
	x := math.Cos(delta) - math.Sin(lat1)*sinLat2
	lon2 := (lon1 + math.Atan2(y, x)) / radians
	return lagdrift.Point{Lat: lat2 / radians, Lon: math.Mod(lon2+540, 360) - 180}
}

// The choice for an address placed nowhere: the sample's spread across
// countries (sampleLagServing), pinned names first, unused extenders first.
func lagDriftChooseUnplaced(set *lagServingSet, used map[server.Id]bool, drift *LagDriftSettings, family int) []*lagDriftExtender {
	sample := sampleLagServing(set, len(set.extenders))
	ordered := []*lagDriftExtender{}
	for _, extender := range sample {
		ordered = append(ordered, &lagDriftExtender{
			extender: extender,
			used:     used[extender.ExtenderId],
			pinned:   lagDriftPinned(extender, family),
		})
	}
	ordered = lagDriftByPreference(ordered)
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
