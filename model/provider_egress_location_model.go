package model

import (
	"context"
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

// SetProviderEgressLocation upserts the probed location for a provider.
func SetProviderEgressLocation(ctx context.Context, e *ProviderEgressLocation) {
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
			`,
			e.ClientId,
			e.LocationId,
			e.CountryCode,
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
