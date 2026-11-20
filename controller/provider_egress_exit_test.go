// A provider is judged by its probed exit (owner decision 2026-11-20, "judge
// by exit IP"; model/provider_egress_exit_model.go). The ingest judges the
// provider's live connections by the exit it submits, and the connect
// announce judges each new connection by it: a slot that connects from a
// residential proxy but exits from a datacenter is no verified subscriber,
// is placed in its exit's country and counts at no unit, while a provider that
// exits from its own address is unchanged.
//
// The ARIN classifier is the fixture's, so the facts do not depend on the
// deployed database. The connecting addresses are RFC 5737 documentation
// addresses, which GeoLite2 places nowhere; the exits are the MaxMind test
// addresses this suite already places (provider_egress_location_controller_test.go).
package controller

import (
	"context"
	"net"
	"net/netip"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The build epochs of the fixture classifier's latest and old databases.
const (
	testExitArinLatestEpoch = int64(1800086400)
	testExitArinOldEpoch    = int64(1800000000)
)

// Classifies every address for the rest of the test, in both slots: the
// subscribers given as verified residential subscribers, everything else, a
// datacenter exit among them, as unknown to the catalog. The test cannot be
// parallel.
func useTestExitArinClassifier(t testing.TB, subscriberIps ...netip.Addr) {
	t.Helper()
	previous := arinInfoPairForAddr
	t.Cleanup(func() {
		arinInfoPairForAddr = previous
	})
	arinInfoPairForAddr = func(addr netip.Addr) (*server.ArinInfo, *server.ArinInfo, error) {
		qualityState := "unknown"
		for _, subscriberIp := range subscriberIps {
			if addr.Unmap() == subscriberIp {
				qualityState = "subscriber"
			}
		}
		info := func(epoch int64) *server.ArinInfo {
			return &server.ArinInfo{
				DatabaseBuildEpoch:   epoch,
				ClassifierVersion:    1,
				QualityPolicyVersion: 2,
				QualityState:         qualityState,
				NonQuality:           qualityState != "subscriber",
			}
		}
		return info(testExitArinLatestEpoch), info(testExitArinOldEpoch), nil
	}
}

// One connection's location and ARIN facts.
type testExitLocationRow struct {
	cityLocationId      server.Id
	countryLocationId   server.Id
	arinRisk            bool
	arinNonQuality      bool
	arinQualityVerified bool
	arinEpoch           int64
	arinOldNonQuality   bool
	arinOldEpoch        int64
}

func testExitConnectionLocation(t testing.TB, ctx context.Context, connectionId server.Id) testExitLocationRow {
	t.Helper()
	row := testExitLocationRow{}
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`
			SELECT
				city_location_id,
				country_location_id,
				arin_risk,
				arin_non_quality,
				arin_quality_verified,
				arin_database_build_epoch,
				arin_old_non_quality,
				arin_old_database_build_epoch
			FROM network_client_location
			WHERE connection_id = $1
			`,
			connectionId,
		).Scan(
			&row.cityLocationId,
			&row.countryLocationId,
			&row.arinRisk,
			&row.arinNonQuality,
			&row.arinQualityVerified,
			&row.arinEpoch,
			&row.arinOldNonQuality,
			&row.arinOldEpoch,
		))
	})
	return row
}

// A new connection of the client from the address.
func testExitConnect(t testing.TB, ctx context.Context, clientId server.Id, ip string, handlerId server.Id) server.Id {
	t.Helper()
	connectionId, _, _, _, err := model.ConnectNetworkClient(ctx, clientId, net.JoinHostPort(ip, "40000"), handlerId)
	if err != nil {
		t.Fatal(err)
	}
	return connectionId
}

// The country row the probe stored the client's exit under.
func testExitProbedCountryLocationId(t testing.TB, ctx context.Context, clientId server.Id) server.Id {
	t.Helper()
	egress := model.GetProviderEgressLocation(ctx, clientId)
	if egress == nil {
		t.Fatal("the probe stored no location")
	}
	location := model.GetLocation(ctx, egress.LocationId)
	if location == nil {
		t.Fatal("the probe's location has no row")
	}
	return location.CountryLocationId
}

