// A provider is judged by its probed exit (owner decisions 2026-11-20, "judge
// by exit IP" and "apply exit risk too"; model/provider_egress_exit_model.go).
// The ingest judges the provider's live connections by the exit it submits,
// and the connect announce judges each new connection by it and records the
// system of the network it came from. A slot that connects from a residential
// proxy but exits from a datacenter is no verified subscriber; once a
// connection since its first probe gives the evidence, its exit differs, so it
// is placed in its exit's country and counts at no unit. A risky exit takes a
// provider out of every bucket. A provider that exits from its own address is
// unchanged.
//
// The ARIN classifier and the autonomous systems are the fixture's, so the
// facts do not depend on the deployed databases. The connecting addresses are
// RFC 5737 documentation addresses, which GeoLite2 places nowhere; the exits are
// the MaxMind test addresses this suite already places
// (provider_egress_location_controller_test.go). The systems are RFC 5398
// documentation numbers.
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
// subscribers given as verified residential subscribers, the risky ones as a
// VPN's unknown addresses with risk, and everything else, a datacenter exit
// among them, as unknown to the catalog. The test cannot be parallel.
func useTestExitArinClassifier(t testing.TB, subscriberIps []netip.Addr, riskyIps ...netip.Addr) {
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
		risk := false
		for _, riskyIp := range riskyIps {
			if addr.Unmap() == riskyIp {
				risk = true
			}
		}
		info := func(epoch int64) *server.ArinInfo {
			return &server.ArinInfo{
				DatabaseBuildEpoch:   epoch,
				ClassifierVersion:    1,
				QualityPolicyVersion: 2,
				QualityState:         qualityState,
				NonQuality:           qualityState != "subscriber",
				Risk:                 risk,
			}
		}
		return info(testExitArinLatestEpoch), info(testExitArinOldEpoch), nil
	}
}

// Numbers the fixtures' addresses for the rest of the test: the residential
// ISP whose addresses a proxy list resells, the datacenter the slots' users'
// traffic exits from, and the ISP of the address the egress-location tests
// place in Montreal. Any other address has no known system.
func useTestExitAsns(t testing.TB) {
	t.Helper()
	systems := []struct {
		prefix netip.Prefix
		asn    uint32
	}{
		{prefix: netip.MustParsePrefix("198.51.100.0/24"), asn: 64496},
		{prefix: netip.PrefixFrom(netip.MustParseAddr(testEgressCountryExitIp), 24).Masked(), asn: 64497},
		{prefix: netip.PrefixFrom(netip.MustParseAddr(testEgressCityExitIp), 24).Masked(), asn: 64498},
	}
	t.Cleanup(model.Testing_SetProviderNetworkAsn(func(addr netip.Addr) uint32 {
		for _, system := range systems {
			if system.prefix.Contains(addr.Unmap()) {
				return system.asn
			}
		}
		return 0
	}))
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

// The number of days the durable record holds for the client.
func testExitRecordedDays(t testing.TB, ctx context.Context, clientId server.Id) int {
	t.Helper()
	var count int
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`SELECT count(*) FROM provider_egress_exit_day WHERE client_id = $1`,
			clientId,
		).Scan(&count))
	})
	return count
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

// A connection from a residential address, stored before any probe at a city
// of its own in Canada with the address's own facts, as the connect announce
// stored it.
func testExitResidentialConnection(t testing.TB, ctx context.Context, clientId server.Id, ip string, handlerId server.Id) server.Id {
	t.Helper()
	connectionId := testExitConnect(t, ctx, clientId, ip, handlerId)
	city := &model.Location{
		LocationType: model.LocationTypeCity,
		City:         "city-" + clientId.String(),
		Region:       "region-" + clientId.String(),
		Country:      "Canada",
		CountryCode:  "ca",
	}
	model.CreateLocation(ctx, city)
	latest, old, err := arinInfoPairForAddr(netip.MustParseAddr(ip))
	if err != nil {
		t.Fatal(err)
	}
	scores := &model.ConnectionLocationScores{}
	setArinConnectionFacts(scores, latest, old)
	if err := model.SetConnectionLocation(ctx, connectionId, city.LocationId, scores); err != nil {
		t.Fatal(err)
	}
	return connectionId
}

