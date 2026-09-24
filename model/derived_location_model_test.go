package model

import (
	"context"
	"math"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// The derived-location table and the genesis readers of the derive phase
// (connect/GEOMAP.md §5.1, §5.4, §5.7, §6).

// A published row of the given node at a city, with every column set.
func testDerivedLocation(nodeKind int, nodeId server.Id, city *Location, updateTime time.Time) *DerivedLocation {
	return &DerivedLocation{
		NodeKind:          nodeKind,
		NodeId:            nodeId,
		GenesisLatitude:   1.5,
		GenesisLongitude:  -2.5,
		GenesisAccuracyKm: 25,
		DeltaLatitude:     0.25,
		DeltaLongitude:    -0.5,
		Latitude:          1.75,
		Longitude:         -3,
		PingCount:         12,
		PeerCount:         3,
		ResidualKm:        4.5,
		Reputation:        0.75,
		CrossedRegion:     true,
		CrossedCountry:    false,
		LocationId:        city.LocationId,
		CityLocationId:    city.CityLocationId,
		RegionLocationId:  city.RegionLocationId,
		CountryLocationId: city.CountryLocationId,
		UpdateTime:        updateTime,
	}
}

// A derivation replaces the table whole: every row it gives is written,
// whatever was there, and every row it does not give is removed.
func TestReplaceDerivedLocationsReplacesTheTable(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		sydney := testStatsCityLocation(ctx, "Sydney", "New South Wales", "Australia", "au")
		tokyo := testStatsCityLocation(ctx, "Tokyo", "Tokyo", "Japan", "jp")
		now := server.NowUtc()

		a := testDerivedLocation(DerivedLocationNodeKindProvider, server.NewId(), sydney, now)
		b := testDerivedLocation(DerivedLocationNodeKindExtender, server.NewId(), sydney, now)
		written, removed := ReplaceDerivedLocations(ctx, []*DerivedLocation{a, b})
		connect.AssertEqual(t, written, 2)
		connect.AssertEqual(t, removed, 0)

		stored := GetDerivedLocation(ctx, a.NodeKind, a.NodeId)
		if stored == nil {
			t.Fatal("the row was not written")
		}
		connect.AssertEqual(t, stored.CountryCode, "au")
		stored.CountryCode = ""
		connect.AssertEqual(t, stored.UpdateTime.Unix(), now.Unix())
		stored.UpdateTime = a.UpdateTime
		connect.AssertEqual(t, *stored, *a)

		// the next derivation publishes b somewhere else and c for the first
		// time, and not a
		later := now.Add(time.Hour)
		b2 := testDerivedLocation(DerivedLocationNodeKindExtender, b.NodeId, tokyo, later)
		b2.CrossedCountry = true
		c := testDerivedLocation(DerivedLocationNodeKindProvider, server.NewId(), tokyo, later)
		written, removed = ReplaceDerivedLocations(ctx, []*DerivedLocation{b2, c})
		connect.AssertEqual(t, written, 2)
		connect.AssertEqual(t, removed, 1)
		if GetDerivedLocation(ctx, a.NodeKind, a.NodeId) != nil {
			t.Fatal("a row the derivation did not publish survived it")
		}
		updated := GetDerivedLocation(ctx, b.NodeKind, b.NodeId)
		connect.AssertEqual(t, updated.CountryCode, "jp")
		connect.AssertEqual(t, updated.CityLocationId, tokyo.CityLocationId)
		connect.AssertEqual(t, updated.CrossedCountry, true)
		connect.AssertEqual(t, updated.UpdateTime.Unix(), later.Unix())

		all := GetDerivedLocations(ctx)
		connect.AssertEqual(t, len(all), 2)

		counts := CountDerivedLocations(ctx)
		connect.AssertEqual(t, counts.NodeKinds, []DerivedLocationNodeKindCount{
			{NodeKind: DerivedLocationNodeKindProvider, Count: 1},
			{NodeKind: DerivedLocationNodeKindExtender, Count: 1},
		})
		connect.AssertEqual(t, counts.CrossedRegion, int64(2))
		connect.AssertEqual(t, counts.CrossedCountry, int64(1))

		// a derivation that publishes nothing empties the table, and the counts
		// still carry every kind
		written, removed = ReplaceDerivedLocations(ctx, nil)
		connect.AssertEqual(t, written, 0)
		connect.AssertEqual(t, removed, 2)
		counts = CountDerivedLocations(ctx)
		connect.AssertEqual(t, counts.NodeKinds, []DerivedLocationNodeKindCount{
			{NodeKind: DerivedLocationNodeKindProvider, Count: 0},
			{NodeKind: DerivedLocationNodeKindExtender, Count: 0},
		})
	})
}

