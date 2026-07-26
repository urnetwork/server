package model

import (
	"context"
	"strings"
	"time"

	"github.com/urnetwork/server"
)

// ProviderEgressLocationMaxAge bounds how long a probed egress location is
// trusted. Past this, the location is ignored and the caller falls back to the
// mmdb lookup on the observed control ip.
const ProviderEgressLocationMaxAge = 7 * 24 * time.Hour

// ProviderEgressLocation is a provider location learned by probing the
// provider's own egress, rather than by looking up its control-connection ip.
type ProviderEgressLocation struct {
	ClientId      server.Id
	LocationId    server.Id
	CountryCode   string
	ASN           int
	Org           string
	Hosting       bool
	Proxy         bool
	Mobile        bool
	CityConfident bool
	ObservedAt    time.Time
	UpdateTime    time.Time
}

// SetProviderEgressLocation upserts the probed location for a provider. The
// upsert is monotonic in observed_at: a replayed or out-of-order submission
// older than what is already stored is silently dropped rather than
// clobbering a newer probe result.
func SetProviderEgressLocation(ctx context.Context, e *ProviderEgressLocation) {
	// country codes are stored/compared lowercased (see CreateLocation in
	// network_client_location_model.go); the geolocation APIs that feed this
	// return uppercase codes (e.g. "US"), so normalize before writing.
	countryCode := strings.ToLower(e.CountryCode)

	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO provider_egress_location (
				client_id,
				location_id,
				country_code,
				asn,
				org,
				hosting,
				proxy,
				mobile,
				city_confident,
				observed_at,
				update_time
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
			ON CONFLICT (client_id) DO UPDATE
			SET
				location_id = $2,
				country_code = $3,
				asn = $4,
				org = $5,
				hosting = $6,
				proxy = $7,
				mobile = $8,
				city_confident = $9,
				observed_at = $10,
				update_time = $11
			WHERE provider_egress_location.observed_at < EXCLUDED.observed_at
			`,
			e.ClientId,
			e.LocationId,
			countryCode,
			e.ASN,
			e.Org,
			e.Hosting,
			e.Proxy,
			e.Mobile,
			e.CityConfident,
			e.ObservedAt.UTC(),
			server.NowUtc(),
		))
	})
}

// GetProviderEgressLocation returns the stored location for a provider, or nil.
func GetProviderEgressLocation(ctx context.Context, clientId server.Id) *ProviderEgressLocation {
	var e *ProviderEgressLocation
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				client_id,
				location_id,
				country_code,
				asn,
				org,
				hosting,
				proxy,
				mobile,
				city_confident,
				observed_at,
				update_time
			FROM provider_egress_location
			WHERE client_id = $1
			`,
			clientId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				e = &ProviderEgressLocation{}
				server.Raise(result.Scan(
					&e.ClientId,
					&e.LocationId,
					&e.CountryCode,
					&e.ASN,
					&e.Org,
					&e.Hosting,
					&e.Proxy,
					&e.Mobile,
					&e.CityConfident,
					&e.ObservedAt,
					&e.UpdateTime,
				))
			}
		})
	})
	return e
}

// GetFreshProviderEgressLocation is GetProviderEgressLocation, filtered to
// entries probed within maxAge. The cutoff is computed in Go and bound as a
// parameter: observed_at is a naive timestamp holding utc, and comparing it
// against sql now() would cast through the session timezone.
func GetFreshProviderEgressLocation(
	ctx context.Context,
	clientId server.Id,
	maxAge time.Duration,
) *ProviderEgressLocation {
	e := GetProviderEgressLocation(ctx, clientId)
	if e == nil {
		return nil
	}
	if e.ObservedAt.Before(server.NowUtc().Add(-maxAge)) {
		return nil
	}
	return e
}