// Submits a probe that saw the exit now.
func testExitSubmit(t testing.TB, ctx context.Context, clientId server.Id, exitIp string) {
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
// another country. Its first probe judges its Quality by the exit, but finds
// no recorded system for the network it connects from, which is no evidence
// that the exit differs. A reconnect from the proxy is judged by the exit and
// records the proxy's system; the next probe finds the exit differs, so the
// slot counts at no unit, its connections are placed in the exit's country,
// and its day is recorded.
func TestSubmitProviderEgressLocationJudgesTheProviderByItsExit(t *testing.T) {
	requireGeoLite2Database(t)

	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		// documentation addresses resolve to no place, whatever the config overrides
		server.UseNoIpOverridesForTest(t)
		const residentialIp = "198.51.100.10"
		useTestExitArinClassifier(t, []netip.Addr{netip.MustParseAddr(residentialIp)})
		useTestExitAsns(t)

		clientId := testEgressProvider(ctx)
		handlerId := model.CreateNetworkClientHandler(ctx)
		connectionId := testExitResidentialConnection(t, ctx, clientId, residentialIp, handlerId)
		proxyCountryLocationId := testExitConnectionLocation(t, ctx, connectionId).countryLocationId
		if row := testExitConnectionLocation(t, ctx, connectionId); row.arinNonQuality || !row.arinQualityVerified {
			t.Fatalf("the fixture's residential connection is no verified subscriber: %+v", row)
		}
		judgedByTheExit := func(what string, connectionId server.Id) testExitLocationRow {
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
			return row
		}

		// the first probe: Quality by the exit, but no evidence of a difference
		testExitSubmit(t, ctx, clientId, testEgressCountryExitIp)
		exitCountryLocationId := testExitProbedCountryLocationId(t, ctx, clientId)
		if exitCountryLocationId == proxyCountryLocationId {
			t.Fatal("the fixture's exit is in the proxy's country")
		}
		if row := judgedByTheExit("the live connection", connectionId); row.countryLocationId != proxyCountryLocationId {
			t.Fatalf("the live connection was placed before any evidence: %+v", row)
		}
		if model.GetVerifyProxiedEgress(ctx, []server.Id{clientId})[clientId] {
			t.Fatal("a slot was marked on its first probe, without evidence")
		}

		// a reconnect from the proxy, judged by the exit, records the proxy's system
		reconnectionId := testExitConnect(t, ctx, clientId, residentialIp, handlerId)
		if err := SetConnectionLocation(ctx, reconnectionId, residentialIp); err != nil {
			t.Fatal(err)
		}
		if row := judgedByTheExit("a reconnect", reconnectionId); row.countryLocationId != exitCountryLocationId {
			t.Fatalf("a reconnect is placed in country %s, want the exit's %s", row.countryLocationId, exitCountryLocationId)
		}

		// the next probe finds the exit differs
		testExitSubmit(t, ctx, clientId, testEgressCountryExitIp)
		if !model.GetVerifyProxiedEgress(ctx, []server.Id{clientId})[clientId] {
			t.Fatal("a slot whose exit differs from its connection is not marked, so it keeps the proxy's unit")
		}
		for _, id := range []server.Id{connectionId, reconnectionId} {
			if row := judgedByTheExit("a live connection", id); row.countryLocationId != exitCountryLocationId {
				t.Fatalf("a live connection is placed in country %s, want the exit's %s", row.countryLocationId, exitCountryLocationId)
			}
		}
		if days := testExitRecordedDays(t, ctx, clientId); days != 1 {
			t.Fatalf("the slot has %d recorded days, want today", days)
		}
	})
}

