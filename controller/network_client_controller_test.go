package controller

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"
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
		connect.AssertEqual(t, err, nil)

		model.SetProviderEgressLocation(ctx, &model.ProviderEgressLocation{
			ClientId:    clientId,
			LocationId:  probed.LocationId,
			CountryCode: "jp",
			ObservedAt:  server.NowUtc(),
		})

		err = SetConnectionLocation(ctx, connectionId, "8.8.8.8")
		connect.AssertEqual(t, err, nil)

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
		connect.AssertEqual(t, countryLocationId, probed.CountryLocationId)
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
		connect.AssertEqual(t, err, nil)

		// no SetProviderEgressLocation call -> mmdb path
		err = SetConnectionLocation(ctx, connectionId, "8.8.8.8")
		connect.AssertEqual(t, err, nil)

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
		connect.AssertEqual(t, count, 1)
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
		connect.AssertEqual(t, err, nil)
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
		connect.AssertEqual(t, err, nil)

		model.SetProviderEgressLocation(ctx, &model.ProviderEgressLocation{
			ClientId:    clientId,
			LocationId:  probed.LocationId,
			CountryCode: "jp",
			ObservedAt:  server.NowUtc().Add(-(model.ProviderEgressLocationMaxAge + time.Hour)),
		})

		err = SetConnectionLocation(ctx, connectionId, clientIp)
		connect.AssertEqual(t, err, nil)

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
		connect.AssertEqual(t, countryLocationId, mmdbLocation.CountryLocationId)
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
		connect.AssertEqual(t, err, nil)
		model.CreateLocation(ctx, mmdbLocation)

		networkId := server.NewId()
		clientId := server.NewId()
		model.Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "", "")

		handlerId := model.CreateNetworkClientHandler(ctx)
		connectionId, _, _, _, err := model.ConnectNetworkClient(ctx, clientId, clientIp+":0", handlerId)
		connect.AssertEqual(t, err, nil)

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
		connect.AssertEqual(t, err, nil)

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
		connect.AssertEqual(t, countryLocationId, mmdbLocation.CountryLocationId)
	})
}

// net_type_foreign on the probed path must be computed the same way as the
// mmdb path: the ARIN org country of the control ip against the mmdb country
// of that SAME control ip, not against the probed country. 8.8.8.8 resolves
// (both via mmdb and ARIN org registration) to "us", so it is non-foreign by
// construction -- this is the same ip used by the parity tests above. The
// probed country here is deliberately "jp", a different country than the
// control ip's mmdb country: probing having changed the answer must not, by
// itself, flip net_type_foreign, or a probed provider is penalized a full
// ranking tier precisely for the reason this feature exists. A prior version
// of this code compared the ARIN org country against the probed country
// instead of the control ip's mmdb country, which made this exact scenario
// (egress country differs from control-ip country) foreign=1 instead of the
// correct foreign=0.
func TestSetConnectionLocationProbedNetTypeForeignMatchesMmdbParity(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		clientIp := "8.8.8.8"

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
		connect.AssertEqual(t, err, nil)

		// probed country ("jp") deliberately differs from the control ip's
		// mmdb country ("us" for 8.8.8.8).
		model.SetProviderEgressLocation(ctx, &model.ProviderEgressLocation{
			ClientId:    clientId,
			LocationId:  probed.LocationId,
			CountryCode: "jp",
			ObservedAt:  server.NowUtc(),
		})

		err = SetConnectionLocation(ctx, connectionId, clientIp)
		connect.AssertEqual(t, err, nil)

		var netTypeForeign int
		server.Db(ctx, func(conn server.PgConn) {
			result, qerr := conn.Query(
				ctx,
				`SELECT net_type_foreign FROM network_client_location WHERE connection_id = $1`,
				connectionId,
			)
			server.WithPgResult(result, qerr, func() {
				if result.Next() {
					server.Raise(result.Scan(&netTypeForeign))
				}
			})
		})
		connect.AssertEqual(t, netTypeForeign, 0)
	})
}

