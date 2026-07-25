package controller

import (
	"context"
	"testing"

	"github.com/go-playground/assert/v2"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// A provider with a fresh probed egress location must be located from that
// entry, not from the mmdb lookup on its control ip.
func TestSetConnectionLocationPrefersEgressLocation(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		// the probed egress location: japan
		probed := &model.Location{
			LocationType: model.LocationTypeCountry,
			Country:      "Japan",
			CountryCode:  "jp",
		}
		model.CreateLocation(ctx, probed)

		networkId := server.NewId()
		clientId := server.NewId()
		model.Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "", "")

		handlerId := model.CreateNetworkClientHandler(ctx)
		connectionId, _, _, _, err := model.ConnectNetworkClient(ctx, clientId, "8.8.8.8:0", handlerId)
		assert.Equal(t, err, nil)

		model.SetProviderEgressLocation(ctx, &model.ProviderEgressLocation{
			ClientId:    clientId,
			LocationId:  probed.LocationId,
			CountryCode: "jp",
			ObservedAt:  server.NowUtc(),
		})

		err = SetConnectionLocation(ctx, connectionId, "8.8.8.8")
		assert.Equal(t, err, nil)

		var countryLocationId server.Id
		server.Db(ctx, func(conn server.PgConn) {
			result, qerr := conn.Query(
				ctx,
				`SELECT country_location_id FROM network_client_location WHERE connection_id = $1`,
				connectionId,
			)
			server.WithPgResult(result, qerr, func() {
				if result.Next() {
					server.Raise(result.Scan(&countryLocationId))
				}
			})
		})
		assert.Equal(t, countryLocationId, probed.CountryLocationId)
	})
}

// With no probed entry, the existing mmdb path still applies.
func TestSetConnectionLocationFallsBackToMmdb(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId := server.NewId()
		clientId := server.NewId()
		model.Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "", "")

		handlerId := model.CreateNetworkClientHandler(ctx)
		connectionId, _, _, _, err := model.ConnectNetworkClient(ctx, clientId, "8.8.8.8:0", handlerId)
		assert.Equal(t, err, nil)

		// no SetProviderEgressLocation call -> mmdb path
		err = SetConnectionLocation(ctx, connectionId, "8.8.8.8")
		assert.Equal(t, err, nil)

		var count int
		server.Db(ctx, func(conn server.PgConn) {
			result, qerr := conn.Query(
				ctx,
				`SELECT COUNT(*) FROM network_client_location WHERE connection_id = $1`,
				connectionId,
			)
			server.WithPgResult(result, qerr, func() {
				if result.Next() {
					server.Raise(result.Scan(&count))
				}
			})
		})
		assert.Equal(t, count, 1)
	})
}
