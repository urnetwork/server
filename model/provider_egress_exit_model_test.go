// The root cause (owner decision 2026-11-20, "judge by exit IP"): a provider
// was judged by the address it connects to the platform from. A miner proxy
// slot that connects from a residential proxy but whose users' traffic exits
// from a datacenter was rated a verified subscriber, published at the proxy's
// city, and counted the proxy's /29 as a head unit. Judged by its exit, its
// live connections carry the datacenter's Quality facts and the probe's
// place, and it counts at no unit; a provider that exits from its own network
// is unchanged. Fixtures use RFC 5737 and RFC 3849 documentation addresses in
// the fixture's isolated Redis and Postgres.
package model

import (
	"context"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// The score epoch of the fixtures, and its window.
const testProviderExitScoreEpoch = 9002

// A provider connected from one address, at a city of its own in Canada, with
// the facts of a verified residential subscriber.
type testProviderExitProvider struct {
	clientId     server.Id
	connectionId server.Id
	ip           netip.Addr
	location     *Location
}

func testProviderExitConnect(t testing.TB, ctx context.Context, ip string) *testProviderExitProvider {
	t.Helper()
	provider := &testProviderExitProvider{
		clientId: server.NewId(),
		ip:       netip.MustParseAddr(ip),
	}
	Testing_CreateDevice(ctx, server.NewId(), server.NewId(), provider.clientId, "", "")
	provider.connectionId = testProviderExitConnection(t, ctx, provider.clientId, ip)
	provider.location = &Location{
		LocationType: LocationTypeCity,
		City:         "city-" + provider.clientId.String(),
		Region:       "region-" + provider.clientId.String(),
		Country:      "country-ca",
		CountryCode:  "ca",
	}
	CreateLocation(ctx, provider.location)
	scores := testProviderExitResidentialScores()
	genesisLocationId := provider.location.LocationId
	scores.GenesisLocationId = &genesisLocationId
	accuracyKm := float32(5)
	scores.AccuracyKm = &accuracyKm
	if err := SetConnectionLocation(ctx, provider.connectionId, provider.location.LocationId, &scores); err != nil {
		t.Fatal(err)
	}
	return provider
}

// A new connection of the client from the address.
func testProviderExitConnection(t testing.TB, ctx context.Context, clientId server.Id, ip string) server.Id {
	t.Helper()
	connectionId, _, _, _, err := ConnectNetworkClient(ctx, clientId, net.JoinHostPort(ip, "40000"), CreateNetworkClientHandler(ctx))
	if err != nil {
		t.Fatal(err)
	}
	return connectionId
}

// The country-granular place a probe stores for an exit GeoLite2 places only
// in Canada.
func testProviderExitCountryPlace(ctx context.Context) *Location {
	place := &Location{
		LocationType: LocationTypeCountry,
		Country:      "country-ca",
		CountryCode:  "ca",
	}
	CreateLocation(ctx, place)
	return place
}

// The judgement of an exit with the given facts, observed now.
func testProviderExitJudgementNow(judgement *ProviderEgressExitJudgement) ProviderEgressExitJudgement {
	observed := *judgement
	observed.ObservedAt = server.NowUtc()
	observed.ArinLookupAt = observed.ObservedAt
	return observed
}

// The judgement of an exit that is a verified residential subscriber, as a
// direct provider's own address is.
func testProviderExitResidentialJudgement() *ProviderEgressExitJudgement {
	return &ProviderEgressExitJudgement{
		ArinQualityVerified:       true,
		ArinDatabaseBuildEpoch:    testProviderExitLatestEpoch,
		ArinOldQualityVerified:    true,
		ArinOldDatabaseBuildEpoch: testProviderExitOldEpoch,
	}
}

// One connection's location row, as the judgement rewrites it.
type testProviderExitRow struct {
	cityLocationId         server.Id
	countryLocationId      server.Id
	genesisLocationId      *server.Id
	accuracyKm             *float32
	arinRisk               bool
	arinNonQuality         bool
	arinQualityVerified    bool
	arinEpoch              int64
	arinOldNonQuality      bool
	arinOldQualityVerified bool
	arinOldEpoch           int64
}

func testProviderExitLocationRow(t testing.TB, ctx context.Context, connectionId server.Id) testProviderExitRow {
	t.Helper()
	row := testProviderExitRow{}
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`
			SELECT
				city_location_id,
				country_location_id,
				genesis_location_id,
				accuracy_km,
				arin_risk,
				arin_non_quality,
				arin_quality_verified,
				arin_database_build_epoch,
				arin_old_non_quality,
				arin_old_quality_verified,
				arin_old_database_build_epoch
			FROM network_client_location
			WHERE connection_id = $1
			`,
			connectionId,
		).Scan(
			&row.cityLocationId,
			&row.countryLocationId,
			&row.genesisLocationId,
			&row.accuracyKm,
			&row.arinRisk,
			&row.arinNonQuality,
			&row.arinQualityVerified,
			&row.arinEpoch,
			&row.arinOldNonQuality,
			&row.arinOldQualityVerified,
			&row.arinOldEpoch,
		))
	})
	return row
}