// The sweep removes a row a day after the derivation that wrote it (§5.7).
func TestRemoveExpiredDerivedLocations(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		sydney := testStatsCityLocation(ctx, "Sydney", "New South Wales", "Australia", "au")
		now := server.NowUtc()
		old := testDerivedLocation(DerivedLocationNodeKindProvider, server.NewId(), sydney, now.Add(-25*time.Hour))
		fresh := testDerivedLocation(DerivedLocationNodeKindProvider, server.NewId(), sydney, now.Add(-time.Hour))
		ReplaceDerivedLocations(ctx, []*DerivedLocation{old, fresh})

		connect.AssertEqual(t, RemoveExpiredDerivedLocations(ctx, now.Add(-24*time.Hour)), 1)
		if GetDerivedLocation(ctx, old.NodeKind, old.NodeId) != nil {
			t.Fatal("the expired row survived the sweep")
		}
		if GetDerivedLocation(ctx, fresh.NodeKind, fresh.NodeId) == nil {
			t.Fatal("the sweep removed a fresh row")
		}
		connect.AssertEqual(t, RemoveExpiredDerivedLocations(ctx, now.Add(-24*time.Hour)), 0)
	})
}

// A connection reads its provider's row, and never an extender's row that
// happens to share the id.
func TestGetDerivedLocationForConnection(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		tokyo := testStatsCityLocation(ctx, "Tokyo", "Tokyo", "Japan", "jp")
		sydney := testStatsCityLocation(ctx, "Sydney", "New South Wales", "Australia", "au")

		clientId := server.NewId()
		Testing_CreateDevice(ctx, server.NewId(), server.NewId(), clientId, "", "")
		handlerId := CreateNetworkClientHandler(ctx)
		connectionId, _, _, _, err := ConnectNetworkClient(ctx, clientId, "192.0.2.1:0", handlerId)
		connect.AssertEqual(t, err, nil)

		if GetDerivedLocationForConnection(ctx, connectionId) != nil {
			t.Fatal("a provider with no row has a derived location")
		}
		ReplaceDerivedLocations(ctx, []*DerivedLocation{
			testDerivedLocation(DerivedLocationNodeKindExtender, clientId, sydney, server.NowUtc()),
		})
		if GetDerivedLocationForConnection(ctx, connectionId) != nil {
			t.Fatal("a connection read an extender's row")
		}
		ReplaceDerivedLocations(ctx, []*DerivedLocation{
			testDerivedLocation(DerivedLocationNodeKindExtender, clientId, sydney, server.NowUtc()),
			testDerivedLocation(DerivedLocationNodeKindProvider, clientId, tokyo, server.NowUtc()),
		})
		derived := GetDerivedLocationForConnection(ctx, connectionId)
		if derived == nil {
			t.Fatal("the provider's row was not read")
		}
		connect.AssertEqual(t, derived.NodeKind, DerivedLocationNodeKindProvider)
		connect.AssertEqual(t, derived.LocationId, tokyo.LocationId)
		connect.AssertEqual(t, derived.CountryCode, "jp")
		if GetDerivedLocationForConnection(ctx, server.NewId()) != nil {
			t.Fatal("an unknown connection has a derived location")
		}
	})
}

