package myinfo

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

type Coordinates struct {
	Latitude  float64 `json:"lat"`
	Longitude float64 `json:"lon"`
}

func ParseCoordinates(ipInfoString string) (Coordinates, error) {
	parts := strings.Split(ipInfoString, ",")
	if len(parts) != 2 {
		return Coordinates{}, fmt.Errorf("invalid coordinates string: %q", ipInfoString)
	}

	lat, err := strconv.ParseFloat(parts[0], 64)
	if err != nil {
		return Coordinates{}, fmt.Errorf("failed to parse latitude %q: %w", parts[0], err)
	}

	lon, err := strconv.ParseFloat(parts[1], 64)
	if err != nil {
		return Coordinates{}, fmt.Errorf("failed to parse longitude %q: %w", parts[1], err)
	}

	return Coordinates{
		Latitude:  lat,
		Longitude: lon,
	}, nil

}

const (
	EarthRadiusMeters = 6371000.0
	SpeedOfLight      = 299792458.0
	EarthRadius
)

var EarthRadiusInLightSeconds = float64(EarthRadiusMeters) / float64(SpeedOfLight)

func (c Coordinates) CalculateDistanceInLightSecondsOnEarthSurface(other Coordinates) float64 {
	latA := degreesToRadians(c.Latitude)
	lonA := degreesToRadians(c.Longitude)
	latB := degreesToRadians(other.Latitude)
	lonB := degreesToRadians(other.Longitude)

	dLat := latB - latA
	dLon := lonB - lonA

	a := math.Sin(dLat/2)*math.Sin(dLat/2) + math.Cos(latA)*math.Cos(latB)*math.Sin(dLon/2)*math.Sin(dLon/2)
	d := 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))

	return EarthRadiusInLightSeconds * d
}

func degreesToRadians(degrees float64) float64 {
	return degrees * (math.Pi / 180.0)
}
