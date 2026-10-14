package controller

import (
	"net/netip"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The GeoLite2 City place of an address (lag.dev/PLAN.md A4, A7), or nil when
// it has no record: where the extender sample places an extender without a
// derived location, and what a track's client fields say about the visitor.
// A deployment without the database places nothing this way, rather than
// failing the caller. Tests replace it.
var lagAddressPlace = func(ip netip.Addr) *model.LagPlace {
	var place *model.LagPlace
	server.HandleError(func() {
		ipInfo, err := server.GetIpInfo(ip)
		if err != nil || ipInfo.CountryCode == "" {
			return
		}
		place = &model.LagPlace{
			City:        ipInfo.City,
			Region:      ipInfo.Region,
			CountryCode: ipInfo.CountryCode,
			CountryName: ipInfo.Country,
			Latitude:    ipInfo.Latitude,
			Longitude:   ipInfo.Longitude,
		}
	})
	return place
}