// Every place an extender's record is signed reads its derived country (§6),
// and the geo dns sets place it by the same country, so a continent set holds
// the extenders whose records name that continent. An extender without a row
// keeps the country it activated from.
func TestExtenderRecordsCarryTheDerivedCountry(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		tokyo := testStatsCityLocation(ctx, "Tokyo", "Tokyo", "Japan", "jp")

		signedCountries := []string{}
		sign := func(extender *NetworkExtender, addresses []*NetworkExtenderAddress, issueTime time.Time) ([]byte, error) {
			signedCountries = append(signedCountries, extender.RecordCountryCode())
			return []byte("record"), nil
		}
		derivedKey := []byte("derived-country-extender-key-001")
		derived := ActivateNetworkExtender(ctx, testExtenderActivation(derivedKey, 4, "192.0.2.70"), sign)
		plain := ActivateNetworkExtender(ctx, testExtenderActivation([]byte("derived-country-extender-key-002"), 4, "192.0.2.71"), sign)
		if derived == nil || plain == nil {
			t.Fatal("the activations were not stored")
		}
		// the activations had no derived location
		connect.AssertEqual(t, signedCountries, []string{"US", "US"})

		ReplaceDerivedLocations(ctx, []*DerivedLocation{
			testDerivedLocation(DerivedLocationNodeKindExtender, derived.Extender.ExtenderId, tokyo, server.NowUtc()),
			// a provider row of the same id says nothing about the extender
			testDerivedLocation(DerivedLocationNodeKindProvider, plain.Extender.ExtenderId, tokyo, server.NowUtc()),
		})

		// the dns txt signer's read
		extender, addresses := GetActiveNetworkExtenderForRecord(ctx, derived.Extender.ExtenderId)
		if extender == nil || len(addresses) != 1 {
			t.Fatal("the extender is not active")
		}
		connect.AssertEqual(t, extender.DerivedCountryCode, "jp")
		connect.AssertEqual(t, extender.CountryCode, "US")
		connect.AssertEqual(t, extender.RecordCountryCode(), "jp")
		extender, _ = GetActiveNetworkExtenderForRecord(ctx, plain.Extender.ExtenderId)
		connect.AssertEqual(t, extender.RecordCountryCode(), "US")

		// the drip
		signedCountries = []string{}
		connect.AssertEqual(t, PublishNetworkExtenderRecord(ctx, derived.Extender.ExtenderId, sign), true)
		connect.AssertEqual(t, PublishNetworkExtenderRecord(ctx, plain.Extender.ExtenderId, sign), true)
		connect.AssertEqual(t, signedCountries, []string{"jp", "US"})

		// a re-activation signs with the derived country, and the activation
		// row it writes keeps where the address resolved to
		signedCountries = []string{}
		reactivated := ActivateNetworkExtender(ctx, testExtenderActivation(derivedKey, 4, "192.0.2.70"), sign)
		connect.AssertEqual(t, signedCountries, []string{"jp"})
		connect.AssertEqual(t, reactivated.Extender.CountryCode, "US")

		// the bootstrap sample
		for _, entry := range GetRandomActiveNetworkExtenders(ctx, 64, server.Id{}) {
			switch entry.Extender.ExtenderId {
			case derived.Extender.ExtenderId:
				connect.AssertEqual(t, entry.Extender.RecordCountryCode(), "jp")
			case plain.Extender.ExtenderId:
				connect.AssertEqual(t, entry.Extender.RecordCountryCode(), "US")
			}
		}

		// the geo dns sets
		dnsCountries := map[server.Id]string{}
		for _, address := range GetActiveNetworkExtenderDnsAddresses(ctx) {
			dnsCountries[address.ExtenderId] = address.CountryCode
		}
		connect.AssertEqual(t, dnsCountries[derived.Extender.ExtenderId], "jp")
		connect.AssertEqual(t, dnsCountries[plain.Extender.ExtenderId], "US")
	})
}

