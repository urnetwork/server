package myinfo

// import "github.com/ipinfo/go/v2/ipinfo"

import (
	"github.com/urnetwork/server/model"
)

type Location struct {
	Coordinates *Coordinates `json:"coordinates,omitempty"`
	City        string       `json:"city,omitempty"`
	Region      string       `json:"region,omitempty"`
	Country     *Country     `json:"country,omitempty"`
	Continent   *Continent   `json:"continent,omitempty"`
	Timezone    string       `json:"timezone,omitempty"`
}

type Privacy struct {
	VPN     bool   `json:"vpn"`
	Proxy   bool   `json:"proxy"`
	Tor     bool   `json:"tor"`
	Relay   bool   `json:"relay"`
	Hosting bool   `json:"hosting"`
	Service string `json:"service"`
}

type Continent struct {
	Code string `json:"code,omitempty"`
	Name string `json:"name,omitempty"`
}

type Country struct {
	Code    string `json:"code,omitempty"`
	Name    string `json:"name,omitempty"`
	FlagURL string `json:"flag_url,omitempty"`
}

type MyInfo struct {
	IP       string    `json:"ip"`
	Location *Location `json:"location,omitempty"`
	Privacy  *Privacy  `json:"privacy,omitempty"`
}

func NewMyInfo(clientIp string, location *model.Location, connectionLocationScores *model.ConnectionLocationScores) (MyInfo, error) {

	// if ipInfo.Location == "" {
	// 	return MyInfo{
	// 		IP: ipInfo.IP.String(),
	// 	}, nil
	// }

	// coords, err := ParseCoordinates(ipInfo.Location)
	// if err != nil {
	// 	return MyInfo{}, err
	// }

	privacy := &Privacy{
		VPN:     0 < connectionLocationScores.NetTypePrivacy,
		Hosting: 0 < connectionLocationScores.NetTypeHosting,
	}

	return MyInfo{
		IP: clientIp,
		Location: &Location{
			Coordinates: &Coordinates{
				Longitude: location.Longitude,
				Latitude:  location.Latitude,
			},
			City:   location.City,
			Region: location.Region,
			Country: &Country{
				Code: location.CountryCode,
				Name: location.Country,
			},
			Continent: &Continent{
				Code: location.ContinentCode,
				Name: location.Continent,
			},
			Timezone: location.Timezone,
		},
		Privacy: privacy,
	}, nil
}
