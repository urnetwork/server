package myinfo

import "github.com/ipinfo/go/v2/ipinfo"

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

func FromIPInfo(ipInfo *ipinfo.Core) (MyInfo, error) {

	if ipInfo.Location == "" {
		return MyInfo{
			IP: ipInfo.IP.String(),
		}, nil
	}

	coords, err := ParseCoordinates(ipInfo.Location)
	if err != nil {
		return MyInfo{}, err
	}

	var privacy *Privacy

	if ipInfo.Privacy != nil {
		privacy = &Privacy{
			VPN:     ipInfo.Privacy.VPN,
			Proxy:   ipInfo.Privacy.Proxy,
			Tor:     ipInfo.Privacy.Tor,
			Relay:   ipInfo.Privacy.Relay,
			Hosting: ipInfo.Privacy.Hosting,
			Service: ipInfo.Privacy.Service,
		}
	}

	return MyInfo{
		IP: ipInfo.IP.String(),
		Location: &Location{
			Coordinates: &coords,
			City:        ipInfo.City,
			Region:      ipInfo.Region,
			Country: &Country{
				Code:    ipInfo.Country,
				Name:    ipInfo.CountryName,
				FlagURL: ipInfo.CountryFlagURL,
			},
			Continent: &Continent{
				Code: ipInfo.Continent.Code,
				Name: ipInfo.Continent.Name,
			},
			Timezone: ipInfo.Timezone,
		},
		Privacy: privacy,
	}, nil
}
