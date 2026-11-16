package controller

import (
	"net/netip"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// A visitor's city-level place, as a track keeps it (A5, amendment 9).
type lagClientPlaceFields struct {
	CountryCode string
	Region      string
	City        string
	// the city's own point; nil unless the place names a city that the
	// place list holds
	Latitude  *float64
	Longitude *float64
}

// The city-level place of a visitor's address (A5): the country, region and
// city of its GeoLite2 City record, and the coordinates of the city itself,
// one point per city whatever block of it the address is in (lagCityPoint).
// GeoLite2 places a network at its postal area's point, so the record's own
// coordinates would keep a point finer than the city beside the network and
// the time (amendment 9); they are never kept. A place that names no city, or
// a city the place list does not hold, keeps no coordinates. nil when the
// lookup cannot answer. An extender is placed by model.GetLagExtenderPlaces,
// never by this. Unlike the extenders' places this is never replaced: tests
// install a database and a place list instead, so they run what production
// runs.
func lagClientPlace(addr netip.Addr) *lagClientPlaceFields {
	var place *lagClientPlaceFields
	server.HandleError(func() {
		ipInfo, err := server.GetIpInfo(addr)
		if err != nil || ipInfo.CountryCode == "" {
			return
		}
		fields := &lagClientPlaceFields{
			CountryCode: ipInfo.CountryCode,
			Region:      ipInfo.Region,
			City:        ipInfo.City,
		}
		if ipInfo.City != "" {
			if latitude, longitude, ok := lagCityPoint(ipInfo.CityGeonameId); ok {
				fields.Latitude = &latitude
				fields.Longitude = &longitude
			}
		}
		place = fields
	})
	return place
}

// A city's own point, by its GeoNames id: its coordinate in the canonical
// place list (model.CurrentPlaces, connect/GEOMAP.md §4), which is exported
// from the same GeoLite2 City build that answers the lookup and deployed
// beside it. That is the coordinate most of the city's networks carry, the
// city's own location rather than a postal area's, and it is the same for
// every address in the city. False when the id is 0, the deployment has no
// place list, or the list has no such city. The list is loaded on its first
// use in the process, shared with location matching.
func lagCityPoint(cityGeonameId uint32) (latitude float64, longitude float64, ok bool) {
	if cityGeonameId == 0 {
		return 0, 0, false
	}
	places := model.CurrentPlaces()
	if places == nil {
		return 0, 0, false
	}
	place := places.CityByGeonameId(cityGeonameId)
	if place == nil {
		return 0, 0, false
	}
	return place.Latitude, place.Longitude, true
}
