package model

import (
	"context"
	"strings"

	"github.com/urnetwork/server"
)

// Where lag.dev places an extender (lag.dev/PLAN.md A4): its derived location
// (connect/GEOMAP.md §6), which its own pings placed it at, when it has one.
// The caller falls back to GeoLite2 City for the address.

// A place as the extender sample answers it. Names are English, the country
// code lower case, and a name the place does not reach is empty.
type LagPlace struct {
	City        string
	Region      string
	CountryCode string
	CountryName string
	Latitude    float64
	Longitude   float64
}

// The derived locations of the given extenders, by extender id, in one query:
// the derived coordinates and the names of the city, region and country the
// derived point maps to. An extender without a derived location is absent.
// This is GetDerivedLocation for many extenders at once, with the names
// joined.
func GetLagExtenderDerivedPlaces(ctx context.Context, extenderIds []server.Id) map[server.Id]*LagPlace {
	places := map[server.Id]*LagPlace{}
	if len(extenderIds) == 0 {
		return places
	}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				derived_location.node_id,
				derived_location.latitude,
				derived_location.longitude,
				COALESCE(city.location_name, ''),
				COALESCE(region.location_name, ''),
				COALESCE(country.location_name, ''),
				COALESCE(country.country_code, '')
			FROM derived_location
			LEFT JOIN location AS city ON city.location_id = derived_location.city_location_id
			LEFT JOIN location AS region ON region.location_id = derived_location.region_location_id
			LEFT JOIN location AS country ON country.location_id = derived_location.country_location_id
			WHERE
				derived_location.node_kind = $1 AND
				derived_location.node_id = ANY($2)
			`,
			DerivedLocationNodeKindExtender,
			extenderIds,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var extenderId server.Id
				place := &LagPlace{}
				server.Raise(result.Scan(
					&extenderId,
					&place.Latitude,
					&place.Longitude,
					&place.City,
					&place.Region,
					&place.CountryName,
					&place.CountryCode,
				))
				place.CountryCode = strings.ToLower(strings.TrimSpace(place.CountryCode))
				if place.CountryCode == "" {
					// a row whose country a merge removed places nothing the
					// sample can name; the address lookup does better
					continue
				}
				places[extenderId] = place
			}
		})
	})
	return places
}

// The latest measurement request of each of the given extenders, by extender
// id, as the certificate model keeps it (lag.dev/PLAN.md A4, A8, C9): its
// state is A8's answer, so `issued` is an extender serving its certificate,
// and a pending, refused or withdrawn request, an owner's opt-out included,
// is one that is not. An extender with no request is absent.
func GetLagExtenderMeasurements(ctx context.Context, extenderIds []server.Id) map[server.Id]*NetworkExtenderMeasurement {
	measurements := map[server.Id]*NetworkExtenderMeasurement{}
	if len(extenderIds) == 0 {
		return measurements
	}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT `+networkExtenderMeasurementColumns+`
			FROM network_extender_measurement
			WHERE extender_id = ANY($1)
			`,
			extenderIds,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				measurement := scanNetworkExtenderMeasurement(result)
				measurements[measurement.ExtenderId] = measurement
			}
		})
	})
	return measurements
}
