package controller

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net/netip"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The read precedence of connect/GEOMAP.md §6: a node's derived location,
// while it has one, then a fresh egress probe, then its genesis.

// An address the GeoLite2 build the test environment deploys resolves (to
// Canada), so the mmdb path has a lookup to store; the egress-location tests
// use the same one. A documentation address would resolve to nothing, and no
// other public address belongs in test data.
const testDerivedClientIp = "24.48.0.1"

// The location stored for one connection.
type testConnectionLocation struct {
	cityLocationId    server.Id
	regionLocationId  server.Id
	countryLocationId server.Id
	genesisLocationId *server.Id
	accuracyKm        *float32
	netTypeHosting    int
}

// The location row stored for a connection; fails the test when there is none.
func readTestConnectionLocation(t testing.TB, ctx context.Context, connectionId server.Id) *testConnectionLocation {
	t.Helper()
	var location *testConnectionLocation
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				city_location_id,
				region_location_id,
				country_location_id,
				genesis_location_id,
				accuracy_km,
				net_type_hosting
			FROM network_client_location
			WHERE connection_id = $1
			`,
			connectionId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				location = &testConnectionLocation{}
				server.Raise(result.Scan(
					&location.cityLocationId,
					&location.regionLocationId,
					&location.countryLocationId,
					&location.genesisLocationId,
					&location.accuracyKm,
					&location.netTypeHosting,
				))
			}
		})
	})
	if location == nil {
		t.Fatalf("connection %s has no location", connectionId)
	}
	return location
}

// A new provider device connected from `clientIp`: its client id and
// connection id.
func testDerivedConnection(t testing.TB, ctx context.Context, clientIp string) (server.Id, server.Id) {
	t.Helper()
	clientId := server.NewId()
	model.Testing_CreateDevice(ctx, server.NewId(), server.NewId(), clientId, "", "")
	handlerId := model.CreateNetworkClientHandler(ctx)
	connectionId, _, _, _, err := model.ConnectNetworkClient(ctx, clientId, clientIp+":0", handlerId)
	if err != nil {
		t.Fatal(err)
	}
	return clientId, connectionId
}

// A created city location row.
func testDerivedCity(ctx context.Context, city string, region string, country string, countryCode string) *model.Location {
	location := &model.Location{
		LocationType: model.LocationTypeCity,
		City:         city,
		Region:       region,
		Country:      country,
		CountryCode:  countryCode,
	}
	model.CreateLocation(ctx, location)
	return location
}

// A published derived row placing a node at `location`, crossing its genesis
// region and country.
func testDerivedRow(nodeKind int, nodeId server.Id, location *model.Location) *model.DerivedLocation {
	return &model.DerivedLocation{
		NodeKind:          nodeKind,
		NodeId:            nodeId,
		GenesisLatitude:   1.5,
		GenesisLongitude:  -2.5,
		GenesisAccuracyKm: 1000,
		Latitude:          35.6895,
		Longitude:         139.6917,
		PingCount:         24,
		PeerCount:         4,
		ResidualKm:        3,
		Reputation:        1,
		CrossedRegion:     true,
		CrossedCountry:    true,
		LocationId:        location.LocationId,
		CityLocationId:    location.CityLocationId,
		RegionLocationId:  location.RegionLocationId,
		CountryLocationId: location.CountryLocationId,
		UpdateTime:        server.NowUtc(),
	}
}

// A client with a derived location is published there, ahead of a fresh
// city-confident probe, and the mmdb lookup is stored beside it as the genesis
// the next derivation anchors to. The probe's flags still apply: they describe
// the egress, not a location.
func TestSetConnectionLocationPrefersTheDerivedLocation(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientIp := testDerivedClientIp
		ipInfo, err := server.GetIpInfoFromString(clientIp)
		connect.AssertEqual(t, err, nil)
		mmdbLocation, _, err := GetLocationForIp(ctx, clientIp)
		connect.AssertEqual(t, err, nil)
		// the fixture is only meaningful while the address resolves
		connect.AssertEqual(t, mmdbLocation.CountryCode, "ca")
		model.CreateLocation(ctx, mmdbLocation)

		tokyo := testDerivedCity(ctx, "Tokyo", "Tokyo", "Japan", "jp")
		sydney := testDerivedCity(ctx, "Sydney", "New South Wales", "Australia", "au")

		clientId, connectionId := testDerivedConnection(t, ctx, clientIp)
		model.ReplaceDerivedLocations(ctx, []*model.DerivedLocation{
			testDerivedRow(model.DerivedLocationNodeKindProvider, clientId, tokyo),
		})
		model.SetProviderEgressLocation(ctx, &model.ProviderEgressLocation{
			ClientId:      clientId,
			LocationId:    sydney.LocationId,
			CountryCode:   "au",
			CityConfident: true,
			Hosting:       true,
			ObservedAt:    server.NowUtc(),
		})

		connect.AssertEqual(t, SetConnectionLocation(ctx, connectionId, clientIp), nil)
		stored := readTestConnectionLocation(t, ctx, connectionId)
		connect.AssertEqual(t, stored.cityLocationId, tokyo.CityLocationId)
		connect.AssertEqual(t, stored.regionLocationId, tokyo.RegionLocationId)
		connect.AssertEqual(t, stored.countryLocationId, tokyo.CountryLocationId)
		if stored.genesisLocationId == nil || *stored.genesisLocationId != mmdbLocation.LocationId {
			t.Fatalf("the genesis stored as %v, want the mmdb location %s", stored.genesisLocationId, mmdbLocation.LocationId)
		}
		if stored.accuracyKm == nil || *stored.accuracyKm != float32(ipInfo.AccuracyRadiusKm) {
			t.Fatalf("the genesis radius stored as %v, want %d", stored.accuracyKm, ipInfo.AccuracyRadiusKm)
		}
		// a probe carries no hosting verdict any more (connect/GEOMAP.md
		// §11.3), so the deprecated flag on the probed row reaches nothing
		connect.AssertEqual(t, stored.netTypeHosting, 0)

		// once the row is gone -- swept, or not renewed -- the probe wins again
		model.ReplaceDerivedLocations(ctx, nil)
		connect.AssertEqual(t, SetConnectionLocation(ctx, connectionId, clientIp), nil)
		stored = readTestConnectionLocation(t, ctx, connectionId)
		connect.AssertEqual(t, stored.cityLocationId, sydney.CityLocationId)
		if stored.genesisLocationId != nil || stored.accuracyKm != nil {
			t.Fatalf("the probed location stored a genesis %v, radius %v", stored.genesisLocationId, stored.accuracyKm)
		}
	})
}

// Without a derived location the mmdb path stores the lookup as its own
// genesis, and an extender's row that shares the client's id is not the
// client's.
func TestSetConnectionLocationRecordsTheGenesisOnTheMmdbPath(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientIp := testDerivedClientIp
		mmdbLocation, _, err := GetLocationForIp(ctx, clientIp)
		connect.AssertEqual(t, err, nil)
		model.CreateLocation(ctx, mmdbLocation)
		tokyo := testDerivedCity(ctx, "Tokyo", "Tokyo", "Japan", "jp")

		clientId, connectionId := testDerivedConnection(t, ctx, clientIp)
		model.ReplaceDerivedLocations(ctx, []*model.DerivedLocation{
			testDerivedRow(model.DerivedLocationNodeKindExtender, clientId, tokyo),
		})

		connect.AssertEqual(t, SetConnectionLocation(ctx, connectionId, clientIp), nil)
		stored := readTestConnectionLocation(t, ctx, connectionId)
		connect.AssertEqual(t, stored.countryLocationId, mmdbLocation.CountryLocationId)
		if stored.genesisLocationId == nil || *stored.genesisLocationId != mmdbLocation.LocationId {
			t.Fatalf("the genesis stored as %v, want the mmdb location %s", stored.genesisLocationId, mmdbLocation.LocationId)
		}
	})
}

// A derived row whose place cannot be stored is no reason to leave a
// connection unlocated: the connection falls through to the mmdb path, as if
// it had no derived location.
func TestSetConnectionLocationDerivedWriteErrorFallsBackToMmdb(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientIp := testDerivedClientIp
		mmdbLocation, _, err := GetLocationForIp(ctx, clientIp)
		connect.AssertEqual(t, err, nil)
		model.CreateLocation(ctx, mmdbLocation)

		clientId, connectionId := testDerivedConnection(t, ctx, clientIp)
		// a row naming a location that was never created
		dangling := &model.Location{
			LocationId:        server.NewId(),
			CityLocationId:    server.NewId(),
			RegionLocationId:  server.NewId(),
			CountryLocationId: server.NewId(),
		}
		model.ReplaceDerivedLocations(ctx, []*model.DerivedLocation{
			testDerivedRow(model.DerivedLocationNodeKindProvider, clientId, dangling),
		})

		connect.AssertEqual(t, SetConnectionLocation(ctx, connectionId, clientIp), nil)
		stored := readTestConnectionLocation(t, ctx, connectionId)
		connect.AssertEqual(t, stored.countryLocationId, mmdbLocation.CountryLocationId)
	})
}

// The record carries the derived country and its continent while the extender
// has one (§6). Signing reads no database, so this runs anywhere.
func TestSignExtenderRecordCarriesTheDerivedCountry(t *testing.T) {
	rootPublicKey, rootPrivateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	config := &ExtenderConfig{
		NetworkHost: testExtenderNetworkHost,
	}
	for _, test := range []struct {
		countryCode        string
		derivedCountryCode string
		wantCountryCode    string
		wantContinentCode  string
	}{
		{countryCode: "de", derivedCountryCode: "us", wantCountryCode: "us", wantContinentCode: "NA"},
		{countryCode: "", derivedCountryCode: "jp", wantCountryCode: "jp", wantContinentCode: "AS"},
		{countryCode: "de", derivedCountryCode: "", wantCountryCode: "de", wantContinentCode: "EU"},
	} {
		extender := &model.NetworkExtender{
			ExtenderId:         server.NewId(),
			PublicKey:          rootPublicKey,
			TcpPort:            443,
			UdpPort:            443,
			DnsPort:            connect.ExtenderDnsPort,
			DnsTld:             connect.DefaultExtenderDnsTld,
			CountryCode:        test.countryCode,
			DerivedCountryCode: test.derivedCountryCode,
		}
		addresses := []*model.NetworkExtenderAddress{
			{
				IpVersion: 4,
				Ip:        netip.MustParseAddr("192.0.2.8"),
				Carriers:  []string{connect.ExtenderCarrierTcp},
			},
		}
		record, _, err := SignExtenderRecord(config, rootPrivateKey, extender, addresses, time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC))
		if err != nil {
			t.Fatal(err)
		}
		body, err := connect.NewExtenderRootKeySet(rootPublicKey).VerifyRecord(record)
		if err != nil {
			t.Fatal(err)
		}
		connect.AssertEqual(t, body.CountryCode, test.wantCountryCode)
		connect.AssertEqual(t, body.ContinentCode, test.wantContinentCode)
	}
}

// An activation of an extender with a derived location answers with a record
// in the derived country (§6); the activation history keeps where the address
// resolved to.
func TestExtenderActivateSignsTheDerivedCountry(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newTestExtenderFixture(t)
		rootPrivateKey := installTestExtenderConfig(t, fixture.api)
		rootPublicKey := rootPrivateKey.Public().(ed25519.PublicKey)

		clientSession := newTestExtenderSession(t, ctx, fixture.clientAddress("127.0.0.1"))
		result, err := ExtenderActivate(fixture.activateArgs(), clientSession)
		if err != nil {
			t.Fatal(err)
		}
		if !result.Activated {
			t.Fatalf("the activation was refused: %s", result.Error)
		}
		// loopback resolves to no country
		body := verifyTestExtenderRecord(t, rootPublicKey, result.Record)
		connect.AssertEqual(t, body.CountryCode, "")
		connect.AssertEqual(t, body.ContinentCode, "")

		extenderId := testExtenderIdForKey(ctx, t, fixture.publicKey)
		tokyo := testDerivedCity(ctx, "Tokyo", "Tokyo", "Japan", "jp")
		model.ReplaceDerivedLocations(ctx, []*model.DerivedLocation{
			testDerivedRow(model.DerivedLocationNodeKindExtender, extenderId, tokyo),
		})

		result, err = ExtenderActivate(fixture.activateArgs(), clientSession)
		if err != nil {
			t.Fatal(err)
		}
		if !result.Activated {
			t.Fatalf("the second activation was refused: %s", result.Error)
		}
		body = verifyTestExtenderRecord(t, rootPublicKey, result.Record)
		connect.AssertEqual(t, body.CountryCode, "jp")
		connect.AssertEqual(t, body.ContinentCode, "AS")

		stored := model.Testing_GetNetworkExtender(ctx, extenderId)
		connect.AssertEqual(t, stored.Extender.CountryCode, "")
	})
}

// The derived-location gauges: the published rows by kind and crossing, from
// the table, and the last derivation, from its record, with the capacity the
// planner projects for the next against its budget.
func TestStatsRefreshPublishesTheDerivedLocations(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		tokyo := testDerivedCity(ctx, "Tokyo", "Tokyo", "Japan", "jp")

		provider := testDerivedRow(model.DerivedLocationNodeKindProvider, server.NewId(), tokyo)
		provider.CrossedCountry = false
		extender := testDerivedRow(model.DerivedLocationNodeKindExtender, server.NewId(), tokyo)
		model.ReplaceDerivedLocations(ctx, []*model.DerivedLocation{provider, extender})
		runTime := time.Date(2026, 9, 23, 16, 0, 0, 0, time.UTC)
		model.AddDeriveLocationsRun(ctx, &model.DeriveLocationsRun{
			RunTime:           runTime,
			Nodes:             6,
			Terms:             20,
			Published:         2,
			ExcludedSources:   1,
			ResidualKm:        1.5,
			GenesisResidualKm: 160,
			IngestSeconds:     1.25,
			SolveSeconds:      2.5,
			PeakBytes:         180 * 1024 * 1024,
			ProjectedSeconds:  7.5,
			ProjectedBytes:    190 * 1024 * 1024,
			MaxSolveSeconds:   600,
			MaxSolveBytes:     8 * 1024 * 1024 * 1024,
		})

		statsRefreshDerivedLocations(ctx)

		connect.AssertEqual(t, testutil.ToFloat64(statsDerivedLocationsGauge.gauge.WithLabelValues("provider")), 1.0)
		connect.AssertEqual(t, testutil.ToFloat64(statsDerivedLocationsGauge.gauge.WithLabelValues("extender")), 1.0)
		connect.AssertEqual(t, testutil.ToFloat64(statsDerivedLocationCrossingsGauge.gauge.WithLabelValues("region")), 2.0)
		connect.AssertEqual(t, testutil.ToFloat64(statsDerivedLocationCrossingsGauge.gauge.WithLabelValues("country")), 1.0)
		connect.AssertEqual(t, testStatsGaugeValue(t, statsDeriveExcludedSourcesGauge), 1.0)
		connect.AssertEqual(t, testutil.ToFloat64(statsDeriveResidualKmGauge.gauge.WithLabelValues("derived")), 1.5)
		connect.AssertEqual(t, testutil.ToFloat64(statsDeriveResidualKmGauge.gauge.WithLabelValues("genesis")), 160.0)
		connect.AssertEqual(t, testStatsGaugeValue(t, statsDeriveLastRunSecondsGauge), float64(runTime.Unix()))
		for at, want := range map[string]float64{"measured": 2.5, "projected": 7.5, "budget": 600} {
			connect.AssertEqual(t, testutil.ToFloat64(statsDeriveSolveSecondsGauge.gauge.WithLabelValues(at)), want)
		}
		for at, want := range map[string]float64{"peak": 180 * 1024 * 1024, "projected": 190 * 1024 * 1024, "budget": 8 * 1024 * 1024 * 1024} {
			connect.AssertEqual(t, testutil.ToFloat64(statsDeriveSolveBytesGauge.gauge.WithLabelValues(at)), want)
		}
		connect.AssertEqual(t, testStatsGaugeValue(t, statsDeriveIngestSecondsGauge), 1.25)

		// the sweep empties the table: the kinds read zero rather than
		// disappearing
		model.ReplaceDerivedLocations(ctx, nil)
		statsRefreshDerivedLocations(ctx)
		if count := testutil.CollectAndCount(statsDerivedLocationsGauge.gauge, "urnetwork_stats_derived_locations"); count != 2 {
			t.Fatalf("derived_locations series = %d, want 2", count)
		}
		connect.AssertEqual(t, testutil.ToFloat64(statsDerivedLocationsGauge.gauge.WithLabelValues("provider")), 0.0)
		connect.AssertEqual(t, testutil.ToFloat64(statsDerivedLocationCrossingsGauge.gauge.WithLabelValues("region")), 0.0)
	})
}