// A slot connects from a residential proxy, a verified subscriber at a city of
// its own, and the probe sees its users' traffic leave from a datacenter in
// another country. Judged by the exit, the live connection is no verified
// subscriber and is placed in the exit's country, the slot counts at no unit,
// and a reconnect from the same proxy is judged the same way.
func TestSubmitProviderEgressLocationJudgesTheProviderByItsExit(t *testing.T) {
	requireGeoLite2Database(t)

	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		// documentation addresses resolve to no place, whatever the config overrides
		server.UseNoIpOverridesForTest(t)
		const residentialIp = "198.51.100.10"
		useTestExitArinClassifier(t, netip.MustParseAddr(residentialIp))

		clientId := testEgressProvider(ctx)
		handlerId := model.CreateNetworkClientHandler(ctx)
		connectionId := testExitConnect(t, ctx, clientId, residentialIp, handlerId)
		// the proxy's city, where the connection is stored before any probe
		proxyCity := &model.Location{
			LocationType: model.LocationTypeCity,
			City:         "city-" + clientId.String(),
			Region:       "region-" + clientId.String(),
			Country:      "Canada",
			CountryCode:  "ca",
		}
		model.CreateLocation(ctx, proxyCity)
		latest, old, err := arinInfoPairForAddr(netip.MustParseAddr(residentialIp))
		if err != nil {
			t.Fatal(err)
		}
		scores := &model.ConnectionLocationScores{}
		setArinConnectionFacts(scores, latest, old)
		if err := model.SetConnectionLocation(ctx, connectionId, proxyCity.LocationId, scores); err != nil {
			t.Fatal(err)
		}
		if row := testExitConnectionLocation(t, ctx, connectionId); row.arinNonQuality || !row.arinQualityVerified {
			t.Fatalf("the fixture's residential connection is no verified subscriber: %+v", row)
		}

		_, err = SubmitProviderEgressLocation(ctx, &SubmitProviderEgressLocationArgs{
			ClientId:   clientId,
			ExitIp:     testEgressCountryExitIp,
			ObservedAt: server.NowUtc(),
		})
		if err != nil {
			t.Fatal(err)
		}
		exitCountryLocationId := testExitProbedCountryLocationId(t, ctx, clientId)
		if exitCountryLocationId == proxyCity.CountryLocationId {
			t.Fatal("the fixture's exit is in the proxy's country")
		}

		if !model.GetVerifyProxiedEgress(ctx, []server.Id{clientId})[clientId] {
			t.Fatal("a slot whose exit differs from its connection is not marked, so it keeps the proxy's unit")
		}
		judged := func(what string, connectionId server.Id) {
			t.Helper()
			row := testExitConnectionLocation(t, ctx, connectionId)
			if !row.arinNonQuality || row.arinQualityVerified || row.arinEpoch != testExitArinLatestEpoch {
				t.Fatalf("%s is judged by its residential proxy, not its datacenter exit: %+v", what, row)
			}
			if !row.arinOldNonQuality || row.arinOldEpoch != testExitArinOldEpoch {
				t.Fatalf("%s keeps the proxy's old facts: %+v", what, row)
			}
			if row.arinRisk {
				t.Fatalf("%s took a risk it does not have: %+v", what, row)
			}
			if row.countryLocationId != exitCountryLocationId {
				t.Fatalf("%s is placed in country %s, want the exit's %s", what, row.countryLocationId, exitCountryLocationId)
			}
		}
		judged("the live connection", connectionId)

		// the announce of a new connection from the same proxy
		reconnectionId := testExitConnect(t, ctx, clientId, residentialIp, handlerId)
		if err := SetConnectionLocation(ctx, reconnectionId, residentialIp); err != nil {
			t.Fatal(err)
		}
		judged("a reconnect", reconnectionId)
	})
}

// A provider whose probe sees its users' traffic leave from its own address is
// unchanged: no mark, the same facts and the same place, on its live
// connection and on a reconnect.
func TestSubmitProviderEgressLocationLeavesADirectProviderAsItWas(t *testing.T) {
	requireGeoLite2Database(t)

	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		useTestExitArinClassifier(t, netip.MustParseAddr(testEgressCityExitIp))

		clientId := testEgressProvider(ctx)
		handlerId := model.CreateNetworkClientHandler(ctx)
		connectionId := testExitConnect(t, ctx, clientId, testEgressCityExitIp, handlerId)
		if err := SetConnectionLocation(ctx, connectionId, testEgressCityExitIp); err != nil {
			t.Fatal(err)
		}
		before := testExitConnectionLocation(t, ctx, connectionId)
		if before.arinNonQuality || !before.arinQualityVerified {
			t.Fatalf("the fixture's direct provider is no verified subscriber: %+v", before)
		}

		_, err := SubmitProviderEgressLocation(ctx, &SubmitProviderEgressLocationArgs{
			ClientId:   clientId,
			ExitIp:     testEgressCityExitIp,
			ObservedAt: server.NowUtc(),
		})
		if err != nil {
			t.Fatal(err)
		}

		if model.GetVerifyProxiedEgress(ctx, []server.Id{clientId})[clientId] {
			t.Fatal("a provider that exits from its own address is marked")
		}
		judgement := model.GetProviderEgressExitJudgement(ctx, clientId)
		if judgement == nil || judgement.ExitDiffers || judgement.ArinNonQuality || !judgement.ArinQualityVerified {
			t.Fatalf("the direct provider's judgement %+v, want its own address's facts", judgement)
		}
		if after := testExitConnectionLocation(t, ctx, connectionId); after != before {
			t.Fatalf("the live connection changed from %+v to %+v", before, after)
		}

		reconnectionId := testExitConnect(t, ctx, clientId, testEgressCityExitIp, handlerId)
		if err := SetConnectionLocation(ctx, reconnectionId, testEgressCityExitIp); err != nil {
			t.Fatal(err)
		}
		if reconnected := testExitConnectionLocation(t, ctx, reconnectionId); reconnected.arinNonQuality || !reconnected.arinQualityVerified ||
			reconnected.cityLocationId != before.cityLocationId || reconnected.countryLocationId != before.countryLocationId {
			t.Fatalf("a reconnect is %+v, want the live connection's %+v", reconnected, before)
		}
	})
}

