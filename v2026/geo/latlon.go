// A point on the sphere, the currency between the place list, its
// containment hinges and the location solver of geo/solve (which imports this
// package, never the other way round).

package geo

// A point on the sphere, in degrees.
type LatLon struct {
	Latitude  float64
	Longitude float64
}