// A provider whose probe sees its users' traffic leave from its own address is
// unchanged: no mark, no recorded day, the same facts and the same place, on
// its live connection and on a reconnect.
func TestSubmitProviderEgressLocationLeavesADirectProviderAsItWas(t *testing.T) {
	requireGeoLite2Database(t)

	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		useTestExitArinClassifier(t, []netip.Addr{netip.MustParseAddr(testEgressCityExitIp)})
		useTestExitAsns(t)

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

		testExitSubmit(t, ctx, clientId, testEgressCityExitIp)
		// a reconnect records the network's system, and a second probe judges it
		reconnectionId := testExitConnect(t, ctx, clientId, testEgressCityExitIp, handlerId)
		if err := SetConnectionLocation(ctx, reconnectionId, testEgressCityExitIp); err != nil {
			t.Fatal(err)
		}
		testExitSubmit(t, ctx, clientId, testEgressCityExitIp)

		if model.GetVerifyProxiedEgress(ctx, []server.Id{clientId})[clientId] {
			t.Fatal("a provider that exits from its own address is marked")
		}
		judgement := model.GetProviderEgressExitJudgement(ctx, clientId)
		if judgement == nil || judgement.ExitDiffers || judgement.ArinNonQuality || !judgement.ArinQualityVerified || judgement.ArinRisk {
			t.Fatalf("the direct provider's judgement %+v, want its own address's facts", judgement)
		}
		if after := testExitConnectionLocation(t, ctx, connectionId); after != before {
			t.Fatalf("the live connection changed from %+v to %+v", before, after)
		}
		if reconnected := testExitConnectionLocation(t, ctx, reconnectionId); reconnected.arinNonQuality || !reconnected.arinQualityVerified ||
			reconnected.cityLocationId != before.cityLocationId || reconnected.countryLocationId != before.countryLocationId {
			t.Fatalf("a reconnect is %+v, want the live connection's %+v", reconnected, before)
		}
		if days := testExitRecordedDays(t, ctx, clientId); days != 0 {
			t.Fatalf("a provider that exits from its own address has %d recorded days", days)
		}
	})
}

// The probe sees a provider's users' traffic leave from a risky address, a
// VPN's. Its own residential address is clean, but judged by the exit, as if
// it connected from there, its live connection and a reconnect are risky, which
// excludes it from every bucket, Speed and Online included. The exit needs no
// evidence of a difference for that.
func TestSubmitProviderEgressLocationRiskyExitTakesTheProviderOutOfEveryBucket(t *testing.T) {
	requireGeoLite2Database(t)

	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		server.UseNoIpOverridesForTest(t)
		const residentialIp = "198.51.100.11"
		useTestExitArinClassifier(t, []netip.Addr{netip.MustParseAddr(residentialIp)}, netip.MustParseAddr(testEgressCountryExitIp))
		useTestExitAsns(t)

		clientId := testEgressProvider(ctx)
		handlerId := model.CreateNetworkClientHandler(ctx)
		connectionId := testExitResidentialConnection(t, ctx, clientId, residentialIp, handlerId)
		if row := testExitConnectionLocation(t, ctx, connectionId); row.arinRisk {
			t.Fatalf("the fixture's residential connection is risky: %+v", row)
		}

		testExitSubmit(t, ctx, clientId, testEgressCountryExitIp)
		if row := testExitConnectionLocation(t, ctx, connectionId); !row.arinRisk || row.arinQualityVerified {
			t.Fatalf("the live connection of a provider with a risky exit is %+v, want risky", row)
		}
		reconnectionId := testExitConnect(t, ctx, clientId, residentialIp, handlerId)
		if err := SetConnectionLocation(ctx, reconnectionId, residentialIp); err != nil {
			t.Fatal(err)
		}
		if row := testExitConnectionLocation(t, ctx, reconnectionId); !row.arinRisk || row.arinQualityVerified {
			t.Fatalf("a reconnect of a provider with a risky exit is %+v, want risky", row)
		}
	})
}