// The exit differs from the connections exactly when it is in none of their
// networks: the /29 of an IPv4 address, the /56 of an IPv6 one. Without a
// connection there is nothing it differs from. In the test env for the
// address hash pepper.
func TestProviderEgressExitDiffersOutsideEveryConnectionNetwork(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		hashes := func(ips ...string) [][32]byte {
			connectionAddressHashes := [][32]byte{}
			for _, ip := range ips {
				connectionAddressHashes = append(connectionAddressHashes, server.ClientIpHashForAddr(netip.MustParseAddr(ip)))
			}
			return connectionAddressHashes
		}
		cases := []struct {
			exitIp        string
			connectionIps []string
			differs       bool
		}{
			// the exit is the connection's own address
			{exitIp: "192.0.2.17", connectionIps: []string{"192.0.2.17"}, differs: false},
			// another address of the connection's /29
			{exitIp: "192.0.2.22", connectionIps: []string{"192.0.2.17"}, differs: false},
			// the next /29
			{exitIp: "192.0.2.25", connectionIps: []string{"192.0.2.17"}, differs: true},
			// a residential proxy connection and a datacenter exit
			{exitIp: "203.0.113.9", connectionIps: []string{"198.51.100.8"}, differs: true},
			// one of a dual-stack provider's connections is in the exit's network
			{exitIp: "203.0.113.9", connectionIps: []string{"2001:db8:0:1::9", "203.0.113.10"}, differs: false},
			// an IPv4 exit of a provider that connects over IPv6 alone
			{exitIp: "203.0.113.9", connectionIps: []string{"2001:db8:0:1::9"}, differs: true},
			// IPv6: the same /56, then another /56
			{exitIp: "2001:db8:0:2::5", connectionIps: []string{"2001:db8:0:1::9"}, differs: false},
			{exitIp: "2001:db8:0:100::5", connectionIps: []string{"2001:db8:0:1::9"}, differs: true},
			// a mapped exit is its IPv4 address
			{exitIp: "::ffff:192.0.2.17", connectionIps: []string{"192.0.2.17"}, differs: false},
			// no connection to compare
			{exitIp: "203.0.113.9", connectionIps: nil, differs: false},
		}
		for _, c := range cases {
			if differs := providerEgressExitDiffers(netip.MustParseAddr(c.exitIp), hashes(c.connectionIps...)); differs != c.differs {
				t.Errorf("exit %s, connections %v: differs %t, want %t", c.exitIp, c.connectionIps, differs, c.differs)
			}
		}
	})
}

