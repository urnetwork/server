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

func GetNetworkBlockedLocations(
	ctx context.Context,
	networkId server.Id,
) []server.Id {

	var locations []server.Id

	server.Tx(ctx, func(tx server.PgTx) {

		rows, _ := tx.Query(
			ctx,
			`
				SELECT client_location_id
				FROM exclude_network_client_location
				WHERE network_id = $1
			`,
			networkId,
		)

		for rows.Next() {
			var locationId server.Id
			rows.Scan(&locationId)
			locations = append(locations, locationId)
		}

	})

	return locations
}