// The connect announce records the system of the network a probed provider
// connects from, and nothing for a provider without a fresh probe.
func TestSetConnectionLocationRecordsTheSystemOfAProbedProvidersNetwork(t *testing.T) {
	requireGeoLite2Database(t)

	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		server.UseNoIpOverridesForTest(t)
		const residentialIp = "198.51.100.12"
		useTestExitArinClassifier(t, []netip.Addr{netip.MustParseAddr(residentialIp)})
		useTestExitAsns(t)
		handlerId := model.CreateNetworkClientHandler(ctx)

		unprobedId := testEgressProvider(ctx)
		if err := SetConnectionLocation(ctx, testExitConnect(t, ctx, unprobedId, residentialIp, handlerId), residentialIp); err == nil {
			t.Fatal("GeoLite2 placed a documentation address")
		}

		probedId := testEgressProvider(ctx)
		testExitResidentialConnection(t, ctx, probedId, residentialIp, handlerId)
		testExitSubmit(t, ctx, probedId, testEgressCountryExitIp)
		if err := SetConnectionLocation(ctx, testExitConnect(t, ctx, probedId, residentialIp, handlerId), residentialIp); err != nil {
			t.Fatal(err)
		}

		recorded := func(clientId server.Id) int {
			var count int64
			server.Redis(ctx, func(r server.RedisClient) {
				var err error
				count, err = r.HLen(ctx, "{pnet_"+clientId.String()+"}").Result()
				server.Raise(err)
			})
			return int(count)
		}
		if count := recorded(unprobedId); count != 0 {
			t.Fatalf("a provider without a probe has %d recorded networks", count)
		}
		if count := recorded(probedId); count != 1 {
			t.Fatalf("a probed provider has %d recorded networks, want its one", count)
		}
	})
}

// A provider whose exit differs from every network it connects from is placed
// by its probe ahead of its derived location, which refines the lookup of the
// address it connects from. A provider that exits from its own address keeps
// the precedence of connect/GEOMAP.md §6: its derived location first.
func TestSetConnectionLocationPlacesAProviderWhoseExitDiffersByItsProbe(t *testing.T) {
	requireGeoLite2Database(t)

	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		useTestExitArinClassifier(t, []netip.Addr{netip.MustParseAddr(testDerivedClientIp)})
		useTestExitAsns(t)
		tokyo := testDerivedCity(ctx, "Tokyo", "Tokyo", "Japan", "jp")

		slotId, slotConnectionId := testDerivedConnection(t, ctx, testDerivedClientIp)
		directId, directConnectionId := testDerivedConnection(t, ctx, testDerivedClientIp)
		model.ReplaceDerivedLocations(ctx, []*model.DerivedLocation{
			testDerivedRow(model.DerivedLocationNodeKindProvider, slotId, tokyo),
			testDerivedRow(model.DerivedLocationNodeKindProvider, directId, tokyo),
		})
		locate := func(connectionId server.Id) *testConnectionLocation {
			t.Helper()
			if err := SetConnectionLocation(ctx, connectionId, testDerivedClientIp); err != nil {
				t.Fatal(err)
			}
			return readTestConnectionLocation(t, ctx, connectionId)
		}
		// the slot's users' traffic leaves from another network than it
		// connects from; the direct provider's from its own address. The first
		// probes find no recorded system, and the announces after them record it.
		testExitSubmit(t, ctx, slotId, testEgressCountryExitIp)
		testExitSubmit(t, ctx, directId, testDerivedClientIp)
		if slot := locate(slotConnectionId); slot.cityLocationId != tokyo.CityLocationId {
			t.Fatalf("before any evidence the slot is placed at %+v, want its derived city %s", slot, tokyo.CityLocationId)
		}
		locate(directConnectionId)
		testExitSubmit(t, ctx, slotId, testEgressCountryExitIp)
		testExitSubmit(t, ctx, directId, testDerivedClientIp)

		exitCountryLocationId := testExitProbedCountryLocationId(t, ctx, slotId)
		if slot := locate(slotConnectionId); slot.countryLocationId != exitCountryLocationId || slot.genesisLocationId != nil {
			t.Fatalf("the slot is placed at %+v, want its exit's country %s with no genesis, not its derived city %s", slot, exitCountryLocationId, tokyo.CityLocationId)
		}
		if direct := locate(directConnectionId); direct.cityLocationId != tokyo.CityLocationId {
			t.Fatalf("the direct provider is placed at %+v, want its derived city %s", direct, tokyo.CityLocationId)
		}
	})
}

// Placement by a fresh probe: the probe wins whenever the provider's exit
// differs from every network it connects from, however coarse, and otherwise
// only as probedLocationPreferred allows, never coarsening a same-country city.
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
