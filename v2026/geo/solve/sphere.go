// The geometry the solver works in: points on the sphere carried as unit
// vectors, a correction as an offset on the plane tangent at a genesis, and
// the moves between them along great circles.

package solve

import (
	"math"

	"github.com/urnetwork/server/v2026/geo"
)

// The mean earth radius the solver measures with: the one the place list and
// the server's other distance helpers use, so a derived distance and a
// reverse-geocoded one agree.
const EarthRadiusKm = geo.EarthRadiusKm

// No two points are farther apart than half a great circle, so no accuracy
// radius wider than this means anything.
const halfCircumferenceKm = math.Pi * EarthRadiusKm

// A point on the sphere, in degrees: the place list's own type, so the
// containment it implements takes the solver's points as they are.
type LatLon = geo.LatLon

// A correction on the tangent plane at a genesis: km north and km east. It is
// applied along the great circle it points along (Move), which is the inverse
// of the azimuthal equidistant projection centred on the genesis, so a node's
// surface distance from its genesis is exactly the offset's length and the
// genesis term of the objective is exactly quadratic in the offset.
type Offset struct {
	NorthKm float64
	EastKm  float64
}

// The surface distance the offset moves a point.
func (self Offset) LengthKm() float64 {
	return math.Hypot(self.NorthKm, self.EastKm)
}

// The great-circle distance between two points.
func DistanceKm(a LatLon, b LatLon) float64 {
	return surfaceKm(unitVector(a), unitVector(b))
}

// The point reached by travelling offset.LengthKm() from `from` along the
// great circle that leaves it in the offset's direction.
func Move(from LatLon, offset Offset) LatLon {
	frame := newTangentFrame(from)
	return frame.at(offset).latLon()
}

// The offset that moves `from` to `to`: Move(from, OffsetBetween(from, to)) is
// `to`. The derive job uses it to warm start a node from the position it
// derived last time. Every direction reaches the antipode of `from`, and for
// it the offset points north.
func OffsetBetween(from LatLon, to LatLon) Offset {
	frame := newTangentFrame(from)
	return frame.offsetTo(unitVector(to))
}

// A point as a unit vector. The surface distance between two of them, the
// move along a great circle and its inverse are then a few products each, with
// no special case at the antimeridian, and near the poles only the choice of
// the north/east basis depends on the longitude.
type vec3 struct {
	x float64
	y float64
	z float64
}

// The sum of two vectors.
func (self vec3) plus(other vec3) vec3 {
	return vec3{x: self.x + other.x, y: self.y + other.y, z: self.z + other.z}
}

// The vector times a scalar.
func (self vec3) scaled(s float64) vec3 {
	return vec3{x: s * self.x, y: s * self.y, z: s * self.z}
}

// The scalar product of two vectors.
func (self vec3) dot(other vec3) float64 {
	return self.x*other.x + self.y*other.y + self.z*other.z
}

// The vector product of two vectors.
func (self vec3) cross(other vec3) vec3 {
	return vec3{
		x: self.y*other.z - self.z*other.y,
		y: self.z*other.x - self.x*other.z,
		z: self.x*other.y - self.y*other.x,
	}
}

// The length of the vector.
func (self vec3) norm() float64 {
	return math.Sqrt(self.dot(self))
}

// A point's unit vector.
func unitVector(p LatLon) vec3 {
	const radiansPerDegree = math.Pi / 180
	sinLatitude, cosLatitude := math.Sincos(p.Latitude * radiansPerDegree)
	sinLongitude, cosLongitude := math.Sincos(p.Longitude * radiansPerDegree)
	return vec3{x: cosLatitude * cosLongitude, y: cosLatitude * sinLongitude, z: sinLatitude}
}

// The point a vector points at, in degrees.
func (self vec3) latLon() LatLon {
	const degreesPerRadian = 180 / math.Pi
	return LatLon{
		Latitude:  math.Atan2(self.z, math.Hypot(self.x, self.y)) * degreesPerRadian,
		Longitude: math.Atan2(self.y, self.x) * degreesPerRadian,
	}
}

// The great-circle distance between two unit vectors. The angle is taken as
// atan2(|a×b|, a·b), which keeps full precision at every separation: acos(a·b)
// loses it for nearby points and asin(|a×b|) for nearly antipodal ones. Both
// arguments scale together, so a vector a rounding off unit length does not
// bias the angle.
func surfaceKm(a vec3, b vec3) float64 {
	return EarthRadiusKm * math.Atan2(a.cross(b).norm(), a.dot(b))
}

// A genesis and the orthonormal north and east directions of the plane
// tangent to the sphere there.
type tangentFrame struct {
	origin vec3
	north  vec3
	east   vec3
}

// The frame at a point. At a pole north and east are undefined; the longitude
// still gives an orthonormal pair there, and the solver only needs a basis,
// not a compass.
func newTangentFrame(p LatLon) tangentFrame {
	const radiansPerDegree = math.Pi / 180
	sinLatitude, cosLatitude := math.Sincos(p.Latitude * radiansPerDegree)
	sinLongitude, cosLongitude := math.Sincos(p.Longitude * radiansPerDegree)
	return tangentFrame{
		origin: vec3{x: cosLatitude * cosLongitude, y: cosLatitude * sinLongitude, z: sinLatitude},
		north:  vec3{x: -sinLatitude * cosLongitude, y: -sinLatitude * sinLongitude, z: cosLatitude},
		east:   vec3{x: -sinLongitude, y: cosLongitude, z: 0},
	}
}

// The origin moved by an offset: travelling an angle θ = |offset|/R along the
// great circle through the origin with unit tangent t lands at
// cos(θ)·origin + sin(θ)·t, and t = (north·n + east·e)/|offset|.
func (self *tangentFrame) at(offset Offset) vec3 {
	lengthKm := offset.LengthKm()
	angle := lengthKm / EarthRadiusKm
	// sin(θ)/|offset|, which tends to 1/R as the offset vanishes. Below this
	// angle the next term of the series, θ⁴/120, is under the precision of a
	// float64, so the two-term series is exact and a zero offset needs no
	// special case.
	const seriesAngle = 1e-4
	var tangentScale float64
	if angle < seriesAngle {
		tangentScale = (1 - angle*angle/6) / EarthRadiusKm
	} else {
		tangentScale = math.Sin(angle) / lengthKm
	}
	tangent := self.north.scaled(offset.NorthKm).plus(self.east.scaled(offset.EastKm))
	return self.origin.scaled(math.Cos(angle)).plus(tangent.scaled(tangentScale))
}

// The offset that moves the origin to p, inverting at: p's components along
// north and east point along the great circle from the origin to p, and with
// its component along the origin they give the angle travelled. Projecting
// onto the basis, rather than subtracting the radial part from p, keeps the
// direction a unit vector even when p is so near the antipode that its
// tangential part is rounding.
func (self *tangentFrame) offsetTo(p vec3) Offset {
	along := self.origin.dot(p)
	north := self.north.dot(p)
	east := self.east.dot(p)
	sinAngle := math.Hypot(north, east)
	if sinAngle == 0 {
		if along < 0 {
			return Offset{NorthKm: halfCircumferenceKm}
		}
		return Offset{}
	}
	lengthKm := EarthRadiusKm * math.Atan2(sinAngle, along)
	return Offset{
		NorthKm: lengthKm * north / sinAngle,
		EastKm:  lengthKm * east / sinAngle,
	}
}