// GetFreshProviderEgressLocationForConnection resolves the probed provider
// egress location for a connection in a single query, joining
// network_client_connection to provider_egress_location on client_id. This
// exists for the connect-announce hot path (SetConnectionLocation), which
// previously spent two round trips per connection -- resolving the client id
// for the connection, then fetching its fresh egress location -- before ever
// reaching the mmdb fallback; collapsing to one query matters on a path that
// runs for every connection and inside a retry loop.
//
// As with GetFreshProviderEgressLocation, the maxAge cutoff is computed in Go
// and compared in Go: observed_at is a naive timestamp holding utc, and
// comparing it against sql now() would cast through the session timezone.
func GetFreshProviderEgressLocationForConnection(
	ctx context.Context,
	connectionId server.Id,
	maxAge time.Duration,
) *ProviderEgressLocation {
	var e *ProviderEgressLocation
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				pel.client_id,
				pel.location_id,
				pel.country_code,
				pel.asn,
				pel.org,
				pel.hosting,
				pel.proxy,
				pel.mobile,
				pel.city_confident,
				pel.observed_at,
				pel.update_time
			FROM network_client_connection ncc
			INNER JOIN provider_egress_location pel ON pel.client_id = ncc.client_id
			WHERE ncc.connection_id = $1
			`,
			connectionId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				e = &ProviderEgressLocation{}
				server.Raise(result.Scan(
					&e.ClientId,
					&e.LocationId,
					&e.CountryCode,
					&e.ASN,
					&e.Org,
					&e.Hosting,
					&e.Proxy,
					&e.Mobile,
					&e.CityConfident,
					&e.ObservedAt,
					&e.UpdateTime,
				))
			}
		})
	})
	if e == nil {
		return nil
	}
	if e.ObservedAt.Before(server.NowUtc().Add(-maxAge)) {
		return nil
	}
	return e
}

// GetLocation returns the canonical location row, or nil.
//
// Note: the location table also has a location_name column, but it holds the
// name for whichever granularity that specific row represents (e.g. a city
// row's own name), not a single name field on the Location struct. Location
// instead splits City/Region/Country by joining sibling rows (see
// IndexSearchLocationsInTx in network_client_location_model.go). This helper
// only needs to resolve identity/type, so it selects the columns that map
// directly onto Location's fields and leaves City/Region/Country empty.
func GetLocation(ctx context.Context, locationId server.Id) *Location {
	var loc *Location
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT location_id, location_type, city_location_id, region_location_id, country_location_id, country_code
			FROM location
			WHERE location_id = $1
			`,
			locationId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				loc = &Location{}
				// city_location_id/region_location_id are only set once the
				// row's hierarchy reaches that granularity (e.g. a country
				// row has both NULL); server.Id.Scan errors on a nil source,
				// so scan through nullable pointers as in
				// IndexSearchLocationsInTx (network_client_location_model.go).
				var cityLocationId *server.Id
				var regionLocationId *server.Id
				var countryLocationId *server.Id
				server.Raise(result.Scan(
					&loc.LocationId,
					&loc.LocationType,
					&cityLocationId,
					&regionLocationId,
					&countryLocationId,
					&loc.CountryCode,
				))
				if cityLocationId != nil {
					loc.CityLocationId = *cityLocationId
				}
				if regionLocationId != nil {
					loc.RegionLocationId = *regionLocationId
				}
				if countryLocationId != nil {
					loc.CountryLocationId = *countryLocationId
				}
			}
		})
	})
	return loc
}

// GetProviderEgressLocationDue returns the client ids of providers whose
// egress location is due for a probe: their newest probe is older than
// minObservedAt, or they have never been probed at all. Oldest first, so the
// longest-unprobed are handed out first, capped at limit.
//
// This is the durable replacement for the prober's in-memory ttl cache: the
// schedule lives in the database, so a prober restart resumes where it left
// off instead of re-probing everything.
//
// Two things about the shape of this query matter.
//
// First, candidates are sourced from the live provider population
// (network_client_location_reliability, connected + valid) and the egress row
// is LEFT JOINed on. The dominant case by far is a provider that has *never*
// been probed and therefore has no provider_egress_location row at all;
// selecting from provider_egress_location would return exactly the providers
// that least need probing and none of the ones that most do.
//
// Second, only providers holding a Public provide key are returned. Probing
// tunnels through the provider itself, which means opening a contract from
// outside the provider's own network -- something a provider without a Public
// key refuses. Offering one to the prober would burn a probe slot on a
// guaranteed failure. This is the same filter UpdateClientLocations and
// UpdateClientScores apply (network_client_location_model.go).
//
// The cutoff is computed by the caller in Go and bound as a parameter:
// observed_at is a naive `timestamp` holding utc, and comparing it against sql
// now() would cast through the session timezone and silently skip a window.
func GetProviderEgressLocationDue(
	ctx context.Context,
	minObservedAt time.Time,
	limit int,
) []server.Id {
	clientIds := []server.Id{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				network_client_location_reliability.client_id
			FROM network_client_location_reliability

			LEFT JOIN provider_egress_location ON
				provider_egress_location.client_id = network_client_location_reliability.client_id

			WHERE
				network_client_location_reliability.connected = true AND
				network_client_location_reliability.valid = true AND
				EXISTS (
					SELECT 1 FROM provide_key
					WHERE
						provide_key.client_id = network_client_location_reliability.client_id AND
						provide_key.provide_mode = $1
				) AND
				(
					provider_egress_location.observed_at IS NULL OR
					provider_egress_location.observed_at < $2
				)

			-- never-probed sorts ahead of merely stale: a missing observed_at
			-- is infinitely old
			ORDER BY provider_egress_location.observed_at ASC NULLS FIRST
			LIMIT $3
			`,
			ProvideModePublic,
			minObservedAt.UTC(),
			limit,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var clientId server.Id
				server.Raise(result.Scan(&clientId))
				clientIds = append(clientIds, clientId)
			}
		})
	})
	return clientIds
}

// RemoveExpiredProviderEgressLocations drops entries probed before
// minObservedAt.
func RemoveExpiredProviderEgressLocations(ctx context.Context, minObservedAt time.Time) {
	server.MaintenanceTx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`DELETE FROM provider_egress_location WHERE observed_at < $1`,
			minObservedAt.UTC(),
		))
	})
}
