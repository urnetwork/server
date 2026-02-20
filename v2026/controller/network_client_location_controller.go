package controller

import (
	"context"
	// "encoding/json"
	// "net/http"
	"net/netip"

	// "encoding/base64"
	// "fmt"
	// "io"
	// "sync"
	// "time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

func GetLocationForIp(ctx context.Context, clientIp string) (*model.Location, *model.ConnectionLocationScores, error) {
	addr, err := netip.ParseAddr(clientIp)
	if err != nil {
		return nil, nil, err
	}

	ipInfo, err := server.GetIpInfo(addr)
	if err != nil {
		return nil, nil, err
	}

	location := &model.Location{
		LocationType:  model.LocationTypeCity,
		City:          ipInfo.City,
		Region:        ipInfo.Region,
		Country:       ipInfo.Country,
		CountryCode:   ipInfo.CountryCode,
		Continent:     ipInfo.Continent,
		ContinentCode: ipInfo.ContinentCode,
		Latitude:      ipInfo.Latitude,
		Longitude:     ipInfo.Longitude,
		Timezone:      ipInfo.Timezone,
	}
	location.LocationType, err = location.GuessLocationType()
	if err != nil {
		return nil, nil, err
	}

	connectionLocationScores := &model.ConnectionLocationScores{}
	if ipInfo.Hosting {
		connectionLocationScores.NetTypeHosting = 1
	}
	if ipInfo.Privacy {
		connectionLocationScores.NetTypePrivacy = 1
	}
	if ipInfo.Virtual {
		connectionLocationScores.NetTypeVirtual = 1
	}

	arinInfo, err := server.GetArinInfo(addr)
	if err == nil {
		// if the org ownership does not match the ip country,
		// we consider the use case of the ip to be virtual
		foreign := false
		for _, orgCountryCode := range arinInfo.OrgCountryCodes {
			if orgCountryCode != ipInfo.CountryCode {
				foreign = true
				break
			}
		}
		if foreign {
			connectionLocationScores.NetTypeForeign = 1
		}
	}

	return location, connectionLocationScores, nil
}