// The genesis readers take the whole node set at once: each extender's latest
// activation, each provider's fresh probe, each provider's newest connected
// location, and the location rows they name.
func TestDeriveGenesisReaders(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		sydney := testStatsCityLocation(ctx, "Sydney", "New South Wales", "Australia", "au")
		tokyo := testStatsCityLocation(ctx, "Tokyo", "Tokyo", "Japan", "jp")
		sign, _ := testRecordSigner()

		// two activations of one extender: the later one is its genesis
		publicKey := []byte("derive-genesis-extender-key-0001")
		first := testExtenderActivation(publicKey, 4, "192.0.2.80")
		firstAccuracyKm := float32(50)
		first.AccuracyKm = &firstAccuracyKm
		activated := ActivateNetworkExtender(ctx, first.WithLocation(sydney), sign)
		second := testExtenderActivation(publicKey, 6, "2001:db8::80")
		secondAccuracyKm := float32(5)
		second.AccuracyKm = &secondAccuracyKm
		ActivateNetworkExtender(ctx, second.WithLocation(tokyo), sign)
		extenderId := activated.Extender.ExtenderId
		activations := GetLatestNetworkExtenderActivations(ctx, []server.Id{extenderId, server.NewId()})
		connect.AssertEqual(t, len(activations), 1)
		connect.AssertEqual(t, *activations[extenderId].LocationId, tokyo.LocationId)
		connect.AssertEqual(t, *activations[extenderId].AccuracyKm, secondAccuracyKm)
		connect.AssertEqual(t, len(GetLatestNetworkExtenderActivations(ctx, nil)), 0)

		// a fresh probe and a stale one
		freshClientId := server.NewId()
		staleClientId := server.NewId()
		now := server.NowUtc()
		SetProviderEgressLocation(ctx, &ProviderEgressLocation{
			ClientId:      freshClientId,
			LocationId:    tokyo.LocationId,
			CountryCode:   "JP",
			CityConfident: true,
			ObservedAt:    now.Add(-time.Hour),
		})
		SetProviderEgressLocation(ctx, &ProviderEgressLocation{
			ClientId:    staleClientId,
			LocationId:  tokyo.CountryLocationId,
			CountryCode: "jp",
			ObservedAt:  now.Add(-(ProviderEgressLocationMaxAge + time.Hour)),
		})
		probes := GetFreshProviderEgressLocations(ctx, []server.Id{freshClientId, staleClientId}, now.Add(-ProviderEgressLocationMaxAge))
		connect.AssertEqual(t, len(probes), 1)
		connect.AssertEqual(t, probes[freshClientId].LocationId, tokyo.LocationId)
		connect.AssertEqual(t, probes[freshClientId].CityConfident, true)
		connect.AssertEqual(t, probes[freshClientId].CountryCode, "jp")

		// a provider connected twice: the connection that is connected is its
		// genesis, whichever is newer
		clientId := server.NewId()
		Testing_CreateDevice(ctx, server.NewId(), server.NewId(), clientId, "", "")
		handlerId := CreateNetworkClientHandler(ctx)
		connectedId, _, _, _, err := ConnectNetworkClient(ctx, clientId, "192.0.2.81:0", handlerId)
		connect.AssertEqual(t, err, nil)
		accuracyKm := float32(20)
		connect.AssertEqual(t, SetConnectionLocation(ctx, connectedId, sydney.LocationId, &ConnectionLocationScores{
			AccuracyKm:        &accuracyKm,
			GenesisLocationId: &sydney.LocationId,
		}), nil)
		disconnectedId, _, _, _, err := ConnectNetworkClient(ctx, clientId, "192.0.2.82:0", handlerId)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, SetConnectionLocation(ctx, disconnectedId, tokyo.LocationId, &ConnectionLocationScores{}), nil)
		connect.AssertEqual(t, DisconnectNetworkClient(ctx, disconnectedId), nil)

		connections := GetConnectionGenesisLocations(ctx, []server.Id{clientId, server.NewId()})
		connect.AssertEqual(t, len(connections), 1)
		connection := connections[clientId]
		connect.AssertEqual(t, connection.CityLocationId, sydney.CityLocationId)
		connect.AssertEqual(t, connection.CountryLocationId, sydney.CountryLocationId)
		connect.AssertEqual(t, *connection.GenesisLocationId, sydney.LocationId)
		connect.AssertEqual(t, *connection.AccuracyKm, accuracyKm)

		// the rows they name, with their hierarchy, in one read
		locations := GetLocations(ctx, []server.Id{sydney.LocationId, tokyo.CountryLocationId, server.NewId()})
		connect.AssertEqual(t, len(locations), 2)
		connect.AssertEqual(t, locations[sydney.LocationId].LocationType, LocationTypeCity)
		connect.AssertEqual(t, locations[sydney.LocationId].Region, "New South Wales")
		connect.AssertEqual(t, locations[sydney.LocationId].CountryCode, "au")
		connect.AssertEqual(t, locations[tokyo.CountryLocationId].LocationType, LocationTypeCountry)
		connect.AssertEqual(t, locations[tokyo.CountryLocationId].Region, "")
		connect.AssertEqual(t, len(GetLocations(ctx, nil)), 0)
	})
}