// A slot that connects from a residential proxy and exits from a datacenter
// is judged by the datacenter: its live connection carries the datacenter's
// Quality facts and the probe's place, and it is marked, so it shows no SN Top
// board pin and no capture the flag unit. A direct provider and a provider
// whose exit is another address of its own /29 keep their facts, their place
// and their units.
func TestJudgeProviderEgressExitByTheExitNotTheConnection(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := testVerifyFamilySettings("provider-egress-exit-unit-test")
		window := CtfVerifyHeadWindow(testProviderExitScoreEpoch)

		slot := testProviderExitConnect(t, ctx, "198.51.100.8")
		direct := testProviderExitConnect(t, ctx, "192.0.2.17")
		neighbor := testProviderExitConnect(t, ctx, "192.0.2.33")
		providers := []*testProviderExitProvider{slot, direct, neighbor}
		all := []server.Id{}
		for _, provider := range providers {
			all = append(all, provider.clientId)
			testVerifySetProvideModes(ctx, provider.clientId)
			FeedVerifyConnectionEgress(ctx, provider.clientId, provider.connectionId, provider.ip, settings)
		}
		place := testProviderExitCountryPlace(ctx)

		slotJudged, latest := JudgeProviderEgressExit(ctx, slot.clientId, netip.MustParseAddr("203.0.113.9"),
			testProviderExitJudgementNow(testProviderExitDatacenterJudgement()), place.LocationId, time.Minute)
		if !latest || !slotJudged.ExitDiffers {
			t.Fatalf("the slot's datacenter exit: latest %t, differs %t; want both", latest, slotJudged.ExitDiffers)
		}
		directJudged, latest := JudgeProviderEgressExit(ctx, direct.clientId, direct.ip,
			testProviderExitJudgementNow(testProviderExitResidentialJudgement()), place.LocationId, time.Minute)
		if !latest || directJudged.ExitDiffers {
			t.Fatalf("the direct provider's own exit: latest %t, differs %t; want latest only", latest, directJudged.ExitDiffers)
		}
		neighborJudged, latest := JudgeProviderEgressExit(ctx, neighbor.clientId, netip.MustParseAddr("192.0.2.38"),
			testProviderExitJudgementNow(testProviderExitResidentialJudgement()), place.LocationId, time.Minute)
		if !latest || neighborJudged.ExitDiffers {
			t.Fatalf("an exit in the provider's own /29: latest %t, differs %t; want latest only", latest, neighborJudged.ExitDiffers)
		}

		// the slot's live connection is judged by the datacenter and placed by
		// the probe; its own risk stays
		slotRow := testProviderExitLocationRow(t, ctx, slot.connectionId)
		if !slotRow.arinNonQuality || slotRow.arinQualityVerified || slotRow.arinEpoch != testProviderExitLatestEpoch {
			t.Fatalf("the slot's connection was not judged by its exit: %+v", slotRow)
		}
		if !slotRow.arinOldNonQuality || slotRow.arinOldQualityVerified || slotRow.arinOldEpoch != testProviderExitOldEpoch {
			t.Fatalf("the slot's connection kept its own old facts: %+v", slotRow)
		}
		if slotRow.arinRisk {
			t.Fatalf("the judgement changed the slot's risk: %+v", slotRow)
		}
		if slotRow.cityLocationId != place.CountryLocationId || slotRow.countryLocationId != place.CountryLocationId ||
			slotRow.genesisLocationId != nil || slotRow.accuracyKm != nil {
			t.Fatalf("the slot stayed at its proxy's city %s: %+v, want the probe's country %s", slot.location.CityLocationId, slotRow, place.CountryLocationId)
		}

		// the others are as they were
		for _, provider := range []*testProviderExitProvider{direct, neighbor} {
			row := testProviderExitLocationRow(t, ctx, provider.connectionId)
			if row.arinNonQuality || !row.arinQualityVerified || row.arinEpoch != testProviderExitLatestEpoch || !row.arinOldQualityVerified {
				t.Fatalf("a provider that exits from its own network lost its facts: %+v", row)
			}
			if row.cityLocationId != provider.location.CityLocationId || row.genesisLocationId == nil || *row.genesisLocationId != provider.location.LocationId {
				t.Fatalf("a provider that exits from its own network was coarsened: %+v", row)
			}
		}

		marked := GetVerifyProxiedEgress(ctx, all)
		if len(marked) != 1 || !marked[slot.clientId] {
			t.Fatalf("marked %v, want the slot alone", marked)
		}
		pins := getVerifyHeadPrefixPins(ctx, all, settings, window, true)
		if _, ok := pins[slot.clientId]; ok || len(pins) != 2 ||
			pins[direct.clientId] != VerifyEgressIpHashWithSettings(direct.ip, settings) ||
			pins[neighbor.clientId] != VerifyEgressIpHashWithSettings(neighbor.ip, settings) {
			t.Fatalf("board pins %v, want the direct provider's and the neighbor's /29 alone", pins)
		}
		egresses := GetCtfProviderEgress(ctx, all, settings, testProviderExitScoreEpoch, true)
		if egress := egresses[slot.clientId]; egress == nil || egress.Unit != nil || !egress.Proxied {
			t.Fatalf("capture the flag gave the slot %+v, want no unit", egress)
		}
		if egress := egresses[direct.clientId]; egress == nil || egress.Unit == nil || *egress.Unit != VerifyEgressIpHashWithSettings(direct.ip, settings) || egress.Proxied {
			t.Fatalf("capture the flag gave the direct provider %+v, want its /29", egress)
		}
		if recorded := PinVerifyHopEgressIpHash(ctx, slot.clientId, slot.ip, window, false, settings); recorded != ([32]byte{}) {
			t.Fatalf("the slot's hop recorded %x, want the zero hash", recorded)
		}
	})
}

