package controller

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

type MyIpInfoResult struct {
	Info               MyInfo `json:"info"`
	ConnectedToNetwork bool   `json:"connected_to_network"`
}

func GetMyIpInfo(session *session.ClientSession) (*MyIpInfoResult, error) {
	clientIp, _, err := session.ClientIpPort()
	if err != nil {
		return nil, err
	}

	location, connectionLocationScores, err := GetLocationForIp(session.Ctx, clientIp)
	if err != nil {
		return nil, err
	}

	myInfo, err := NewMyInfo(clientIp, location, connectionLocationScores)
	if err != nil {
		return nil, err
	}

	return &MyIpInfoResult{
		Info:               myInfo,
		ConnectedToNetwork: model.IsIpConnectedToNetwork(session.Ctx, clientIp),
	}, nil
}

type MyInfo struct {
	IP       string      `json:"ip"`
	Location *IpLocation `json:"location,omitempty"`
	Privacy  *IpPrivacy  `json:"privacy,omitempty"`
}

type IpLocation struct {
	Coordinates *Coordinates `json:"coordinates,omitempty"`
	City        string       `json:"city,omitempty"`
	Region      string       `json:"region,omitempty"`
	Country     *IpCountry   `json:"country,omitempty"`
	Continent   *IpContinent `json:"continent,omitempty"`
	Timezone    string       `json:"timezone,omitempty"`
}

type IpPrivacy struct {
	VPN     bool   `json:"vpn"`
	Proxy   bool   `json:"proxy"`
	Tor     bool   `json:"tor"`
	Relay   bool   `json:"relay"`
	Hosting bool   `json:"hosting"`
	Service string `json:"service"`
}

type IpContinent struct {
	Code string `json:"code,omitempty"`
	Name string `json:"name,omitempty"`
}

type IpCountry struct {
	Code    string `json:"code,omitempty"`
	Name    string `json:"name,omitempty"`
	FlagURL string `json:"flag_url,omitempty"`
}

func NewMyInfo(clientIp string, location *model.Location, connectionLocationScores *model.ConnectionLocationScores) (MyInfo, error) {
	privacy := &IpPrivacy{
		VPN:     0 < connectionLocationScores.NetTypePrivacy,
		Hosting: 0 < connectionLocationScores.NetTypeHosting,
	}

	return MyInfo{
		IP: clientIp,
		Location: &IpLocation{
			Coordinates: &Coordinates{
				Longitude: location.Longitude,
				Latitude:  location.Latitude,
			},
			City:   location.City,
			Region: location.Region,
			Country: &IpCountry{
				Code: location.CountryCode,
				Name: location.Country,
			},
			Continent: &IpContinent{
				Code: location.ContinentCode,
				Name: location.Continent,
			},
			Timezone: location.Timezone,
		},
		Privacy: privacy,
	}, nil
}

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