// A relayed co-signed ping is an attestation, not a term: counted per pinger
// and target, apart from the direct ones.
func TestGetNetworkPingRelayedCosignCounts(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()
		pingerId := server.NewId()
		targetId := server.NewId()
		ping := func(cosign int, hopCount int, createTime time.Time) *NetworkPing {
			return &NetworkPing{
				PingerKind:       NetworkPingPingerKindExtender,
				PingerId:         pingerId,
				TargetExtenderId: targetId,
				ProbeNonce:       server.NewId().Bytes(),
				RttMs:            30,
				ProbeTime:        createTime,
				Cosign:           cosign,
				PingerSignature:  []byte("pinger-signature"),
				Cosignature:      []byte("cosignature"),
				HopCount:         hopCount,
				CreateTime:       createTime,
			}
		}
		connect.AssertEqual(t, AddNetworkPings(ctx, []*NetworkPing{
			ping(NetworkPingCosignCosigned, 1, now),
			ping(NetworkPingCosignCosigned, 2, now),
			// direct, refused, and out of the window: none of them counted
			ping(NetworkPingCosignCosigned, 0, now),
			ping(NetworkPingCosignRejected, 1, now),
			ping(NetworkPingCosignCosigned, 1, now.Add(-25*time.Hour)),
		}), 5)

		counts := []NetworkPingAttestationCount{}
		GetNetworkPingRelayedCosignCounts(ctx, now.Add(-24*time.Hour), func(count *NetworkPingAttestationCount) {
			counts = append(counts, *count)
		})
		connect.AssertEqual(t, counts, []NetworkPingAttestationCount{{
			PingerKind:       NetworkPingPingerKindExtender,
			PingerId:         pingerId,
			TargetExtenderId: targetId,
			Count:            2,
		}})

		// and the direct one is the only term
		terms := []*NetworkPingTerm{}
		GetNetworkPingTerms(ctx, now.Add(-24*time.Hour), func(term *NetworkPingTerm) {
			terms = append(terms, term)
		})
		connect.AssertEqual(t, len(terms), 1)
	})
}

// The history keeps the latest runs newest first, trimmed to
// DeriveLocationsRunHistory, and its head is the last derivation the stats
// collector publishes; the monitor compares a run with the ones before it
// (SIGNALS.md §2.19c).
func TestDeriveLocationsRunHistory(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		if _, ok := GetDeriveLocationsRun(ctx); ok {
			t.Fatal("a run is recorded before any derivation")
		}
		connect.AssertEqual(t, len(GetDeriveLocationsRuns(ctx, DeriveLocationsRunHistory)), 0)

		start := time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)
		for i := 0; i < DeriveLocationsRunHistory+2; i += 1 {
			AddDeriveLocationsRun(ctx, &DeriveLocationsRun{
				RunTime:           start.Add(time.Duration(i) * 8 * time.Hour),
				Nodes:             1100 + i,
				Sources:           1000 + i,
				Terms:             33000,
				CosignedPings:     90000 + i,
				Published:         640 + i,
				ExcludedSources:   i,
				ResidualKm:        12.5,
				GenesisResidualKm: 48.25,
				Converged:         i%2 == 0,
				LastRoundSweeps:   7,
				SweepCap:          100,
			})
		}
		last := DeriveLocationsRunHistory + 1
		run, ok := GetDeriveLocationsRun(ctx)
		if !ok {
			t.Fatal("the last run was not read back")
		}
		connect.AssertEqual(t, *run, DeriveLocationsRun{
			RunTime:           start.Add(time.Duration(last) * 8 * time.Hour),
			Nodes:             1100 + last,
			Sources:           1000 + last,
			Terms:             33000,
			CosignedPings:     90000 + last,
			Published:         640 + last,
			ExcludedSources:   last,
			ResidualKm:        12.5,
			GenesisResidualKm: 48.25,
			Converged:         last%2 == 0,
			LastRoundSweeps:   7,
			SweepCap:          100,
		})

		runs := GetDeriveLocationsRuns(ctx, 3)
		connect.AssertEqual(t, len(runs), 3)
		for i, run := range runs {
			connect.AssertEqual(t, run.Nodes, 1100+last-i)
		}
		// trimmed to the history, however many are asked for
		connect.AssertEqual(t, len(GetDeriveLocationsRuns(ctx, 2*DeriveLocationsRunHistory)), DeriveLocationsRunHistory)
		connect.AssertEqual(t, len(GetDeriveLocationsRuns(ctx, 0)), 0)
	})
}