// A fresh probed location's Hosting/Proxy flags must map onto the stored
// connection's net_type_hosting/net_type_privacy scores. Mobile must NOT map
// onto net_type_virtual: Hosting/Proxy have direct mmdb-path equivalents
// (ipInfo.Hosting/ipInfo.Privacy, see GetLocationForIp), but Mobile has none
// (IpInfo has no Mobile concept at all, and NetTypeVirtual is only ever set
// from the ipinfo schema's is_satellite field, never from DB-IP or from
// anything Mobile-shaped) -- deriving NetTypeVirtual from Mobile would give
// a probed mobile provider a ranking penalty an identical unprobed mobile
// provider never takes, breaking the parity this feature promises.
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
		connect.AssertEqual(t, err, nil)

		model.SetProviderEgressLocation(ctx, &model.ProviderEgressLocation{
			ClientId:    clientId,
			LocationId:  probed.LocationId,
			CountryCode: "jp",
			Hosting:     true,
			Proxy:       true,
			Mobile:      true,
			ObservedAt:  server.NowUtc(),
		})

		err = SetConnectionLocation(ctx, connectionId, "8.8.8.8")
		connect.AssertEqual(t, err, nil)

		var netTypeHosting int
		var netTypePrivacy int
		var netTypeVirtual int
		server.Db(ctx, func(conn server.PgConn) {
			result, qerr := conn.Query(
				ctx,
				`SELECT net_type_hosting, net_type_privacy, net_type_virtual FROM network_client_location WHERE connection_id = $1`,
				connectionId,
			)
			server.WithPgResult(result, qerr, func() {
				if result.Next() {
					server.Raise(result.Scan(&netTypeHosting, &netTypePrivacy, &netTypeVirtual))
				}
			})
		})
		connect.AssertEqual(t, netTypeHosting, 1)
		connect.AssertEqual(t, netTypePrivacy, 1)
		connect.AssertEqual(t, netTypeVirtual, 0)
	})
}

// A failing probed-egress lookup must not propagate out of
// SetConnectionLocation, and the connection must still be located via mmdb.
//
// model.GetFreshProviderEgressLocationForConnection goes through server.Db,
// which re-panics any postgres error that is neither transient nor a
// connection error (see isTransientError / isConnectionError in db.go).
// undefined_table (42P01) is exactly such an error, and it is not a
// hypothetical one: provider_egress_location is a new table in this change,
// so rolling the binary before running `bringyourctl db migrate` makes every
// single connection announce hit it.
//
// That matters far more than a failed lookup, because SetConnectionLocation is
// called from ConnectNetworkClient *before* connect's disconnect-cleanup defer
// is registered (connect/transport_announce.go): an escaping panic tears the
// connection down and orphans its network_client_connection row as
// connected = true. This project has already lost ~30k rows to that exact
// deploy-ordering mistake once.
//
// The failure is injected for real -- the table is dropped, so the query
// genuinely raises 42P01 out of the pgx driver -- rather than asserted against
// a mock, because the thing under test is what server.Db does with a real
// postgres error, not what the call site does with a fabricated one.
func TestSetConnectionLocationEgressLookupErrorFallsBackToMmdb(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		clientIp := "8.8.8.8"

		mmdbLocation, _, err := GetLocationForIp(ctx, clientIp)
		connect.AssertEqual(t, err, nil)
		model.CreateLocation(ctx, mmdbLocation)

		networkId := server.NewId()
		clientId := server.NewId()
		model.Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "", "")

		handlerId := model.CreateNetworkClientHandler(ctx)
		connectionId, _, _, _, err := model.ConnectNetworkClient(ctx, clientId, clientIp+":0", handlerId)
		connect.AssertEqual(t, err, nil)

		// a fresh probed entry exists, so the lookup is definitely reached --
		// then the table it reads is dropped out from under it, which is the
		// state a deploy-before-migrate leaves the binary in.
		model.SetProviderEgressLocation(ctx, &model.ProviderEgressLocation{
			ClientId:    clientId,
			LocationId:  mmdbLocation.LocationId,
			CountryCode: "jp",
			ObservedAt:  server.NowUtc(),
		})
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DROP TABLE provider_egress_location`))
		})

		// the panic is caught here rather than left to kill the test binary,
		// so a regression reports as this test failing with the reason
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("the probed egress lookup must not panic out of SetConnectionLocation; it reached the caller as: %v", r)
				}
			}()
			err = SetConnectionLocation(ctx, connectionId, clientIp)
		}()
		connect.AssertEqual(t, err, nil)

		// and the mmdb path still produced a location for the connection
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
		connect.AssertEqual(t, countryLocationId, mmdbLocation.CountryLocationId)
	})
}
