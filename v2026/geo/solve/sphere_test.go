// Tests of the solver's geometry: distances, moves along great circles and
// their inverse, and agreement with the place list's distances.

package solve

import (
	"math"
	mathrand "math/rand"
	"testing"

	"github.com/urnetwork/server/v2026/geo"
)

// Known distances: a degree of arc, the antimeridian, the pole, the antipodes.
func TestDistanceKm(t *testing.T) {
	for _, test := range []struct {
		name   string
		a, b   LatLon
		wantKm float64
	}{
		{name: "same point", a: LatLon{Latitude: 51.5, Longitude: -0.1}, b: LatLon{Latitude: 51.5, Longitude: -0.1}, wantKm: 0},
		// one degree of arc on the 6371 km sphere
		{name: "one degree of latitude", a: LatLon{Latitude: 0, Longitude: 0}, b: LatLon{Latitude: 1, Longitude: 0}, wantKm: 111.195},
		{name: "across the antimeridian", a: LatLon{Latitude: 0, Longitude: 179.5}, b: LatLon{Latitude: 0, Longitude: -179.5}, wantKm: 111.195},
		{name: "across the pole", a: LatLon{Latitude: 89, Longitude: 0}, b: LatLon{Latitude: 89, Longitude: 180}, wantKm: 222.390},
		{name: "antipodes", a: LatLon{Latitude: 0, Longitude: 0}, b: LatLon{Latitude: 0, Longitude: 180}, wantKm: math.Pi * EarthRadiusKm},
		{name: "london paris", a: LatLon{Latitude: 51.5074, Longitude: -0.1278}, b: LatLon{Latitude: 48.8566, Longitude: 2.3522}, wantKm: 343.556},
	} {
		if distanceKm := DistanceKm(test.a, test.b); 0.01 < math.Abs(distanceKm-test.wantKm) {
			t.Errorf("%s: DistanceKm = %.4f, want %.4f", test.name, distanceKm, test.wantKm)
		}
	}
}

// Move travels along a great circle, so the distance from the start is the
// offset's length, and OffsetBetween undoes it -- at every latitude, across the
// antimeridian, at a pole, and for offsets from a metre to a continent.
func TestMoveAndOffsetBetween(t *testing.T) {
	starts := []LatLon{
		{Latitude: 0, Longitude: 0},
		{Latitude: 47.5, Longitude: 7.5},
		{Latitude: -33.9, Longitude: 18.4},
		{Latitude: 0, Longitude: 179.99},
		{Latitude: 89.9, Longitude: 45},
		{Latitude: 90, Longitude: 0},
		{Latitude: -90, Longitude: 120},
	}
	for _, start := range starts {
		for _, lengthKm := range []float64{0, 1e-9, 0.001, 10, 500, 5000} {
			for _, bearing := range []float64{0, 0.3, math.Pi / 2, 2, math.Pi, 4.5} {
				offset := Offset{NorthKm: lengthKm * math.Cos(bearing), EastKm: lengthKm * math.Sin(bearing)}
				moved := Move(start, offset)
				if distanceKm := DistanceKm(start, moved); 1e-9*max(1, lengthKm) < math.Abs(distanceKm-lengthKm) {
					t.Fatalf("Move(%+v, %+v) is %v km away, want %v km", start, offset, distanceKm, lengthKm)
				}
				back := OffsetBetween(start, moved)
				if 1e-7*max(1, lengthKm) < math.Hypot(back.NorthKm-offset.NorthKm, back.EastKm-offset.EastKm) {
					t.Fatalf("OffsetBetween(%+v, Move(%+v)) = %+v, want %+v", start, offset, back, offset)
				}
			}
		}
	}

	// away from the poles north is north and east is east
	north := Move(LatLon{Latitude: 10, Longitude: 20}, Offset{NorthKm: 111.195})
	if 1e-3 < math.Abs(north.Latitude-11) || 1e-9 < math.Abs(north.Longitude-20) {
		t.Fatalf("111.195 km north of (10, 20) = %+v", north)
	}
	east := Move(LatLon{Latitude: 0, Longitude: 179.5}, Offset{EastKm: 111.195})
	if 1e-9 < math.Abs(east.Latitude) || 1e-3 < math.Abs(east.Longitude+179.5) {
		t.Fatalf("111.195 km east of (0, 179.5) = %+v", east)
	}

	// every direction reaches the antipode; whichever is taken, it is half the
	// earth away and Move goes back to it
	for _, pair := range [][2]LatLon{
		{{Latitude: 0, Longitude: 0}, {Latitude: 0, Longitude: 180}},
		{{Latitude: 90, Longitude: 0}, {Latitude: -90, Longitude: 0}},
		{{Latitude: 30, Longitude: 40}, {Latitude: -30, Longitude: -140}},
	} {
		offset := OffsetBetween(pair[0], pair[1])
		if 1e-6 < math.Abs(offset.LengthKm()-math.Pi*EarthRadiusKm) {
			t.Fatalf("OffsetBetween(%+v, %+v) = %+v, want half the circumference", pair[0], pair[1], offset)
		}
		if distanceKm := DistanceKm(Move(pair[0], offset), pair[1]); 1e-6 < distanceKm {
			t.Fatalf("Move(%+v, OffsetBetween the antipodes) is %v km from %+v", pair[0], distanceKm, pair[1])
		}
	}
}

// the place list's containment is the solver's
var _ Containment = (*geo.PlaceContainment)(nil)

// The solver measures with unit vectors and atan2, the place list with the
// haversine; they must agree, on the same sphere, for a derived point to map
// where the solver put it.
func TestDistanceAgreesWithThePlaceList(t *testing.T) {
	random := mathrand.New(mathrand.NewSource(3))
	for i := 0; i < 2000; i += 1 {
		a := LatLon{Latitude: random.Float64()*180 - 90, Longitude: random.Float64()*360 - 180}
		b := LatLon{Latitude: random.Float64()*180 - 90, Longitude: random.Float64()*360 - 180}
		if i%2 == 0 {
			// nearby points, where precision is hardest
			b = LatLon{Latitude: min(90, a.Latitude+random.Float64()*0.01), Longitude: a.Longitude + random.Float64()*0.01}
		}
		solveKm := DistanceKm(a, b)
		geoKm := geo.DistanceKm(a.Latitude, a.Longitude, b.Latitude, b.Longitude)
		if 1e-6 < math.Abs(solveKm-geoKm) {
			t.Fatalf("distance %+v %+v: solve %v km, place list %v km", a, b, solveKm, geoKm)
		}
	}
}
