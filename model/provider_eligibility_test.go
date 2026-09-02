package model

// Shared fixtures for tests that pin the active top-level provider boundary.

import (
	"context"

	"github.com/urnetwork/server"
)

// Creates one provider-shaped client row without going through provider
// publication. This lets each downstream query prove that it enforces the
// lifecycle boundary itself instead of inheriting an upstream filter by luck.
func testingCreateProviderClient(
	ctx context.Context,
	networkId server.Id,
	sourceClientId *server.Id,
	active bool,
) server.Id {
	clientId := server.NewId()
	Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "", "")
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			UPDATE network_client
			SET source_client_id = $2, active = $3
			WHERE client_id = $1
			`,
			clientId,
			sourceClientId,
			active,
		))
	})
	SetProvide(ctx, clientId, map[ProvideMode][]byte{
		ProvideModePublic: []byte("public-secret"),
	})
	return clientId
}

// Seeds the materialized live-location input directly. Bypassing its writer is
// deliberate: a downstream eligibility test must fail if its own predicate is
// removed even if a future writer happens to filter the same client first.
func testingInsertProviderLocationReliability(
	ctx context.Context,
	clientId server.Id,
	networkId server.Id,
	city *Location,
) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO network_client_location_reliability (
				client_id,
				network_id,
				update_block_number,
				city_location_id,
				region_location_id,
				country_location_id,
				client_address_hash_count,
				location_count,
				connected
			)
			VALUES ($1, $2, 1, $3, $4, $5, 1, 1, true)
			`,
			clientId,
			networkId,
			city.LocationId,
			city.RegionLocationId,
			city.CountryLocationId,
		))
	})
}

// Seeds the lookback-0 row required by the public providers map.
func testingInsertProviderConnectionScore(ctx context.Context, clientId server.Id, city *Location) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO client_connection_reliability_score (
				client_id,
				lookback_index,
				independent_reliability_score,
				independent_reliability_weight,
				reliability_score,
				reliability_weight,
				min_block_number,
				max_block_number,
				city_location_id,
				region_location_id,
				country_location_id
			)
			VALUES ($1, 0, 1, 1, 1, 1, 1, 1, $2, $3, $4)
			`,
			clientId,
			city.LocationId,
			city.RegionLocationId,
			city.CountryLocationId,
		))
	})
}
