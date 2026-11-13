package work

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/geo/solve"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// GEOMAP §5.6 test 4, "No pings": a node with no samples at all, a node with
// fewer than MinDerivePings, or with pings to a single peer, converges to its
// genesis exactly and is never published; a derived row it had from an
// earlier run is removed by the next run, and by the sweep if no run comes;
// and the read precedence of §6 then answers the egress probe or the lookup
// again.

// A derived row far from wherever the node's genesis is, as a stale earlier
// run would have left it.
func deriveTestStaleRow(nodeKind int, nodeId server.Id, location *model.Location, updateTime time.Time) *model.DerivedLocation {
	return &model.DerivedLocation{
		NodeKind:          nodeKind,
		NodeId:            nodeId,
		GenesisLatitude:   location.Latitude,
		GenesisLongitude:  location.Longitude,
		GenesisAccuracyKm: 1000,
		DeltaLatitude:     3,
		DeltaLongitude:    3,
		Latitude:          location.Latitude + 3,
		Longitude:         location.Longitude + 3,
		PingCount:         40,
		PeerCount:         4,
		ResidualKm:        2,
		Reputation:        1,
		CrossedRegion:     true,
		LocationId:        location.LocationId,
		CityLocationId:    location.CityLocationId,
		RegionLocationId:  location.RegionLocationId,
		CountryLocationId: location.CountryLocationId,
		UpdateTime:        updateTime,
	}
}

