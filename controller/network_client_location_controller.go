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

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
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

	connectionLocationScores.NetTypeForeign = arinForeignScore(addr, ipInfo.CountryCode)

	return location, connectionLocationScores, nil
}

// arinForeignScore cross-checks the ARIN org registration country for addr
// against countryCode. Both the ordinary path (GetLocationForIp) and the
// provider-egress path (SetConnectionLocation, in network_client_controller.go) pass the
// mmdb-resolved country of addr here, not the probed egress country: this is
// deliberate parity, so a probed and an unprobed provider on the same
// control ip are scored on the same basis and probing does not, by itself,
// change this ranking signal. If the org's registered country differs, the
// use case is considered foreign (VPN/proxy-like), matching the heuristic
// previously inlined in GetLocationForIp.
//
// If the ARIN lookup fails, this returns 0 without error: it must never fail
// or panic a caller on the connect-announce hot path over a missing/failed
// foreign check.
func arinForeignScore(addr netip.Addr, countryCode string) int {
	arinInfo, err := server.GetArinInfo(addr)
	if err != nil {
		return 0
	}
	// if the org ownership does not match the claimed country,
	// we consider the use case of the ip to be foreign
	for _, orgCountryCode := range arinInfo.OrgCountryCodes {
		if orgCountryCode != countryCode {
			return 1
		}
	}
	return 0
}
