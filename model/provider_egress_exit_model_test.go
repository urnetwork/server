// The root cause (owner decisions 2026-11-20, "judge by exit IP" and "apply
// exit risk too"): a provider was judged by the address it connects to the
// platform from. A miner proxy slot that connects from a residential proxy but
// whose users' traffic exits from a datacenter was rated a verified
// subscriber, published at the proxy's city, and counted the proxy's /29 as a
// head unit. Judged by its exit, its live connections carry the datacenter's
// Quality facts, the exit's risk and the probe's place, it counts at no unit,
// and its day is recorded for the settlement epoch. The exit differs only on
// positive evidence, so a carrier NAT, a second line of one ISP and a provider
// that connects over IPv6 while its exit is IPv4 are spared, as is a provider
// whose networks have no recorded system. Fixtures use RFC 5737 and RFC 3849
// documentation addresses and RFC 5398 documentation systems in the fixture's
// isolated Redis and Postgres.
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

// The fixtures' autonomous systems.
const (
	// the residential ISP whose addresses a proxy list resells
	testProviderExitResidentialAsn = uint32(64496)
	// the datacenter the slots' users' traffic exits from
	testProviderExitDatacenterAsn = uint32(64497)
	// an ISP with a carrier NAT and IPv6
	testProviderExitCarrierAsn = uint32(64498)
	// an IPv6 proxy list
	testProviderExitIpv6ProxyAsn = uint32(64499)
)

// Numbers the fixtures' addresses for the rest of the test. Any other address
// has no known system.
func useTestProviderExitAsns(t testing.TB) {
	t.Helper()
	systems := []struct {
		prefix netip.Prefix
		asn    uint32
	}{
		{prefix: netip.MustParsePrefix("198.51.100.0/24"), asn: testProviderExitResidentialAsn},
		{prefix: netip.MustParsePrefix("203.0.113.0/24"), asn: testProviderExitDatacenterAsn},
		{prefix: netip.MustParsePrefix("192.0.2.0/24"), asn: testProviderExitCarrierAsn},
		{prefix: netip.MustParsePrefix("2001:db8::/48"), asn: testProviderExitCarrierAsn},
		{prefix: netip.MustParsePrefix("2001:db8:ffff::/48"), asn: testProviderExitIpv6ProxyAsn},
	}
	t.Cleanup(Testing_SetProviderNetworkAsn(func(addr netip.Addr) uint32 {
		for _, system := range systems {
			if system.prefix.Contains(addr.Unmap()) {
				return system.asn
			}
		}
		return 0
	}))
}

// A provider connected from one address, at a city of its own in Canada, with
// the facts of a verified residential subscriber.
type testProviderExitProvider struct {
	clientId     server.Id
	networkId    server.Id
	connectionId server.Id
	ip           netip.Addr
	location     *Location
}

func testProviderExitConnect(t testing.TB, ctx context.Context, ip string) *testProviderExitProvider {
	t.Helper()
	provider := &testProviderExitProvider{
		clientId:  server.NewId(),
		networkId: server.NewId(),
		ip:        netip.MustParseAddr(ip),
	}
	Testing_CreateDevice(ctx, provider.networkId, server.NewId(), provider.clientId, "", "")
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

// A provider connected from one address whose network's system the connect
// announce recorded, as it does for a connection after the provider's first
// probe.
func testProviderExitConnectRecorded(t testing.TB, ctx context.Context, ip string) *testProviderExitProvider {
	t.Helper()
	provider := testProviderExitConnect(t, ctx, ip)
	SetProviderConnectionNetworkAsn(ctx, provider.clientId, ip)
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
	arinOldRisk            bool
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
				arin_old_risk,
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
			&row.arinOldRisk,
			&row.arinOldNonQuality,
			&row.arinOldQualityVerified,
			&row.arinOldEpoch,
		))
	})
	return row
}

// One client's day in the durable record.
type testProviderExitDay struct {
	networkId        server.Id
	differsCount     int
	crossFamilyCount int
	firstObservedAt  time.Time
	lastObservedAt   time.Time
}