// The plan: a node with fewer than MinDerivePings, one with pings to a single
// peer, and one whose only pings name a node without a genesis are none of
// them published, and the last, which has no term the solve can use, sits at
// its genesis exactly although it is warm started from a previous row hundreds
// of km away: with only the genesis term its minimum is the genesis, and the
// solver's undamped step reaches it. A node with no pings at all is not
// solved. The rest of the graph publishes as before. Pure.
func TestPlanDerivationNoPings(t *testing.T) {
	settings := deriveTestSettings()
	settings.LambdaRegion = 0
	settings.LambdaCountry = 0
	graph := newDeriveTestGraph(t, settings)
	rows := graph.pings(8)

	// Each extra provider is placed by a wide lookup at Westport, 270 km from
	// where it is: wrong enough that a correction would improve on it, so only
	// the publish gates keep it at genesis, and on no peer's own coordinates,
	// where a distance has no gradient to start the solve from.
	westport := deriveTestCity(t, graph.places, 9006)
	provider := func(name string) (string, server.Id) {
		id := server.NewId()
		nodeId := deriveNodeId(model.DerivedLocationNodeKindProvider, id)
		graph.geneses[nodeId] = &deriveGenesis{
			source:          deriveGenesisSourceConnection,
			level:           solve.GenesisLevelCity,
			position:        deriveTestLatLon(westport),
			radiusKm:        1000,
			countryCode:     westport.CountryCode,
			region:          westport.Region,
			regionGeonameId: westport.RegionGeonameId,
		}
		return nodeId, id
	}
	// the extra providers sit where the region provider truly is, and ping
	// with the round trips that place implies, so reputation has nothing
	// against them and only the publish gates decide
	truth := graph.truth[deriveNodeId(model.DerivedLocationNodeKindProvider, graph.providers["region"])]
	add := func(pingerId server.Id, targetId server.Id, samples int) {
		rttMs := 30
		if targetTruth, ok := graph.truth[deriveNodeId(model.DerivedLocationNodeKindExtender, targetId)]; ok {
			rttMs = deriveTestRttMs(settings, truth, targetTruth)
		}
		for range samples {
			rows = append(rows, &model.NetworkPingTerm{
				PingerKind:       model.NetworkPingPingerKindProvider,
				PingerId:         pingerId,
				TargetExtenderId: targetId,
				RttMs:            rttMs,
			})
		}
	}
	// two pings, to two peers: under MinDerivePings
	few, fewId := provider("few")
	add(fewId, graph.extenders["west"], 1)
	add(fewId, graph.extenders["east"], 1)
	// many pings, to one peer
	onePeer, onePeerId := provider("one-peer")
	add(onePeerId, graph.extenders["west"], 8)
	// pings only toward an extender the operator has no genesis for
	stranded, strandedId := provider("stranded")
	add(strandedId, server.NewId(), 8)
	// no pings at all
	silent, _ := provider("silent")
	inputs := deriveTestInputs(settings, rows)

	previousDerivedLocations := map[string]*model.DerivedLocation{}
	for _, nodeId := range []string{few, onePeer, stranded, silent} {
		genesis := graph.geneses[nodeId].position
		previousDerivedLocations[nodeId] = &model.DerivedLocation{
			// hundreds of km off
			Latitude:  genesis.Latitude + 3,
			Longitude: genesis.Longitude + 3,
		}
	}
	plan := planDerivation(inputs, graph.geneses, previousDerivedLocations, graph.places, settings)

	published := map[string]bool{}
	for _, publication := range plan.publications {
		published[publication.nodeResult.Id] = true
	}
	for name, nodeId := range map[string]string{"few": few, "one peer": onePeer, "stranded": stranded, "silent": silent} {
		if published[nodeId] {
			t.Fatalf("%s was published", name)
		}
	}
	// the graph's own providers still publish
	for name, id := range graph.providers {
		if !published[deriveNodeId(model.DerivedLocationNodeKindProvider, id)] {
			t.Fatalf("the %s provider is no longer published", name)
		}
	}

	fewResult := plan.result.Node(few)
	connect.AssertEqual(t, fewResult.PingCount, 2)
	connect.AssertEqual(t, fewResult.PeerCount, 2)
	onePeerResult := plan.result.Node(onePeer)
	connect.AssertEqual(t, onePeerResult.PingCount, 8)
	connect.AssertEqual(t, onePeerResult.PeerCount, 1)

	// exactly at genesis: no correction at all, not a remnant of the warm
	// start stopped at the step tolerance
	strandedNode := plan.nodes[slices.IndexFunc(plan.nodes, func(node solve.Node) bool { return node.Id == stranded })]
	if !(400 < strandedNode.PreviousCorrection.LengthKm()) {
		t.Fatalf("the stranded node was warm started %.1f km from genesis, not from its previous row", strandedNode.PreviousCorrection.LengthKm())
	}
	strandedResult := plan.result.Node(stranded)
	if strandedResult == nil {
		t.Fatal("the stranded node was not solved")
	}
	connect.AssertEqual(t, strandedResult.Correction, solve.Offset{})
	connect.AssertEqual(t, strandedResult.PingCount, 0)
	if distanceKm := solve.DistanceKm(strandedResult.Position, graph.geneses[stranded].position); !(distanceKm < 1e-9) {
		t.Fatalf("the stranded node sits %g km from its genesis", distanceKm)
	}
	if plan.result.Node(silent) != nil {
		t.Fatal("a node with no pings was solved")
	}
	t.Logf(
		"few: %d pings %d peers; one peer: %d pings %d peers; stranded: warm started %.1f km off, correction %+v; published %d; refusals %+v",
		fewResult.PingCount, fewResult.PeerCount, onePeerResult.PingCount, onePeerResult.PeerCount,
		strandedNode.PreviousCorrection.LengthKm(), strandedResult.Correction, len(plan.publications), plan.result.PublishRefusals,
	)
	// the gates say why: two pings is too few, one peer too few peers, and
	// the stranded node has no pings
	summary := plan.summary(inputs)
	if summary.PublishRefusals.FewPings < 2 || summary.PublishRefusals.FewPeers < 1 {
		t.Fatalf("refusals %+v", summary.PublishRefusals)
	}
}

