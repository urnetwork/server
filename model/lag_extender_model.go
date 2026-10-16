package model

import (
	"context"
	"strings"
	"time"

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

// An extender's latest measurement request as the lag.dev sample reads it
// (lag.dev/PLAN.md A4, A8, C9), with whether the extender holds a
// certificate for it.
type LagExtenderMeasurement struct {
	*NetworkExtenderMeasurement
	// whether a live certificate for the request's names and key has its name
	// published (C8) and has been handed to the extender by A8 (deliver_time),
	// which installs it only when it reads it there
	Delivered bool
}

// Whether the extender serves its measurement name now, as far as its
// request says: the request is answered `issued`, and its certificate is
// published and delivered. A pending, refused or withdrawn request, an
// owner's opt-out included, is one that does not.
func (self *LagExtenderMeasurement) Serving() bool {
	return self.State == NetworkExtenderMeasurementStateIssued && self.Delivered
}

// The latest measurement request of each of the given extenders, by extender
// id, as the certificate model keeps it, with whether a certificate for it is
// delivered at `now`: its state is A8's answer, and an issued request serves
// only once A8 has handed the extender its certificate. Until then, up to
// half an hour after its name is published, the name answers without it. An
// extender with no request is absent.
func GetLagExtenderMeasurements(ctx context.Context, extenderIds []server.Id, now time.Time) map[server.Id]*LagExtenderMeasurement {
	measurements := map[server.Id]*LagExtenderMeasurement{}
	if len(extenderIds) == 0 {
		return measurements
	}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				`+networkExtenderMeasurementColumns+`,
				EXISTS (
					SELECT 1
					FROM network_extender_measurement_cert
					WHERE
						network_extender_measurement_cert.extender_id = request.extender_id AND
						network_extender_measurement_cert.names = request.names AND
						network_extender_measurement_cert.public_key = request.public_key AND
						network_extender_measurement_cert.withdraw_time IS NULL AND
						network_extender_measurement_cert.publish_time IS NOT NULL AND
						network_extender_measurement_cert.deliver_time IS NOT NULL AND
						$2 < network_extender_measurement_cert.not_after
				)
			FROM network_extender_measurement AS request
			WHERE request.extender_id = ANY($1)
			`,
			extenderIds,
			now.UTC(),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				measurement := &LagExtenderMeasurement{}
				measurement.NetworkExtenderMeasurement = scanNetworkExtenderMeasurement(result, &measurement.Delivered)
				measurements[measurement.ExtenderId] = measurement
			}
		})
	})
	return measurements
}