// Placement by a fresh probe: the probe wins whenever the provider's exit is in
// none of the networks it connects from, however coarse, and otherwise only as
// probedLocationPreferred allows, never coarsening a same-country city.
func TestProbePlacesConnectionWhenTheExitDiffers(t *testing.T) {
	probedCountry := &model.ProviderEgressLocation{
		LocationId:  server.NewId(),
		CountryCode: "ca",
	}
	mmdbCity := func(countryCode string) *model.Location {
		return &model.Location{
			LocationId:   server.NewId(),
			LocationType: model.LocationTypeCity,
			CountryCode:  countryCode,
		}
	}
	differs := &model.ProviderEgressExitJudgement{ExitDiffers: true}
	same := &model.ProviderEgressExitJudgement{}
	cases := []struct {
		name   string
		egress *model.ProviderEgressLocation
		exit   *model.ProviderEgressExitJudgement
		mmdb   *model.Location
		places bool
	}{
		{name: "no probe", egress: nil, exit: differs, mmdb: mmdbCity("ca"), places: false},
		{name: "a same-country city, no judgement", egress: probedCountry, exit: nil, mmdb: mmdbCity("ca"), places: false},
		{name: "a same-country city, the exit is the connection's network", egress: probedCountry, exit: same, mmdb: mmdbCity("ca"), places: false},
		{name: "a same-country city, the exit differs", egress: probedCountry, exit: differs, mmdb: mmdbCity("ca"), places: true},
		{name: "another country's city", egress: probedCountry, exit: nil, mmdb: mmdbCity("us"), places: true},
		{name: "no lookup", egress: probedCountry, exit: same, mmdb: nil, places: true},
	}
	for _, c := range cases {
		if places := probePlacesConnection(c.egress, c.exit, c.mmdb); places != c.places {
			t.Errorf("%s: the probe places the connection %t, want %t", c.name, places, c.places)
		}
	}
}

// A provider whose exit is in none of the networks it connects from is placed
// by its probe ahead of its derived location, which refines the lookup of the
// address it connects from. A provider that exits from its own address keeps
// the precedence of connect/GEOMAP.md §6: its derived location first.
func TestSetConnectionLocationPlacesAProviderWhoseExitDiffersByItsProbe(t *testing.T) {
	requireGeoLite2Database(t)

	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		useTestExitArinClassifier(t, netip.MustParseAddr(testDerivedClientIp))
		tokyo := testDerivedCity(ctx, "Tokyo", "Tokyo", "Japan", "jp")

		slotId, slotConnectionId := testDerivedConnection(t, ctx, testDerivedClientIp)
		directId, directConnectionId := testDerivedConnection(t, ctx, testDerivedClientIp)
		model.ReplaceDerivedLocations(ctx, []*model.DerivedLocation{
			testDerivedRow(model.DerivedLocationNodeKindProvider, slotId, tokyo),
			testDerivedRow(model.DerivedLocationNodeKindProvider, directId, tokyo),
		})
		submit := func(clientId server.Id, exitIp string) {
			t.Helper()
			_, err := SubmitProviderEgressLocation(ctx, &SubmitProviderEgressLocationArgs{
				ClientId:   clientId,
				ExitIp:     exitIp,
				ObservedAt: server.NowUtc(),
			})
			if err != nil {
				t.Fatal(err)
			}
		}
		// the slot's users' traffic leaves from another network than it
		// connects from; the direct provider's from its own address
		submit(slotId, testEgressCountryExitIp)
		submit(directId, testDerivedClientIp)

		if err := SetConnectionLocation(ctx, slotConnectionId, testDerivedClientIp); err != nil {
			t.Fatal(err)
		}
		exitCountryLocationId := testExitProbedCountryLocationId(t, ctx, slotId)
		if slot := readTestConnectionLocation(t, ctx, slotConnectionId); slot.countryLocationId != exitCountryLocationId || slot.genesisLocationId != nil {
			t.Fatalf("the slot is placed at %+v, want its exit's country %s with no genesis, not its derived city %s", slot, exitCountryLocationId, tokyo.CityLocationId)
		}

		if err := SetConnectionLocation(ctx, directConnectionId, testDerivedClientIp); err != nil {
			t.Fatal(err)
		}
		if direct := readTestConnectionLocation(t, ctx, directConnectionId); direct.cityLocationId != tokyo.CityLocationId {
			t.Fatalf("the direct provider is placed at %+v, want its derived city %s", direct, tokyo.CityLocationId)
		}
	})
}