// A connection location stores the lookup's genesis beside the published
// location (§6), and a connection without one stores NULL.
func TestSetConnectionLocationStoresTheGenesisLocation(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		sydney := testStatsCityLocation(ctx, "Sydney", "New South Wales", "Australia", "au")
		tokyo := testStatsCityLocation(ctx, "Tokyo", "Tokyo", "Japan", "jp")

		clientId := server.NewId()
		Testing_CreateDevice(ctx, server.NewId(), server.NewId(), clientId, "", "")
		handlerId := CreateNetworkClientHandler(ctx)
		connectionId, _, _, _, err := ConnectNetworkClient(ctx, clientId, "192.0.2.90:0", handlerId)
		connect.AssertEqual(t, err, nil)

		genesisLocationId := func() *server.Id {
			var locationId *server.Id
			server.Db(ctx, func(conn server.PgConn) {
				result, err := conn.Query(
					ctx,
					`SELECT genesis_location_id FROM network_client_location WHERE connection_id = $1`,
					connectionId,
				)
				server.WithPgResult(result, err, func() {
					if result.Next() {
						server.Raise(result.Scan(&locationId))
					}
				})
			})
			return locationId
		}

		connect.AssertEqual(t, SetConnectionLocation(ctx, connectionId, tokyo.LocationId, &ConnectionLocationScores{
			GenesisLocationId: &sydney.LocationId,
		}), nil)
		stored := genesisLocationId()
		if stored == nil || *stored != sydney.LocationId {
			t.Fatalf("the genesis stored as %v, want %s", stored, sydney.LocationId)
		}
		// an update without one clears it, as a probed location stores none
		connect.AssertEqual(t, SetConnectionLocation(ctx, connectionId, tokyo.LocationId, &ConnectionLocationScores{}), nil)
		if stored := genesisLocationId(); stored != nil {
			t.Fatalf("a location without a genesis stored %s", *stored)
		}
	})
}

// The migrated table and column exist with the shapes the model writes.
func TestDerivedLocationMigrationsApply(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		columns := []string{}
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(
				ctx,
				`
				SELECT table_name || '.' || column_name
				FROM information_schema.columns
				WHERE
					table_schema = current_schema() AND
					(table_name = 'derived_location' OR (table_name = 'network_client_location' AND column_name = 'genesis_location_id'))
				`,
			)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var column string
					server.Raise(result.Scan(&column))
					columns = append(columns, column)
				}
			})
		})
		connect.AssertEqual(t, len(columns), 21)
		if !slices.Contains(columns, "network_client_location.genesis_location_id") {
			t.Fatalf("columns %v", columns)
		}
		// the location columns are inventoried for the de-duplication job
		unknown := []string{}
		missing := []string{}
		server.Tx(ctx, func(tx server.PgTx) {
			unknown, missing = locationReferenceDriftInTx(ctx, tx)
		})
		connect.AssertEqual(t, unknown, []string(nil))
		connect.AssertEqual(t, missing, []string(nil))
	})
}

// The pinger-id hash ranges cover the int64 space exactly once, contiguous
// and in order, for any count. Pure.
func TestNetworkPingHashRangesCoverTheSpace(t *testing.T) {
	for _, count := range []int{1, 2, 3, 4, 16, 17, 64} {
		ranges := NetworkPingHashRanges(count)
		connect.AssertEqual(t, len(ranges), count)
		connect.AssertEqual(t, ranges[0].Lo, int64(math.MinInt64))
		connect.AssertEqual(t, ranges[count-1].Hi, int64(math.MaxInt64))
		for i := 1; i < count; i += 1 {
			if ranges[i-1].Hi+1 != ranges[i].Lo || !(ranges[i].Lo <= ranges[i].Hi) {
				t.Fatalf("%d ranges: %+v then %+v", count, ranges[i-1], ranges[i])
			}
		}
	}
	connect.AssertEqual(t, NetworkPingHashRanges(0), NetworkPingHashRanges(1))
}
