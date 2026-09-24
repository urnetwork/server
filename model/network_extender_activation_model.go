package model

import (
	"context"
	"time"

	"github.com/urnetwork/server"
)

// The activation history of an extender (connect/EXTENDER.md M1): one row per
// activation, carrying where the activating address resolved to and the
// privacy-preserving hash of that address, exactly what a provider's
// connection row and its location row carry together.
//
// network_extender holds only the latest activation's location. This table is
// the history, so a latency attestation (connect/DESIGNNOTES4.md §3) can later
// be placed against where the extender was when it was measured, and so an
// extender that moves is visible as a sequence rather than an overwrite.

type NetworkExtenderActivationRecord struct {
	ActivationId server.Id
	ExtenderId   server.Id
	ActivateTime time.Time
	IpVersion    int
	// nil when the activating address could not be read
	ClientAddressHash []byte
	// empty when the address resolved to no country
	CountryCode       string
	LocationId        *server.Id
	CityLocationId    *server.Id
	RegionLocationId  *server.Id
	CountryLocationId *server.Id
	// the lookup's accuracy radius in km (connect/GEOMAP.md §5.1); nil when
	// it gave none, and on rows written before the column
	AccuracyKm *float32
}

// Writes the history row of one activation inside the activation transaction.
func insertNetworkExtenderActivationInTx(
	ctx context.Context,
	tx server.PgTx,
	extenderId server.Id,
	activation *NetworkExtenderActivation,
	activateTime time.Time,
) {
	server.RaisePgResult(tx.Exec(
		ctx,
		`
		INSERT INTO network_extender_activation (
			activation_id,
			extender_id,
			activate_time,
			ip_version,
			client_address_hash,
			country_code,
			location_id,
			city_location_id,
			region_location_id,
			country_location_id,
			accuracy_km
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		`,
		server.NewId(),
		extenderId,
		activateTime.UTC(),
		activation.IpVersion,
		activation.ClientAddressHash,
		activation.CountryCode,
		activation.LocationId,
		activation.CityLocationId,
		activation.RegionLocationId,
		activation.CountryLocationId,
		activation.AccuracyKm,
	))
}

// GetNetworkExtenderActivations reads the activation history of one extender
// at or after `minActivateTime`, oldest first.
func GetNetworkExtenderActivations(
	ctx context.Context,
	extenderId server.Id,
	minActivateTime time.Time,
) []*NetworkExtenderActivationRecord {
	activations := []*NetworkExtenderActivationRecord{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				activation_id,
				activate_time,
				ip_version,
				client_address_hash,
				country_code,
				location_id,
				city_location_id,
				region_location_id,
				country_location_id,
				accuracy_km
			FROM network_extender_activation
			WHERE extender_id = $1 AND $2 <= activate_time
			ORDER BY activate_time ASC
			`,
			extenderId,
			minActivateTime.UTC(),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				activation := &NetworkExtenderActivationRecord{ExtenderId: extenderId}
				server.Raise(result.Scan(
					&activation.ActivationId,
					&activation.ActivateTime,
					&activation.IpVersion,
					&activation.ClientAddressHash,
					&activation.CountryCode,
					&activation.LocationId,
					&activation.CityLocationId,
					&activation.RegionLocationId,
					&activation.CountryLocationId,
					&activation.AccuracyKm,
				))
				activations = append(activations, activation)
			}
		})
	})
	return activations
}
