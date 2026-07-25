package controller

import (
	"context"
	"testing"
	"time"

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

// A probed egress location observed longer ago than ProviderEgressLocationMaxAge
// must be ignored, falling back to the mmdb lookup exactly as if there were no
// probed entry at all.
func TestSetConnectionLocationStaleProbedFallsBackToMmdb(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		clientIp := "8.8.8.8"

		// the mmdb location this ip actually resolves to; created up front so
		// its canonical (deduped) location id is known for the assertion.
		mmdbLocation, _, err := GetLocationForIp(ctx, clientIp)
		assert.Equal(t, err, nil)
		model.CreateLocation(ctx, mmdbLocation)

		// a stale probed location, deliberately a different country than
		// whatever the mmdb lookup returns, so a wrongly-preferred probed
		// value would be caught by the assertion below.
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
		connectionId, _, _, _, err := model.ConnectNetworkClient(ctx, clientId, clientIp+":0", handlerId)
		assert.Equal(t, err, nil)

		model.SetProviderEgressLocation(ctx, &model.ProviderEgressLocation{
			ClientId:    clientId,
			LocationId:  probed.LocationId,
			CountryCode: "jp",
			ObservedAt:  server.NowUtc().Add(-(model.ProviderEgressLocationMaxAge + time.Hour)),
		})

		err = SetConnectionLocation(ctx, connectionId, clientIp)
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
		assert.Equal(t, countryLocationId, mmdbLocation.CountryLocationId)
		if countryLocationId == probed.CountryLocationId {
			t.Fatal("stale probed location must not be used")
		}
	})
}

// A probed egress location whose location_id points at no existing location
// row makes the storage write fail (SetConnectionLocation returns an error
// rather than panicking, see model.SetConnectionLocation). The connection
// must still end up located via the mmdb path, and SetConnectionLocation
// itself must not panic or error out on this.
func TestSetConnectionLocationProbedWriteErrorFallsBackToMmdb(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		clientIp := "8.8.8.8"

		mmdbLocation, _, err := GetLocationForIp(ctx, clientIp)
		assert.Equal(t, err, nil)
		model.CreateLocation(ctx, mmdbLocation)

		networkId := server.NewId()
		clientId := server.NewId()
		model.Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "", "")

		handlerId := model.CreateNetworkClientHandler(ctx)
		connectionId, _, _, _, err := model.ConnectNetworkClient(ctx, clientId, clientIp+":0", handlerId)
		assert.Equal(t, err, nil)

		// a fresh probed entry pointing at a location id that was never
		// created via CreateLocation -- the storage write in
		// model.SetConnectionLocation must fail cleanly on this, not panic.
		model.SetProviderEgressLocation(ctx, &model.ProviderEgressLocation{
			ClientId:    clientId,
			LocationId:  server.NewId(),
			CountryCode: "jp",
			ObservedAt:  server.NowUtc(),
		})

		err = SetConnectionLocation(ctx, connectionId, clientIp)
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
		assert.Equal(t, countryLocationId, mmdbLocation.CountryLocationId)
	})
}

// A fresh probed location's Hosting/Proxy flags must map onto the stored
// connection's net_type_hosting/net_type_privacy scores.
func TestSetConnectionLocationMapsProbedFlagsToScores(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

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
			Hosting:     true,
			Proxy:       true,
			ObservedAt:  server.NowUtc(),
		})

		err = SetConnectionLocation(ctx, connectionId, "8.8.8.8")
		assert.Equal(t, err, nil)

		var netTypeHosting int
		var netTypePrivacy int
		server.Db(ctx, func(conn server.PgConn) {
			result, qerr := conn.Query(
				ctx,
				`SELECT net_type_hosting, net_type_privacy FROM network_client_location WHERE connection_id = $1`,
				connectionId,
			)
			server.WithPgResult(result, qerr, func() {
				if result.Next() {
					server.Raise(result.Scan(&netTypeHosting, &netTypePrivacy))
				}
			})
		})
		assert.Equal(t, netTypeHosting, 1)
		assert.Equal(t, netTypePrivacy, 1)
	})
}
