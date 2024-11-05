package myinfo_test

import (
	_ "embed"
	"encoding/json"
	"testing"

	"bringyour.com/service/api/myipinfo/myinfo"
	"github.com/ipinfo/go/v2/ipinfo"
	"github.com/stretchr/testify/require"
)

//go:embed sample_ipinfo_response.json
var sampleIPInfoResponse []byte

func TestFromIPInfo(t *testing.T) {

	ip := &ipinfo.Core{}
	err := json.Unmarshal(sampleIPInfoResponse, ip)
	require.NoError(t, err)

	mi, err := myinfo.FromIPInfo(ip)
	require.NoError(t, err)

	expected := myinfo.MyInfo{
		IP: "81.6.15.76",
		Location: &myinfo.Location{
			Coordinates: &myinfo.Coordinates{
				Latitude:  47.3667,
				Longitude: 8.55,
			},
			City:   "Zürich",
			Region: "Zurich",
			Country: &myinfo.Country{
				Code:    "CH",
				Name:    "Switzerland",
				FlagURL: "https://cdn.ipinfo.io/static/images/countries-flags/CH.svg",
			},
			Continent: &myinfo.Continent{
				Code: "EU",
				Name: "Europe",
			},
			Timezone: "Europe/Zurich",
		},
		Privacy: &myinfo.Privacy{
			VPN:     false,
			Proxy:   false,
			Tor:     false,
			Relay:   false,
			Hosting: true,
			Service: "",
		},
	}

	require.Equal(t, expected, mi)

}