// The city and country ids stored for one connection.
func deriveTestConnectionLocation(t testing.TB, ctx context.Context, connectionId server.Id) (cityLocationId server.Id, countryLocationId server.Id) {
	t.Helper()
	found := false
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT city_location_id, country_location_id FROM network_client_location WHERE connection_id = $1`,
			connectionId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				found = true
				server.Raise(result.Scan(&cityLocationId, &countryLocationId))
			}
		})
	})
	if !found {
		t.Fatalf("connection %s has no location", connectionId)
	}
	return cityLocationId, countryLocationId
}

// An address the GeoLite2 build the test environment deploys resolves (to
// Canada), so a connection from it has a lookup to fall back to; the
// egress-location tests use the same one. A documentation address would
// resolve to nothing, and no other public address belongs in test data.
const deriveTestClientIp = "24.48.0.1"

// A provider connected from deriveTestClientIp, with the lookup's location
// created.
func deriveTestLookupProvider(t testing.TB, ctx context.Context) (clientId server.Id, connectionId server.Id, lookup *model.Location) {
	t.Helper()
	clientIp := deriveTestClientIp
	lookup, _, err := controller.GetLocationForIp(ctx, clientIp)
	if err != nil {
		t.Fatal(err)
	}
	// the fixture is only meaningful while the address resolves
	connect.AssertEqual(t, lookup.CountryCode, "ca")
	model.CreateLocation(ctx, lookup)
	clientId = server.NewId()
	model.Testing_CreateDevice(ctx, server.NewId(), server.NewId(), clientId, "", "")
	handlerId := model.CreateNetworkClientHandler(ctx)
	connectionId, _, _, _, err = model.ConnectNetworkClient(ctx, clientId, clientIp+":0", handlerId)
	if err != nil {
		t.Fatal(err)
	}
	return clientId, connectionId, lookup
}

// The read path after a derived row goes: the lookup, and with a fresh
// city-confident probe the probe, never the old derived place.
func assertDeriveTestReadPathFallsBack(t testing.TB, ctx context.Context, clientId server.Id, connectionId server.Id, lookup *model.Location, derived *model.Location) {
	t.Helper()
	connect.AssertEqual(t, controller.SetConnectionLocation(ctx, connectionId, deriveTestClientIp), nil)
	cityLocationId, countryLocationId := deriveTestConnectionLocation(t, ctx, connectionId)
	connect.AssertEqual(t, countryLocationId, lookup.CountryLocationId)
	if cityLocationId == derived.CityLocationId || countryLocationId == derived.CountryLocationId {
		t.Fatal("the connection was stored at the old derived place")
	}

	tokyo := &model.Location{
		LocationType: model.LocationTypeCity,
		City:         "Tokyo",
		Region:       "Tokyo",
		Country:      "Japan",
		CountryCode:  "jp",
	}
	model.CreateLocation(ctx, tokyo)
	model.SetProviderEgressLocation(ctx, &model.ProviderEgressLocation{
		ClientId:      clientId,
		LocationId:    tokyo.LocationId,
		CountryCode:   "jp",
		CityConfident: true,
		ObservedAt:    server.NowUtc(),
	})
	connect.AssertEqual(t, controller.SetConnectionLocation(ctx, connectionId, deriveTestClientIp), nil)
	cityLocationId, countryLocationId = deriveTestConnectionLocation(t, ctx, connectionId)
	connect.AssertEqual(t, cityLocationId, tokyo.CityLocationId)
	connect.AssertEqual(t, countryLocationId, tokyo.CountryLocationId)
}

// (a) A node with a row from an earlier run and no co-signed pings in the
// window: the next run publishes nothing for it and removes its row, and its
// connection is then stored at the lookup, or a fresh probe, never at the old
// derived place. Before the run the row is what the connection reads, which is
// what makes the after meaningful.
func TestDeriveLocationsRemovesTheRowOfANodeWithNoPings(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer model.Testing_PushPlaces(deriveTestPlaces)()
		places := model.CurrentPlaces()
		settings := deriveTestSettings()
		graph := newDeriveTestGraph(t, settings)
		graph.places = places
		now := server.NowUtc()
		locations := seedDeriveTestGraph(t, ctx, graph, 8, now)

		clientId, connectionId, lookup := deriveTestLookupProvider(t, ctx)
		middle := locations[9002]
		model.ReplaceDerivedLocations(ctx, []*model.DerivedLocation{
			deriveTestStaleRow(model.DerivedLocationNodeKindProvider, clientId, middle, now.Add(-8*time.Hour)),
		})
		connect.AssertEqual(t, controller.SetConnectionLocation(ctx, connectionId, deriveTestClientIp), nil)
		cityLocationId, _ := deriveTestConnectionLocation(t, ctx, connectionId)
		connect.AssertEqual(t, cityLocationId, middle.CityLocationId)

		result, err := deriveLocations(ctx, places, settings, DefaultDeriveLocationsSettings(), now)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%+v", result)
		if model.GetDerivedLocation(ctx, model.DerivedLocationNodeKindProvider, clientId) != nil {
			t.Fatal("the row of a node with no pings survived the run")
		}
		connect.AssertEqual(t, result.Removed, 1)
		// the graph itself still publishes
		if model.GetDerivedLocation(ctx, model.DerivedLocationNodeKindProvider, graph.providers["country"]) == nil {
			t.Fatal("the graph's provider was not published")
		}

		assertDeriveTestReadPathFallsBack(t, ctx, clientId, connectionId, lookup, middle)
	})
}

// (b) A node with fewer than MinDerivePings, and one with pings to a single
// peer: not published, and the rows they had are removed.
func TestDeriveLocationsDoesNotPublishThinEvidence(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer model.Testing_PushPlaces(deriveTestPlaces)()
		places := model.CurrentPlaces()
		settings := deriveTestSettings()
		graph := newDeriveTestGraph(t, settings)
		graph.places = places
		now := server.NowUtc()
		locations := seedDeriveTestGraph(t, ctx, graph, 8, now)
		middle := locations[9002]

		thin := func(targets map[string]int) server.Id {
			clientId := server.NewId()
			model.Testing_CreateDevice(ctx, server.NewId(), server.NewId(), clientId, "", "")
			handlerId := model.CreateNetworkClientHandler(ctx)
			connectionId, _, _, _, err := model.ConnectNetworkClient(ctx, clientId, "192.0.2.98:0", handlerId)
			if err != nil {
				t.Fatal(err)
			}
			accuracyKm := float32(1000)
			connect.AssertEqual(t, model.SetConnectionLocation(ctx, connectionId, middle.LocationId, &model.ConnectionLocationScores{
				AccuracyKm:        &accuracyKm,
				GenesisLocationId: &middle.LocationId,
			}), nil)
			pings := []*model.NetworkPing{}
			for name, samples := range targets {
				for range samples {
					pings = append(pings, &model.NetworkPing{
						PingerKind:       model.NetworkPingPingerKindProvider,
						PingerId:         clientId,
						TargetExtenderId: graph.extenders[name],
						ProbeNonce:       server.NewId().Bytes(),
						RttMs:            30,
						ProbeTime:        now,
						Cosign:           model.NetworkPingCosignCosigned,
						PingerSignature:  []byte("pinger-signature"),
						Cosignature:      []byte("cosignature"),
						CreateTime:       now,
					})
				}
			}
			connect.AssertEqual(t, model.AddNetworkPings(ctx, pings), len(pings))
			return clientId
		}
		few := thin(map[string]int{"west": 1, "east": 1})
		onePeer := thin(map[string]int{"west": 8})
		model.ReplaceDerivedLocations(ctx, []*model.DerivedLocation{
			deriveTestStaleRow(model.DerivedLocationNodeKindProvider, few, middle, now.Add(-8*time.Hour)),
			deriveTestStaleRow(model.DerivedLocationNodeKindProvider, onePeer, middle, now.Add(-8*time.Hour)),
		})

		result, err := deriveLocations(ctx, places, settings, DefaultDeriveLocationsSettings(), now)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%+v", result)
		connect.AssertEqual(t, result.Nodes, 8)
		connect.AssertEqual(t, result.Removed, 2)
		for name, clientId := range map[string]server.Id{"few": few, "one peer": onePeer} {
			if model.GetDerivedLocation(ctx, model.DerivedLocationNodeKindProvider, clientId) != nil {
				t.Fatalf("the %s provider was published", name)
			}
		}
	})
}

// (c) A node warm started from a stale row whose only pings name a node
// without a genesis has no term to solve on: the solve returns it to its
// genesis, it is refused publication for too few pings, and its row is
// removed rather than rewritten with the stale correction.
func TestDeriveLocationsLeavesAStrandedNodeAtGenesis(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer model.Testing_PushPlaces(deriveTestPlaces)()
		places := model.CurrentPlaces()
		settings := deriveTestSettings()
		graph := newDeriveTestGraph(t, settings)
		graph.places = places
		now := server.NowUtc()
		locations := seedDeriveTestGraph(t, ctx, graph, 8, now)
		middle := locations[9002]

		clientId := server.NewId()
		model.Testing_CreateDevice(ctx, server.NewId(), server.NewId(), clientId, "", "")
		handlerId := model.CreateNetworkClientHandler(ctx)
		connectionId, _, _, _, err := model.ConnectNetworkClient(ctx, clientId, "192.0.2.97:0", handlerId)
		connect.AssertEqual(t, err, nil)
		accuracyKm := float32(1000)
		connect.AssertEqual(t, model.SetConnectionLocation(ctx, connectionId, middle.LocationId, &model.ConnectionLocationScores{
			AccuracyKm:        &accuracyKm,
			GenesisLocationId: &middle.LocationId,
		}), nil)
		// an extender that pings arrive for but that never activated here
		unactivated := server.NewId()
		pings := []*model.NetworkPing{}
		for range 8 {
			pings = append(pings, &model.NetworkPing{
				PingerKind:       model.NetworkPingPingerKindProvider,
				PingerId:         clientId,
				TargetExtenderId: unactivated,
				ProbeNonce:       server.NewId().Bytes(),
				RttMs:            30,
				ProbeTime:        now,
				Cosign:           model.NetworkPingCosignCosigned,
				PingerSignature:  []byte("pinger-signature"),
				Cosignature:      []byte("cosignature"),
				CreateTime:       now,
			})
		}
		connect.AssertEqual(t, model.AddNetworkPings(ctx, pings), len(pings))
		stale := deriveTestStaleRow(model.DerivedLocationNodeKindProvider, clientId, middle, now.Add(-8*time.Hour))
		model.ReplaceDerivedLocations(ctx, []*model.DerivedLocation{stale})

		result, err := deriveLocations(ctx, places, settings, DefaultDeriveLocationsSettings(), now)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%+v", result)
		// the stranded provider is solved, the unactivated extender is not
		connect.AssertEqual(t, result.Nodes, 7)
		connect.AssertEqual(t, result.NodesWithoutGenesis, 1)
		connect.AssertEqual(t, result.Removed, 1)
		if result.PublishRefusals.FewPings < 1 {
			t.Fatalf("no node was refused for too few pings: %+v", result.PublishRefusals)
		}
		if model.GetDerivedLocation(ctx, model.DerivedLocationNodeKindProvider, clientId) != nil {
			t.Fatal("the stranded provider's row was rewritten")
		}
		for _, row := range model.GetDerivedLocations(ctx) {
			if row.DeltaLatitude == stale.DeltaLatitude && row.DeltaLongitude == stale.DeltaLongitude {
				t.Fatal("a row carries the stale correction")
			}
		}
	})
}

// (d) No run at all: the hourly sweep removes a derived row older than
// PingRetention, and the read path falls back the same way. Until the sweep
// the row is present, and present is fresh.
func TestRemoveExpiredPingsFallsBackTheReadPath(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer clientSession.Cancel()

		clientId, connectionId, lookup := deriveTestLookupProvider(t, ctx)
		derived := &model.Location{
			LocationType: model.LocationTypeCity,
			City:         "Sydney",
			Region:       "New South Wales",
			Country:      "Australia",
			CountryCode:  "au",
		}
		model.CreateLocation(ctx, derived)
		model.ReplaceDerivedLocations(ctx, []*model.DerivedLocation{
			deriveTestStaleRow(model.DerivedLocationNodeKindProvider, clientId, derived, server.NowUtc().Add(-(controller.DefaultExtenderPingReportSettings().Retention + time.Hour))),
		})
		connect.AssertEqual(t, controller.SetConnectionLocation(ctx, connectionId, deriveTestClientIp), nil)
		cityLocationId, _ := deriveTestConnectionLocation(t, ctx, connectionId)
		connect.AssertEqual(t, cityLocationId, derived.CityLocationId)

		result, err := RemoveExpiredPings(&RemoveExpiredPingsArgs{}, clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.RemovedDerivedLocations, 1)
		if model.GetDerivedLocation(ctx, model.DerivedLocationNodeKindProvider, clientId) != nil {
			t.Fatal("the sweep left the expired derived row")
		}

		assertDeriveTestReadPathFallsBack(t, ctx, clientId, connectionId, lookup, derived)
	})
}

// (e) A run with no term anywhere publishes nothing, removes every row, and is
// a normal run: no error, not refused, recorded in the history like any
// other, and the chain re-arms.
func TestDeriveLocationsWithNoTermsIsANormalRun(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer clientSession.Cancel()
		defer model.Testing_PushPlaces(deriveTestPlaces)()

		sydney := &model.Location{
			LocationType: model.LocationTypeCity,
			City:         "Sydney",
			Region:       "New South Wales",
			Country:      "Australia",
			CountryCode:  "au",
		}
		model.CreateLocation(ctx, sydney)
		now := server.NowUtc()
		rows := []*model.DerivedLocation{}
		for range 3 {
			rows = append(rows, deriveTestStaleRow(model.DerivedLocationNodeKindExtender, server.NewId(), sydney, now.Add(-8*time.Hour)))
		}
		written, _ := model.ReplaceDerivedLocations(ctx, rows)
		connect.AssertEqual(t, written, 3)

		result, err := DeriveLocations(&DeriveLocationsArgs{}, clientSession)
		connect.AssertEqual(t, err, nil)
		t.Logf("%+v", result)
		connect.AssertEqual(t, result.Refused, "")
		connect.AssertEqual(t, result.Nodes, 0)
		connect.AssertEqual(t, result.Terms, 0)
		connect.AssertEqual(t, result.Published, 0)
		connect.AssertEqual(t, result.Removed, 3)
		connect.AssertEqual(t, len(model.GetDerivedLocations(ctx)), 0)

		run, ok := model.GetDeriveLocationsRun(ctx)
		if !ok {
			t.Fatal("the run was not recorded")
		}
		connect.AssertEqual(t, run.Nodes, 0)
		connect.AssertEqual(t, run.Terms, 0)
		connect.AssertEqual(t, run.Published, 0)
		connect.AssertEqual(t, run.Converged, true)

		server.Tx(ctx, func(tx server.PgTx) {
			connect.AssertEqual(t, DeriveLocationsPost(&DeriveLocationsArgs{}, result, clientSession, tx), nil)
		})
		runAt := testExtenderTaskRunAt(t, ctx, "derive_locations")
		deriveInterval := controller.DefaultExtenderPingReportSettings().DeriveInterval
		if runAt.Before(now.Add(deriveInterval - time.Minute)) {
			t.Fatalf("the chain re-armed at %s, want about %s", runAt, now.Add(deriveInterval))
		}
	})
}
