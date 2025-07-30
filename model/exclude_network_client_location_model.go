package model

import (
	"context"

	"github.com/urnetwork/server"
)

func NetworkBlockLocation(
	ctx context.Context,
	networkId server.Id,
	locationId server.Id,
) {

	server.Tx(ctx, func(tx server.PgTx) {

		tx.Exec(
			ctx,
			`
				INSERT INTO exclude_network_client_location (
					network_id,
					client_location_id
				)
				VALUES ($1, $2)
				ON CONFLICT (network_id, client_location_id) DO NOTHING
			`,
			networkId,
			locationId,
		)

	})

}

func NetworkUnblockLocation(
	ctx context.Context,
	networkId server.Id,
	locationId server.Id,
) {

	server.Tx(ctx, func(tx server.PgTx) {

		tx.Exec(
			ctx,
			`
				DELETE FROM exclude_network_client_location
				WHERE network_id = $1 AND client_location_id = $2
			`,
			networkId,
			locationId,
		)

	})

}

type BlockedLocation struct {
	LocationId   server.Id    `json:"location_id"`
	LocationName string       `json:"location_name"`
	LocationType LocationType `json:"location_type"`
	CountryCode  string       `json:"country_code"`
}

func GetNetworkBlockedLocations(
	ctx context.Context,
	networkId server.Id,
) []BlockedLocation {

	var locations []BlockedLocation

	server.Tx(ctx, func(tx server.PgTx) {

		result, err := tx.Query(
			ctx,
			`
				SELECT
					exclude_network_client_location.client_location_id,
					location.location_name,
					location.location_type,
					location.country_code
				FROM exclude_network_client_location
				INNER JOIN location on location.location_id = exclude_network_client_location.client_location_id
				WHERE network_id = $1
			`,
			networkId,
		)

		server.WithPgResult(result, err, func() {

			for result.Next() {

				var blockedLocation BlockedLocation
				server.Raise(result.Scan(
					&blockedLocation.LocationId,
					&blockedLocation.LocationName,
					&blockedLocation.LocationType,
					&blockedLocation.CountryCode,
				))

				locations = append(locations, blockedLocation)
			}

		})

	})

	return locations
}
