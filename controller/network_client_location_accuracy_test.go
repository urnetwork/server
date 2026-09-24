package controller

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The genesis accuracy radius (connect/GEOMAP.md §5.1): GeoLite2's radius
// around the coordinates it gives an address, carried by the lookup and
// stored beside the location it qualifies, so the derive phase can anchor a
// well placed node firmly and a coarse one loosely.

// The lookup carries the radius GeoLite2 gives the address. It reads no
// database, so this runs anywhere the GeoLite2 file is installed. The
// addresses are MaxMind's published test data, which the deployed build
// places in the United States at a country's radius.
func TestGetLocationForIpCarriesTheAccuracyRadius(t *testing.T) {
	ctx := context.Background()
	for _, clientIp := range []string{"67.43.156.1", "2a02:ec80::1"} {
		ipInfo, err := server.GetIpInfoFromString(clientIp)
		if err != nil {
			t.Fatal(err)
		}
		if ipInfo.AccuracyRadiusKm <= 0 {
			t.Fatalf("GeoLite2 gives %s no accuracy radius", clientIp)
		}
		_, scores, err := GetLocationForIp(ctx, clientIp)
		if err != nil {
			t.Fatal(err)
		}
		if scores.AccuracyKm == nil {
			t.Fatalf("the lookup of %s carries no accuracy radius", clientIp)
		}
		connect.AssertEqual(t, *scores.AccuracyKm, float32(ipInfo.AccuracyRadiusKm))
	}
}

// An unknown radius is nil, never zero: a zero would read as an address placed
// exactly rather than one placed not at all.
func TestGenesisAccuracyKm(t *testing.T) {
	if genesisAccuracyKm(nil) != nil {
		t.Fatal("no lookup has a radius")
	}
	if genesisAccuracyKm(&server.IpInfo{}) != nil {
		t.Fatal("a lookup without a radius has one")
	}
	accuracyKm := genesisAccuracyKm(&server.IpInfo{AccuracyRadiusKm: 5})
	if accuracyKm == nil || *accuracyKm != 5 {
		t.Fatalf("a 5 km radius stored as %v", accuracyKm)
	}
}

// The accuracy radius stored with one connection's location, nil when NULL.
func testConnectionAccuracyKm(t testing.TB, ctx context.Context, connectionId server.Id) *float32 {
	t.Helper()
	var accuracyKm *float32
	found := false
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT accuracy_km FROM network_client_location WHERE connection_id = $1`,
			connectionId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				found = true
				server.Raise(result.Scan(&accuracyKm))
			}
		})
	})
	if !found {
		t.Fatalf("connection %s has no location", connectionId)
	}
	return accuracyKm
}

// A GeoLite2 location is stored with GeoLite2's radius; a location that came
// from the egress probe is stored with none, since the radius describes the
// lookup the probe replaced.
func TestSetConnectionLocationStoresTheAccuracyRadius(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		// MaxMind test data, which GeoLite2 places only in its country
		clientIp := "67.43.156.1"
		ipInfo, err := server.GetIpInfoFromString(clientIp)
		connect.AssertEqual(t, err, nil)

		connectClient := func() (server.Id, server.Id) {
			networkId := server.NewId()
			clientId := server.NewId()
			model.Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "", "")
			handlerId := model.CreateNetworkClientHandler(ctx)
			connectionId, _, _, _, err := model.ConnectNetworkClient(ctx, clientId, clientIp+":0", handlerId)
			connect.AssertEqual(t, err, nil)
			return clientId, connectionId
		}

		// the mmdb path: GeoLite2's location, with its radius
		_, connectionId := connectClient()
		connect.AssertEqual(t, SetConnectionLocation(ctx, connectionId, clientIp), nil)
		accuracyKm := testConnectionAccuracyKm(t, ctx, connectionId)
		if accuracyKm == nil {
			t.Fatal("the GeoLite2 location was stored without its radius")
		}
		connect.AssertEqual(t, *accuracyKm, float32(ipInfo.AccuracyRadiusKm))

		// the probed path: a fresh city-confident probe wins and is stored
		// without a radius
		probed := &model.Location{
			LocationType: model.LocationTypeCity,
			City:         "Tokyo",
			Region:       "Tokyo",
			Country:      "Japan",
			CountryCode:  "jp",
		}
		model.CreateLocation(ctx, probed)
		clientId, connectionId := connectClient()
		model.SetProviderEgressLocation(ctx, &model.ProviderEgressLocation{
			ClientId:      clientId,
			LocationId:    probed.LocationId,
			CountryCode:   "jp",
			CityConfident: true,
			ObservedAt:    server.NowUtc(),
		})
		connect.AssertEqual(t, SetConnectionLocation(ctx, connectionId, clientIp), nil)
		if accuracyKm := testConnectionAccuracyKm(t, ctx, connectionId); accuracyKm != nil {
			t.Fatalf("the probed location was stored with a radius of %f km", *accuracyKm)
		}

		// a stale probe does not win, and the GeoLite2 radius is stored again
		clientId, connectionId = connectClient()
		model.SetProviderEgressLocation(ctx, &model.ProviderEgressLocation{
			ClientId:      clientId,
			LocationId:    probed.LocationId,
			CountryCode:   "jp",
			CityConfident: true,
			ObservedAt:    server.NowUtc().Add(-(model.ProviderEgressLocationMaxAge + time.Hour)),
		})
		connect.AssertEqual(t, SetConnectionLocation(ctx, connectionId, clientIp), nil)
		accuracyKm = testConnectionAccuracyKm(t, ctx, connectionId)
		if accuracyKm == nil || *accuracyKm != float32(ipInfo.AccuracyRadiusKm) {
			t.Fatalf("the GeoLite2 location behind a stale probe stored radius %v", accuracyKm)
		}
	})
}
