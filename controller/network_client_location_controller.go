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
		LocationType:     model.LocationTypeCity,
		City:             ipInfo.City,
		Region:           ipInfo.Region,
		Country:          ipInfo.Country,
		CountryCode:      ipInfo.CountryCode,
		Continent:        ipInfo.Continent,
		ContinentCode:    ipInfo.ContinentCode,
		Latitude:         ipInfo.Latitude,
		Longitude:        ipInfo.Longitude,
		Timezone:         ipInfo.Timezone,
		CityGeonameId:    ipInfo.CityGeonameId,
		RegionGeonameId:  ipInfo.RegionGeonameId,
		CountryGeonameId: ipInfo.CountryGeonameId,
	}
	location.LocationType, err = location.GuessLocationType()
	if err != nil {
		return nil, nil, err
	}

	// GeoLite2 has no ip-quality verdicts, so this path leaves NetTypeHosting,
	// NetTypePrivacy and NetTypeVirtual at 0 -- unknown, not clean. The egress
	// probe is the only source of hosting and privacy: SetConnectionLocation
	// (network_client_controller.go) applies a fresh probe's flags to these
	// scores whichever location it stores, this one included, so only a
	// connection without a fresh probe keeps the zeros. Nothing sets
	// NetTypeVirtual.
	connectionLocationScores := &model.ConnectionLocationScores{
		NetTypeForeign: arinForeignScore(addr, ipInfo.CountryCode),
		AccuracyKm:     genesisAccuracyKm(ipInfo),
	}

	return location, connectionLocationScores, nil
}

// The lookup's accuracy radius as the location rows store it
// (connect/GEOMAP.md §5.1): GeoLite2's radius in km around the coordinates, or
// an override's when it names one. Nil when the lookup has none -- an override
// without a radius, or a record GeoLite2 gives no radius -- since a zero would
// read as a perfectly placed address rather than an unknown one.
func genesisAccuracyKm(ipInfo *server.IpInfo) *float32 {
	if ipInfo == nil || ipInfo.AccuracyRadiusKm <= 0 {
		return nil
	}
	accuracyKm := float32(ipInfo.AccuracyRadiusKm)
	return &accuracyKm
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