// The client's days in the durable record, by date.
func testProviderExitDays(t testing.TB, ctx context.Context, clientId server.Id) map[string]testProviderExitDay {
	t.Helper()
	days := map[string]testProviderExitDay{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				to_char(day, 'YYYY-MM-DD'),
				network_id,
				differs_count,
				cross_family_count,
				first_observed_at,
				last_observed_at
			FROM provider_egress_exit_day
			WHERE client_id = $1
			`,
			clientId,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var dayText string
				day := testProviderExitDay{}
				server.Raise(result.Scan(
					&dayText,
					&day.networkId,
					&day.differsCount,
					&day.crossFamilyCount,
					&day.firstObservedAt,
					&day.lastObservedAt,
				))
				days[dayText] = day
			}
		})
	})
	return days
}

// A slot that connects from a residential proxy and exits from a datacenter
// is judged by the datacenter: its live connection carries the datacenter's
// Quality facts and the probe's place, it is marked, so it shows no SN Top
// board pin and no capture the flag unit, and its day is recorded. So is a
// slot that connects through an IPv6 proxy list. A direct provider, one whose
// exit is another address of its own /29, a carrier NAT's subscriber whose exit
// is another network of its ISP, a provider that connects over its ISP's IPv6
// while the probe exits over IPv4, and a provider whose network has no
// recorded system keep their facts, their place and their units.
func TestJudgeProviderEgressExitByTheExitNotTheConnection(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		useTestProviderExitAsns(t)
		settings := testVerifyFamilySettings("provider-egress-exit-unit-test")
		window := CtfVerifyHeadWindow(testProviderExitScoreEpoch)

		slot := testProviderExitConnectRecorded(t, ctx, "198.51.100.8")
		ipv6Slot := testProviderExitConnectRecorded(t, ctx, "2001:db8:ffff:1::5")
		direct := testProviderExitConnectRecorded(t, ctx, "192.0.2.17")
		neighbor := testProviderExitConnectRecorded(t, ctx, "192.0.2.33")
		carrier := testProviderExitConnectRecorded(t, ctx, "192.0.2.49")
		dualStack := testProviderExitConnectRecorded(t, ctx, "2001:db8:0:1::9")
		unrecorded := testProviderExitConnect(t, ctx, "198.51.100.130")
		providers := []*testProviderExitProvider{slot, ipv6Slot, direct, neighbor, carrier, dualStack, unrecorded}
		all := []server.Id{}
		for _, provider := range providers {
			all = append(all, provider.clientId)
			testVerifySetProvideModes(ctx, provider.clientId)
			FeedVerifyConnectionEgress(ctx, provider.clientId, provider.connectionId, provider.ip, settings)
		}
		place := testProviderExitCountryPlace(ctx)

		judge := func(provider *testProviderExitProvider, exitIp string, judgement *ProviderEgressExitJudgement) ProviderEgressExitJudgement {
			t.Helper()
			judged, latest := JudgeProviderEgressExit(ctx, provider.clientId, netip.MustParseAddr(exitIp),
				testProviderExitJudgementNow(judgement), place.LocationId, time.Minute)
			if !latest {
				t.Fatalf("the judgement of the exit %s was not the latest", exitIp)
			}
			return judged
		}
		for _, c := range []struct {
			name      string
			provider  *testProviderExitProvider
			exitIp    string
			judgement *ProviderEgressExitJudgement
			differs   bool
		}{
			{name: "the slot's datacenter exit", provider: slot, exitIp: "203.0.113.9", judgement: testProviderExitDatacenterJudgement(), differs: true},
			{name: "the IPv6 slot's datacenter exit", provider: ipv6Slot, exitIp: "203.0.113.10", judgement: testProviderExitDatacenterJudgement(), differs: true},
			{name: "the direct provider's own exit", provider: direct, exitIp: "192.0.2.17", judgement: testProviderExitResidentialJudgement(), differs: false},
			{name: "an exit in the provider's own /29", provider: neighbor, exitIp: "192.0.2.38", judgement: testProviderExitResidentialJudgement(), differs: false},
			{name: "a carrier NAT's other network", provider: carrier, exitIp: "192.0.2.70", judgement: testProviderExitResidentialJudgement(), differs: false},
			{name: "an IPv4 exit of an ISP the provider reaches over IPv6", provider: dualStack, exitIp: "192.0.2.90", judgement: testProviderExitResidentialJudgement(), differs: false},
			{name: "an exit of a provider whose network has no recorded system", provider: unrecorded, exitIp: "203.0.113.130", judgement: testProviderExitDatacenterJudgement(), differs: false},
		} {
			if judged := judge(c.provider, c.exitIp, c.judgement); judged.ExitDiffers != c.differs {
				t.Errorf("%s: differs %t, want %t", c.name, judged.ExitDiffers, c.differs)
			}
		}

		// the slots' live connections are judged by the datacenter and placed
		// by the probe; their own risk stays
		for _, provider := range []*testProviderExitProvider{slot, ipv6Slot} {
			row := testProviderExitLocationRow(t, ctx, provider.connectionId)
			if !row.arinNonQuality || row.arinQualityVerified || row.arinEpoch != testProviderExitLatestEpoch {
				t.Fatalf("a slot's connection was not judged by its exit: %+v", row)
			}
			if !row.arinOldNonQuality || row.arinOldQualityVerified || row.arinOldEpoch != testProviderExitOldEpoch {
				t.Fatalf("a slot's connection kept its own old facts: %+v", row)
			}
			if row.arinRisk {
				t.Fatalf("a clean exit made a slot risky: %+v", row)
			}
			if row.cityLocationId != place.CountryLocationId || row.countryLocationId != place.CountryLocationId ||
				row.genesisLocationId != nil || row.accuracyKm != nil {
				t.Fatalf("a slot stayed at its proxy's city %s: %+v, want the probe's country %s", provider.location.CityLocationId, row, place.CountryLocationId)
			}
		}
		// the others keep their place; an exit in the provider's own system is
		// its residential subscriber, so its facts stand
		for _, provider := range []*testProviderExitProvider{direct, neighbor, carrier, dualStack} {
			row := testProviderExitLocationRow(t, ctx, provider.connectionId)
			if row.arinNonQuality || !row.arinQualityVerified || row.arinEpoch != testProviderExitLatestEpoch || !row.arinOldQualityVerified {
				t.Fatalf("a provider that exits from its own network lost its facts: %+v", row)
			}
			if row.cityLocationId != provider.location.CityLocationId || row.genesisLocationId == nil || *row.genesisLocationId != provider.location.LocationId {
				t.Fatalf("a provider that exits from its own network was placed by the probe: %+v", row)
			}
		}
		// without evidence the provider is not placed by the probe, though its
		// Quality is still its exit's
		if row := testProviderExitLocationRow(t, ctx, unrecorded.connectionId); row.cityLocationId != unrecorded.location.CityLocationId || !row.arinNonQuality {
			t.Fatalf("the unrecorded provider: %+v, want its own city and its exit's Quality", row)
		}

		marked := GetVerifyProxiedEgress(ctx, all)
		if len(marked) != 2 || !marked[slot.clientId] || !marked[ipv6Slot.clientId] {
			t.Fatalf("marked %v, want the two slots alone", marked)
		}
		pins := getVerifyHeadPrefixPins(ctx, all, settings, window, true)
		for _, provider := range []*testProviderExitProvider{slot, ipv6Slot} {
			if _, ok := pins[provider.clientId]; ok {
				t.Fatalf("a slot has the board pin %x", pins[provider.clientId])
			}
		}
		for _, provider := range []*testProviderExitProvider{direct, neighbor, carrier, unrecorded} {
			if pins[provider.clientId] != VerifyEgressIpHashWithSettings(provider.ip, settings) {
				t.Fatalf("board pins %v: a provider that exits from its own network lost its /29", pins)
			}
		}
		egresses := GetCtfProviderEgress(ctx, all, settings, testProviderExitScoreEpoch, true)
		if egress := egresses[slot.clientId]; egress == nil || egress.Unit != nil || !egress.Proxied {
			t.Fatalf("capture the flag gave the slot %+v, want no unit", egress)
		}
		if egress := egresses[carrier.clientId]; egress == nil || egress.Unit == nil || *egress.Unit != VerifyEgressIpHashWithSettings(carrier.ip, settings) || egress.Proxied {
			t.Fatalf("capture the flag gave the carrier NAT's subscriber %+v, want its /29", egress)
		}
		if recorded := PinVerifyHopEgressIpHash(ctx, slot.clientId, slot.ip, window, false, settings); recorded != ([32]byte{}) {
			t.Fatalf("the slot's hop recorded %x, want the zero hash", recorded)
		}

		// the differing days are recorded, the cross-family one as such
		today := server.NowUtc().Format(time.DateOnly)
		if day, ok := testProviderExitDays(t, ctx, slot.clientId)[today]; !ok || day.differsCount != 1 || day.crossFamilyCount != 0 || day.networkId != slot.networkId {
			t.Fatalf("the slot's day %+v (recorded %t), want one differing judgement in its network", day, ok)
		}
		if day, ok := testProviderExitDays(t, ctx, ipv6Slot.clientId)[today]; !ok || day.differsCount != 1 || day.crossFamilyCount != 1 {
			t.Fatalf("the IPv6 slot's day %+v (recorded %t), want one cross-family differing judgement", day, ok)
		}
		for _, provider := range []*testProviderExitProvider{direct, neighbor, carrier, dualStack, unrecorded} {
			if days := testProviderExitDays(t, ctx, provider.clientId); len(days) != 0 {
				t.Fatalf("a provider whose exit does not differ has days %v", days)
			}
		}
	})
}

// Only a known system is recorded for a network, by the connection row's own
// address hash.
func TestSetProviderConnectionNetworkAsnRecordsKnownSystems(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		useTestProviderExitAsns(t)
		clientId := server.NewId()
		SetProviderConnectionNetworkAsn(ctx, clientId, "198.51.100.8")
		SetProviderConnectionNetworkAsn(ctx, clientId, "2001:db8:ffff:1::5")
		// no known system
		SetProviderConnectionNetworkAsn(ctx, clientId, "2001:db8:1::5")
		SetProviderConnectionNetworkAsn(ctx, clientId, "not-an-address")
		asns := getProviderConnectionNetworkAsns(ctx, clientId)
		residentialHash, err := server.ClientIpHash("198.51.100.8")
		if err != nil {
			t.Fatal(err)
		}
		ipv6ProxyHash, err := server.ClientIpHash("2001:db8:ffff:1::5")
		if err != nil {
			t.Fatal(err)
		}
		if len(asns) != 2 || asns[residentialHash] != testProviderExitResidentialAsn || asns[ipv6ProxyHash] != testProviderExitIpv6ProxyAsn {
			t.Fatalf("recorded %v, want the two known systems alone", asns)
		}
		server.Redis(ctx, func(r server.RedisClient) {
			ttl, err := r.PTTL(ctx, providerConnectionNetworkAsnKey(clientId)).Result()
			server.Raise(err)
			if ttl <= 0 || ProviderEgressLocationMaxAge < ttl {
				t.Fatalf("the record lives %s, want the probe's freshness", ttl)
			}
		})
	})
}

// The networks compared are the ones live at the probe, allowing the
// prober's clock skew: a provider whose address changed just after the probe
// still exits from its own network, while one whose last connection from the
// exit closed long before the probe does not.
func TestJudgeProviderEgressExitComparesTheConnectionsLiveAtTheProbe(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		useTestProviderExitAsns(t)
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
		judgement := testProviderExitJudgementNow(testProviderExitResidentialJudgement())
		judgement.ObservedAt = observedAt

		// the address changed half a minute after the probe saw it
		renewed := testProviderExitConnectRecorded(t, ctx, "198.51.100.41")
		closeAt(renewed.connectionId, observedAt.Add(30*time.Second))
		testProviderExitConnection(t, ctx, renewed.clientId, "203.0.113.65")
		SetProviderConnectionNetworkAsn(ctx, renewed.clientId, "203.0.113.65")
		if judged, _ := JudgeProviderEgressExit(ctx, renewed.clientId, renewed.ip, judgement, place.LocationId, time.Minute); judged.ExitDiffers {
			t.Fatal("a provider whose address changed after the probe differs from its exit")
		}

		// the connection from the exit's network closed an hour before the probe
		stale := testProviderExitConnectRecorded(t, ctx, "198.51.100.81")
		closeAt(stale.connectionId, observedAt.Add(-time.Hour))
		testProviderExitConnection(t, ctx, stale.clientId, "203.0.113.90")
		SetProviderConnectionNetworkAsn(ctx, stale.clientId, "203.0.113.90")
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
		useTestProviderExitAsns(t)
		provider := testProviderExitConnectRecorded(t, ctx, "198.51.100.120")
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
		// the replay counted the day once
		if day := testProviderExitDays(t, ctx, provider.clientId)[later.ObservedAt.UTC().Format(time.DateOnly)]; day.differsCount != 1 {
			t.Fatalf("the day %+v, want the probe counted once", day)
		}
	})
}

// Each UTC day of differing judgements is one row per client: the day's
// differing probes are counted once each, from the first to the last, and a
// replay counts nothing.
func TestJudgeProviderEgressExitRecordsDifferingDays(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		useTestProviderExitAsns(t)
		provider := testProviderExitConnectRecorded(t, ctx, "198.51.100.150")
		place := testProviderExitCountryPlace(ctx)
		exitIp := netip.MustParseAddr("203.0.113.150")
		judgeAt := func(observedAt time.Time) {
			t.Helper()
			judgement := testProviderExitJudgementNow(testProviderExitDatacenterJudgement())
			judgement.ObservedAt = observedAt
			if judged, latest := JudgeProviderEgressExit(ctx, provider.clientId, exitIp, judgement, place.LocationId, time.Minute); !latest || !judged.ExitDiffers {
				t.Fatalf("the probe at %s: latest %t, differs %t; want both", observedAt, latest, judged.ExitDiffers)
			}
		}

		today := server.NowUtc().Truncate(24 * time.Hour)
		yesterdayProbe := today.Add(-2 * time.Hour)
		firstProbe := today.Add(time.Second)
		secondProbe := today.Add(2 * time.Second)
		judgeAt(yesterdayProbe)
		judgeAt(firstProbe)
		judgeAt(secondProbe)
		judgeAt(secondProbe)

		days := testProviderExitDays(t, ctx, provider.clientId)
		if len(days) != 2 {
			t.Fatalf("days %v, want yesterday and today", days)
		}
		yesterday := days[yesterdayProbe.Format(time.DateOnly)]
		if yesterday.differsCount != 1 || !yesterday.firstObservedAt.Equal(yesterdayProbe) || !yesterday.lastObservedAt.Equal(yesterdayProbe) {
			t.Fatalf("yesterday %+v, want its one probe", yesterday)
		}
		todayRow := days[today.Format(time.DateOnly)]
		if todayRow.differsCount != 2 || todayRow.crossFamilyCount != 0 || todayRow.networkId != provider.networkId ||
			!todayRow.firstObservedAt.Equal(firstProbe) || !todayRow.lastObservedAt.Equal(secondProbe) {
			t.Fatalf("today %+v, want its two probes once each", todayRow)
		}
	})
}

// A risky exit makes the provider's live connection risky, as if it connected
// from there, though its own residential address is clean.
func TestJudgeProviderEgressExitRiskyExitMakesLiveConnectionsRisky(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		useTestProviderExitAsns(t)
		provider := testProviderExitConnect(t, ctx, "198.51.100.160")
		place := testProviderExitCountryPlace(ctx)
		riskyExit := testProviderExitJudgementNow(testProviderExitDatacenterJudgement())
		riskyExit.ArinRisk = true
		riskyExit.ArinOldRisk = true
		JudgeProviderEgressExit(ctx, provider.clientId, netip.MustParseAddr("203.0.113.160"), riskyExit, place.LocationId, time.Minute)
		row := testProviderExitLocationRow(t, ctx, provider.connectionId)
		if !row.arinRisk || !row.arinOldRisk || row.arinQualityVerified || row.arinOldQualityVerified {
			t.Fatalf("a risky exit left the live connection %+v", row)
		}
	})
}