// The connections compared are the ones live at the probe, allowing the
// prober's clock skew: a provider whose address changed just after the probe
// still exits from its own network, while one whose last connection from the
// exit closed long before the probe does not.
func TestJudgeProviderEgressExitComparesTheConnectionsLiveAtTheProbe(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		place := testProviderExitCountryPlace(ctx)
		observedAt := server.NowUtc().Add(-10 * time.Minute)
		closeAt := func(connectionId server.Id, disconnectTime time.Time) {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(
					ctx,
					`UPDATE network_client_connection SET connected = false, disconnect_time = $2 WHERE connection_id = $1`,
					connectionId,
					disconnectTime,
				))
			})
		}

		// the address changed half a minute after the probe saw it
		renewed := testProviderExitConnect(t, ctx, "192.0.2.41")
		closeAt(renewed.connectionId, observedAt.Add(30*time.Second))
		testProviderExitConnection(t, ctx, renewed.clientId, "192.0.2.65")
		judgement := testProviderExitJudgementNow(testProviderExitResidentialJudgement())
		judgement.ObservedAt = observedAt
		if judged, _ := JudgeProviderEgressExit(ctx, renewed.clientId, renewed.ip, judgement, place.LocationId, time.Minute); judged.ExitDiffers {
			t.Fatal("a provider whose address changed after the probe differs from its exit")
		}

		// the connection from the exit's network closed an hour before the probe
		stale := testProviderExitConnect(t, ctx, "192.0.2.81")
		closeAt(stale.connectionId, observedAt.Add(-time.Hour))
		testProviderExitConnection(t, ctx, stale.clientId, "198.51.100.90")
		if judged, _ := JudgeProviderEgressExit(ctx, stale.clientId, stale.ip, judgement, place.LocationId, time.Minute); !judged.ExitDiffers {
			t.Fatal("a connection that closed an hour before the probe counted as live at it")
		}
	})
}

// The judgement of the latest probe is kept for the connect announce while the
// probe is fresh. An older probe's changes nothing; a replay of the latest is
// judged again.
func TestJudgeProviderEgressExitKeepsTheLatestProbe(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		provider := testProviderExitConnect(t, ctx, "198.51.100.120")
		place := testProviderExitCountryPlace(ctx)
		exitIp := netip.MustParseAddr("203.0.113.120")

		if GetProviderEgressExitJudgement(ctx, provider.clientId) != nil {
			t.Fatal("a provider never judged has a judgement")
		}
		later := testProviderExitJudgementNow(testProviderExitDatacenterJudgement())
		if _, latest := JudgeProviderEgressExit(ctx, provider.clientId, exitIp, later, place.LocationId, time.Minute); !latest {
			t.Fatal("the first judgement was not kept")
		}
		earlier := testProviderExitJudgementNow(testProviderExitResidentialJudgement())
		earlier.ObservedAt = later.ObservedAt.Add(-time.Minute)
		if _, latest := JudgeProviderEgressExit(ctx, provider.clientId, exitIp, earlier, place.LocationId, time.Minute); latest {
			t.Fatal("an older probe's judgement replaced a later one")
		}
		kept := GetProviderEgressExitJudgement(ctx, provider.clientId)
		if kept == nil || !kept.ObservedAt.Equal(later.ObservedAt) || !kept.ArinNonQuality || !kept.ExitDiffers {
			t.Fatalf("kept %+v, want the later probe's", kept)
		}
		if _, latest := JudgeProviderEgressExit(ctx, provider.clientId, exitIp, later, place.LocationId, time.Minute); !latest {
			t.Fatal("a replay of the latest probe was not judged again")
		}
		server.Redis(ctx, func(r server.RedisClient) {
			ttl, err := r.PTTL(ctx, providerEgressExitKey(provider.clientId)).Result()
			server.Raise(err)
			if ttl <= 0 || ProviderEgressLocationMaxAge < ttl {
				t.Fatalf("the judgement lives %s, want the probe's freshness", ttl)
			}
		})
		// the older probe left the connection judged by the later one
		row := testProviderExitLocationRow(t, ctx, provider.connectionId)
		if !row.arinNonQuality || row.arinQualityVerified {
			t.Fatalf("an older probe rejudged the connection: %+v", row)
		}
	})
}
