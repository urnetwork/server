package work

import (
	"context"
	"fmt"
	"math"
	"net/netip"
	"runtime"
	"testing"
	"time"
	"unsafe"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/geo"
	"github.com/urnetwork/server/geo/solve"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// The derive phase (connect/GEOMAP.md §5.1, §5.4-§5.7, §6).

// A small place list over the Gulf of Guinea: a region of three cities north
// to south (Estuaire, represented by West), a region of
// three cities west to east (Moyen, represented by Centreville), and two more
// countries north and south. Its geoname ids are synthetic.
const deriveTestPlaces = `
version: 1
source: test
build_epoch: 1
countries:
  ga: {name: Gabon, geoname_id: 2400553, continent_code: af, continent: Africa}
  cm: {name: Cameroon, geoname_id: 2233387, continent_code: af, continent: Africa}
  cg: {name: Congo Republic, geoname_id: 2260494, continent_code: af, continent: Africa}
places:
  ga:
    Estuaire:
      Westport: {geoname_id: 9006, region_geoname_id: 9101, latitude: 1.0, longitude: 9.0, spread_km: 0, time_zone: Africa/Libreville}
      West: {geoname_id: 9001, region_geoname_id: 9101, latitude: 0.0, longitude: 9.0, spread_km: 0, time_zone: Africa/Libreville}
      Westend: {geoname_id: 9007, region_geoname_id: 9101, latitude: -1.0, longitude: 9.0, spread_km: 0, time_zone: Africa/Libreville}
    Moyen:
      Middle: {geoname_id: 9002, region_geoname_id: 9102, latitude: 0.0, longitude: 11.25, spread_km: 0, time_zone: Africa/Libreville}
      Centreville: {geoname_id: 9008, region_geoname_id: 9102, latitude: 0.0, longitude: 12.375, spread_km: 0, time_zone: Africa/Libreville}
      East: {geoname_id: 9003, region_geoname_id: 9102, latitude: 0.0, longitude: 13.5, spread_km: 0, time_zone: Africa/Libreville}
  cm:
    Centre:
      North: {geoname_id: 9004, region_geoname_id: 9201, latitude: 4.5, longitude: 11.25, spread_km: 0, time_zone: Africa/Douala}
      Northgate: {geoname_id: 9009, region_geoname_id: 9201, latitude: 3.5, longitude: 11.25, spread_km: 0, time_zone: Africa/Douala}
  cg:
    Pool:
      South: {geoname_id: 9005, region_geoname_id: 9301, latitude: -4.5, longitude: 11.25, spread_km: 0, time_zone: Africa/Brazzaville}
`

// The test place list, loaded.
func loadDeriveTestPlaces(t testing.TB) *geo.Places {
	t.Helper()
	places, err := geo.LoadPlaces([]byte(deriveTestPlaces))
	if err != nil {
		t.Fatal(err)
	}
	return places
}

// A city of the test list by its synthetic geoname id.
func deriveTestCity(t testing.TB, places *geo.Places, geonameId uint32) *geo.Place {
	t.Helper()
	place := places.CityByGeonameId(geonameId)
	if place == nil {
		t.Fatalf("no test city %d", geonameId)
	}
	return place
}

// The defaults, measured at 10 km per millisecond. A stored round trip is whole milliseconds, which at the default 100 km per ms
// rounds every sample by up to 50 km; the synthetic graphs here are about the
// pipeline, not that rounding, so they round by up to 5 km instead.
func deriveTestSettings() *solve.Settings {
	settings := solve.DefaultSettings()
	settings.KmPerMs = 10
	return settings
}

// The whole-millisecond round trip the settings imply between two points.
func deriveTestRttMs(settings *solve.Settings, from solve.LatLon, to solve.LatLon) int {
	return int(math.Round(solve.DistanceKm(from, to)/settings.KmPerMs + settings.OverheadMs))
}

// A place's coordinates as the solver takes them.
func deriveTestLatLon(place *geo.Place) solve.LatLon {
	return solve.LatLon{Latitude: place.Latitude, Longitude: place.Longitude}
}

// A node id carries its kind, so a provider and an extender with the same id
// are two nodes, and it parses back to its party.
func TestDeriveNodeIds(t *testing.T) {
	id := server.NewId()
	provider := deriveNodeId(model.DerivedLocationNodeKindProvider, id)
	extender := deriveNodeId(model.DerivedLocationNodeKindExtender, id)
	connect.AssertEqual(t, provider, "p:"+id.String())
	connect.AssertEqual(t, extender, "e:"+id.String())
	// the same id as a provider and as an extender is two nodes
	if provider == extender {
		t.Fatal("a provider and an extender with the same id share a node id")
	}
	connect.AssertEqual(t, deriveNodeId(0, id), "")
	for _, nodeId := range []string{provider, extender} {
		key, ok := deriveNodeKeyOf(nodeId)
		if !ok || deriveNodeId(key.nodeKind, key.id) != nodeId {
			t.Fatalf("%s parses to %+v, %v", nodeId, key, ok)
		}
	}
	for _, notNodeId := range []string{"", "x:" + id.String(), "p:", "e:not-an-id"} {
		if _, ok := deriveNodeKeyOf(notNodeId); ok {
			t.Fatalf("%q parses as a node id", notNodeId)
		}
	}
	// the shared table names a party by the same string for every caller
	table := newDeriveNodeIdTable(4)
	first := table.nodeId(model.DerivedLocationNodeKindExtender, id)
	connect.AssertEqual(t, first, extender)
	connect.AssertEqual(t, unsafe.StringData(table.nodeId(model.DerivedLocationNodeKindExtender, id)), unsafe.StringData(first))
	for pingerKind, want := range map[int]int{
		model.NetworkPingPingerKindProvider: model.DerivedLocationNodeKindProvider,
		model.NetworkPingPingerKindExtender: model.DerivedLocationNodeKindExtender,
	} {
		nodeKind, ok := derivePingerNodeKind(pingerKind)
		if !ok || nodeKind != want {
			t.Fatalf("pinger kind %d is node kind %d, %v", pingerKind, nodeKind, ok)
		}
	}
	if _, ok := derivePingerNodeKind(3); ok {
		t.Fatal("an unknown pinger kind is a node kind")
	}
}

// Only a refusal that speaks to honesty counts (§5.5).
func TestDerivePingRefusalIsEvidence(t *testing.T) {
	for reason, want := range map[int]bool{
		0: false,
		// rtt below observed, nonce, wrong extender, bad signature
		1: true,
		2: true,
		3: true,
		5: true,
		// an unknown pinger and a rate limit say nothing about either party
		4:  false,
		6:  false,
		7:  false,
		-1: false,
	} {
		if got := derivePingRefusalIsEvidence(reason); got != want {
			t.Errorf("reason %d is evidence: %v, want %v", reason, got, want)
		}
	}
}

// Samples aggregate per ordered pair (§5.1): the two directions of a pair are
// two terms, each the median of its own source's samples.
func TestDeriveInputsAggregatePerOrderedPair(t *testing.T) {
	settings := solve.DefaultSettings()
	provider := server.NewId()
	extenderA := server.NewId()
	extenderB := server.NewId()
	rows := []*model.NetworkPingTerm{}
	add := func(pingerKind int, pingerId server.Id, targetExtenderId server.Id, rttMs int) {
		rows = append(rows, &model.NetworkPingTerm{
			PingerKind:       pingerKind,
			PingerId:         pingerId,
			TargetExtenderId: targetExtenderId,
			RttMs:            rttMs,
		})
	}
	add(model.NetworkPingPingerKindExtender, extenderA, extenderB, 30)
	add(model.NetworkPingPingerKindExtender, extenderA, extenderB, 10)
	add(model.NetworkPingPingerKindExtender, extenderA, extenderB, 12)
	add(model.NetworkPingPingerKindExtender, extenderB, extenderA, 20)
	add(model.NetworkPingPingerKindProvider, provider, extenderA, 5)
	add(model.NetworkPingPingerKindProvider, provider, extenderA, 8)
	// a kind that is not a node is no sample, and nor is a node's ping to
	// itself
	add(3, server.NewId(), extenderA, 1)
	add(model.NetworkPingPingerKindExtender, extenderB, extenderB, 1)
	inputs := deriveTestInputs(settings, rows)

	a := deriveNodeId(model.DerivedLocationNodeKindExtender, extenderA)
	b := deriveNodeId(model.DerivedLocationNodeKindExtender, extenderB)
	p := deriveNodeId(model.DerivedLocationNodeKindProvider, provider)
	terms := map[[2]string]solve.Term{}
	for _, term := range inputs.terms {
		terms[[2]string{term.Source, term.Target}] = term
	}
	connect.AssertEqual(t, len(terms), 3)
	connect.AssertEqual(t, terms[[2]string{a, b}], solve.Term{Source: a, Target: b, RttMs: 12, Samples: 3})
	connect.AssertEqual(t, terms[[2]string{b, a}], solve.Term{Source: b, Target: a, RttMs: 20, Samples: 1})
	// the median of an even count is the mean of the middle two
	connect.AssertEqual(t, terms[[2]string{p, a}], solve.Term{Source: p, Target: a, RttMs: 6.5, Samples: 2})

	connect.AssertEqual(t, inputs.cosignedPings, 7)
	connect.AssertEqual(t, inputs.droppedPings, 2)
	connect.AssertEqual(t, len(inputs.nodeKeys), 3)
	connect.AssertEqual(t, inputs.nodeKeys[p], deriveNodeKey{nodeKind: model.DerivedLocationNodeKindProvider, id: provider})
	connect.AssertEqual(t, inputs.nodeKeys[a], deriveNodeKey{nodeKind: model.DerivedLocationNodeKindExtender, id: extenderA})
	// the terms' samples are the attestations: a ping that is no sample
	// attests nothing
	connect.AssertEqual(t, *inputs.refusals[a], solve.Refusals{PingsAsPinger: 3, PingsAsTarget: 3})
	connect.AssertEqual(t, *inputs.refusals[b], solve.Refusals{PingsAsPinger: 1, PingsAsTarget: 3})
	connect.AssertEqual(t, *inputs.refusals[p], solve.Refusals{PingsAsPinger: 2})
}

// A refusal counts against both parties' refusal rates only when it is
// evidence, and every attestation with a verdict -- co-signed direct,
// co-signed relayed, or refused as evidence -- is in both denominators
// (§5.5). A refusal that is not evidence, and a ping with no verdict, is in
// neither.
func TestDeriveInputsCountRefusals(t *testing.T) {
	provider := server.NewId()
	pingerExtender := server.NewId()
	target := server.NewId()
	relayedTarget := server.NewId()

	rows := []*model.NetworkPingTerm{}
	for range 3 {
		rows = append(rows, &model.NetworkPingTerm{
			PingerKind:       model.NetworkPingPingerKindProvider,
			PingerId:         provider,
			TargetExtenderId: target,
			RttMs:            20,
		})
	}
	inputs := deriveTestInputs(solve.DefaultSettings(), rows)
	inputs.addRelayedCosigns(&model.NetworkPingAttestationCount{
		PingerKind:       model.NetworkPingPingerKindProvider,
		PingerId:         provider,
		TargetExtenderId: relayedTarget,
		Count:            2,
	})
	refuse := func(pingerKind int, pingerId server.Id, reason int) {
		inputs.addRefusal(&model.NetworkPingRefusal{
			PingerKind:       pingerKind,
			PingerId:         pingerId,
			TargetExtenderId: target,
			Reason:           reason,
		})
	}
	refuse(model.NetworkPingPingerKindProvider, provider, int(connect.ExtenderProbeVerdictReasonRttBelowObserved))
	refuse(model.NetworkPingPingerKindProvider, provider, int(connect.ExtenderProbeVerdictReasonUnknownPinger))
	refuse(model.NetworkPingPingerKindExtender, pingerExtender, int(connect.ExtenderProbeVerdictReasonRateLimited))
	refuse(model.NetworkPingPingerKindExtender, pingerExtender, int(connect.ExtenderProbeVerdictReasonBadSignature))
	refuse(3, server.NewId(), int(connect.ExtenderProbeVerdictReasonNonce))

	connect.AssertEqual(t, inputs.refusalCount, 2)
	connect.AssertEqual(t, inputs.ignoredRefusals, 3)

	p := deriveNodeId(model.DerivedLocationNodeKindProvider, provider)
	x := deriveNodeId(model.DerivedLocationNodeKindExtender, pingerExtender)
	e := deriveNodeId(model.DerivedLocationNodeKindExtender, target)
	r := deriveNodeId(model.DerivedLocationNodeKindExtender, relayedTarget)
	// three co-signed direct, two co-signed relayed, one refused as evidence
	connect.AssertEqual(t, *inputs.refusals[p], solve.Refusals{AsPinger: 1, PingsAsPinger: 6})
	// only its evidence refusal: its rate-limited probe is no attestation
	connect.AssertEqual(t, *inputs.refusals[x], solve.Refusals{AsPinger: 1, PingsAsPinger: 1})
	// three co-signed and two refused as evidence
	connect.AssertEqual(t, *inputs.refusals[e], solve.Refusals{AsTarget: 2, PingsAsTarget: 5})
	connect.AssertEqual(t, *inputs.refusals[r], solve.Refusals{PingsAsTarget: 2})

	// the solver is given the counts of its nodes: the parties of a co-signed
	// direct ping, whatever their counterparts
	refusals := inputs.solverRefusals()
	connect.AssertEqual(t, len(refusals), 2)
	connect.AssertEqual(t, refusals[p], *inputs.refusals[p])
	connect.AssertEqual(t, refusals[e], *inputs.refusals[e])
}

// A resolver of geneses against the test list, with the given location rows.
func deriveTestResolver(t testing.TB, locations ...*model.Location) *deriveGenesisResolver {
	places := loadDeriveTestPlaces(t)
	resolver := &deriveGenesisResolver{
		places:          places,
		representatives: geo.NewRepresentatives(places),
		settings:        solve.DefaultSettings(),
		locations:       map[server.Id]*model.Location{},
	}
	for _, location := range locations {
		resolver.locations[location.LocationId] = location
	}
	return resolver
}

// A city location row of a test place, with or without its coordinates.
func deriveTestCityLocation(place *geo.Place, withCoordinates bool) *model.Location {
	location := &model.Location{
		LocationId:      server.NewId(),
		LocationType:    model.LocationTypeCity,
		City:            place.City,
		Region:          place.Region,
		CountryCode:     place.CountryCode,
		CityGeonameId:   place.GeonameId,
		RegionGeonameId: place.RegionGeonameId,
	}
	location.CityLocationId = location.LocationId
	if withCoordinates {
		location.Latitude = place.Latitude
		location.Longitude = place.Longitude
	}
	return location
}

// Fails the test unless the genesis is placed, at the position, radius and
// stand-in wanted.
func assertDeriveGenesis(t *testing.T, name string, genesis *deriveGenesis, ok bool, wantPosition solve.LatLon, wantRadiusKm float64, wantStandIn bool) {
	t.Helper()
	if !ok || genesis == nil {
		t.Fatalf("%s: no genesis", name)
	}
	if 1e-9 < solve.DistanceKm(genesis.position, wantPosition) {
		t.Fatalf("%s: genesis at %+v, want %+v", name, genesis.position, wantPosition)
	}
	if 1e-6 < math.Abs(genesis.radiusKm-wantRadiusKm) {
		t.Fatalf("%s: radius %.6f km, want %.6f km", name, genesis.radiusKm, wantRadiusKm)
	}
	if genesis.standIn != wantStandIn {
		t.Fatalf("%s: stand-in %v, want %v", name, genesis.standIn, wantStandIn)
	}
}

// The genesis radius (§5.1): the lookup's accuracy radius when the row has
// one, a probe's fixed radius, else the width of the level the row was placed
// at; and a row placed only in a region or country stands at that region's or
// country's representative, anchored at least as wide as its cities spread.
func TestDeriveGenesisRadius(t *testing.T) {
	places := loadDeriveTestPlaces(t)
	settings := solve.DefaultSettings()
	representatives := geo.NewRepresentatives(places)
	west := deriveTestCity(t, places, 9001)
	middle := deriveTestCity(t, places, 9002)

	westCity := deriveTestCityLocation(west, true)
	// a legacy city row with no coordinates, found in the list by its id
	middleCity := deriveTestCityLocation(middle, false)
	moyenRegion := &model.Location{
		LocationId:      server.NewId(),
		LocationType:    model.LocationTypeRegion,
		Region:          "Moyen",
		CountryCode:     "ga",
		RegionGeonameId: 9102,
	}
	moyenRegion.RegionLocationId = moyenRegion.LocationId
	gabon := &model.Location{
		LocationId:   server.NewId(),
		LocationType: model.LocationTypeCountry,
		Country:      "Gabon",
		CountryCode:  "ga",
	}
	gabon.CountryLocationId = gabon.LocationId
	for _, location := range []*model.Location{westCity, middleCity, moyenRegion} {
		location.CountryLocationId = gabon.LocationId
	}
	westCity.RegionLocationId = server.NewId()
	middleCity.RegionLocationId = moyenRegion.LocationId
	resolver := deriveTestResolver(t, westCity, middleCity, moyenRegion, gabon)

	accuracy := func(km float32) *float32 {
		return &km
	}
	activation := func(accuracyKm *float32, locationIds ...*server.Id) *model.NetworkExtenderActivationRecord {
		record := &model.NetworkExtenderActivationRecord{AccuracyKm: accuracyKm}
		record.LocationId = locationIds[0]
		if 1 < len(locationIds) {
			record.CityLocationId = locationIds[1]
		}
		if 2 < len(locationIds) {
			record.RegionLocationId = locationIds[2]
		}
		if 3 < len(locationIds) {
			record.CountryLocationId = locationIds[3]
		}
		return record
	}

	// an extender placed at a city with a radius
	genesis, ok := resolver.extenderGenesis(activation(accuracy(12), &westCity.LocationId))
	assertDeriveGenesis(t, "city with a radius", genesis, ok, deriveTestLatLon(west), 12, false)
	connect.AssertEqual(t, genesis.source, deriveGenesisSourceActivation)
	connect.AssertEqual(t, genesis.regionKey(), geo.ContainmentRegionKey("ga", 9101, "Estuaire"))
	connect.AssertEqual(t, genesis.countryKey(), "ga")

	// a row written before the radius falls back by level
	genesis, ok = resolver.extenderGenesis(activation(nil, &westCity.LocationId))
	assertDeriveGenesis(t, "city without a radius", genesis, ok, deriveTestLatLon(west), settings.CityRadiusKm, false)

	// a city row without coordinates is at the list's coordinates for its id
	genesis, ok = resolver.extenderGenesis(activation(accuracy(20), &middleCity.LocationId))
	assertDeriveGenesis(t, "city without coordinates", genesis, ok, deriveTestLatLon(middle), 20, false)

	// a lookup that reached only the region: the location id is null past the
	// granularity it reached, and the region row stands at its representative
	moyen, _ := representatives.Region("ga", 9102, "Moyen")
	genesis, ok = resolver.extenderGenesis(activation(nil, nil, nil, &moyenRegion.LocationId, &gabon.LocationId))
	assertDeriveGenesis(t, "region without a radius", genesis, ok, deriveTestLatLon(moyen.Place), max(settings.RegionRadiusKm, moyen.SpreadKm), true)
	connect.AssertEqual(t, moyen.Place.City, "Centreville")
	connect.AssertEqual(t, genesis.regionKey(), geo.ContainmentRegionKey("ga", 9102, "Moyen"))

	// a country-only lookup, anchored as wide as GeoLite2 said or the country
	// is, whichever is wider
	country, _ := representatives.Country("ga")
	genesis, ok = resolver.extenderGenesis(activation(accuracy(40), &gabon.LocationId))
	assertDeriveGenesis(t, "country with a narrow radius", genesis, ok, deriveTestLatLon(country.Place), max(40, country.SpreadKm), true)
	connect.AssertEqual(t, genesis.regionKey(), "")
	genesis, ok = resolver.extenderGenesis(activation(accuracy(1000), &gabon.LocationId))
	assertDeriveGenesis(t, "country with a wide radius", genesis, ok, deriveTestLatLon(country.Place), 1000, true)

	// an activation that resolved nothing has no genesis
	if _, ok := resolver.extenderGenesis(activation(accuracy(5), nil)); ok {
		t.Fatal("an unplaced activation has a genesis")
	}
	if _, ok := resolver.extenderGenesis(nil); ok {
		t.Fatal("an extender without an activation has a genesis")
	}

	// a fresh probe wins over the connection, with its own radius: a city
	// confident probe is tight, a country one is as wide as the country
	connection := &model.ConnectionGenesisLocation{
		CityLocationId:    westCity.LocationId,
		RegionLocationId:  westCity.RegionLocationId,
		CountryLocationId: gabon.LocationId,
		GenesisLocationId: &westCity.LocationId,
		AccuracyKm:        accuracy(7),
	}
	genesis, ok = resolver.providerGenesis(&model.ProviderEgressLocation{LocationId: middleCity.LocationId, CityConfident: true}, connection)
	assertDeriveGenesis(t, "city confident probe", genesis, ok, deriveTestLatLon(middle), settings.ProbedCityRadiusKm, false)
	connect.AssertEqual(t, genesis.source, deriveGenesisSourceProbe)
	genesis, ok = resolver.providerGenesis(&model.ProviderEgressLocation{LocationId: gabon.LocationId}, connection)
	assertDeriveGenesis(t, "country probe", genesis, ok, deriveTestLatLon(country.Place), max(settings.ProbedOtherRadiusKm, country.SpreadKm), true)
	// a probe whose row is gone falls back to the connection
	genesis, ok = resolver.providerGenesis(&model.ProviderEgressLocation{LocationId: server.NewId(), CityConfident: true}, connection)
	assertDeriveGenesis(t, "probe without a row", genesis, ok, deriveTestLatLon(west), 7, false)
	connect.AssertEqual(t, genesis.source, deriveGenesisSourceConnection)
}

// A provider published at its derived location keeps its lookup beside it
// (§6), and the derivation anchors to the lookup: never to the derived
// location it wrote, or each run would anchor to the one before.
func TestDeriveProviderGenesisIsTheLookupNotThePublishedLocation(t *testing.T) {
	places := loadDeriveTestPlaces(t)
	settings := solve.DefaultSettings()
	west := deriveTestCity(t, places, 9001)
	middle := deriveTestCity(t, places, 9002)
	lookup := deriveTestCityLocation(west, true)
	derived := deriveTestCityLocation(middle, true)
	gabon := &model.Location{LocationId: server.NewId(), LocationType: model.LocationTypeCountry, Country: "Gabon", CountryCode: "ga"}
	moyenRegion := &model.Location{LocationId: server.NewId(), LocationType: model.LocationTypeRegion, Region: "Moyen", CountryCode: "ga", RegionGeonameId: 9102}
	resolver := deriveTestResolver(t, lookup, derived, gabon, moyenRegion)
	accuracyKm := float32(300)

	// published at the derived city, with the lookup as its genesis
	genesis, ok := resolver.providerGenesis(nil, &model.ConnectionGenesisLocation{
		CityLocationId:    derived.LocationId,
		RegionLocationId:  server.NewId(),
		CountryLocationId: gabon.LocationId,
		GenesisLocationId: &lookup.LocationId,
		AccuracyKm:        &accuracyKm,
	})
	assertDeriveGenesis(t, "published at a derived location", genesis, ok, deriveTestLatLon(west), 300, false)

	// a genesis that cannot be read is no genesis: the published columns may
	// be the derivation's own answer
	missingId := server.NewId()
	if _, ok := resolver.providerGenesis(nil, &model.ConnectionGenesisLocation{
		CityLocationId:    derived.LocationId,
		CountryLocationId: gabon.LocationId,
		GenesisLocationId: &missingId,
	}); ok {
		t.Fatal("a provider whose genesis row is gone was anchored to its published location")
	}

	// a row written before the genesis column is its own genesis, at the
	// granularity its columns reached: a coarser location stores its coarsest
	// id in the finer columns
	genesis, ok = resolver.providerGenesis(nil, &model.ConnectionGenesisLocation{
		CityLocationId:    lookup.LocationId,
		RegionLocationId:  server.NewId(),
		CountryLocationId: gabon.LocationId,
	})
	assertDeriveGenesis(t, "legacy city row", genesis, ok, deriveTestLatLon(west), settings.CityRadiusKm, false)
	genesis, ok = resolver.providerGenesis(nil, &model.ConnectionGenesisLocation{
		CityLocationId:    gabon.LocationId,
		RegionLocationId:  moyenRegion.LocationId,
		CountryLocationId: gabon.LocationId,
	})
	if !ok || genesis.level != solve.GenesisLevelRegion || genesis.region != "Moyen" {
		t.Fatalf("legacy region row: %+v, %v", genesis, ok)
	}
	genesis, ok = resolver.providerGenesis(nil, &model.ConnectionGenesisLocation{
		CityLocationId:    gabon.LocationId,
		RegionLocationId:  gabon.LocationId,
		CountryLocationId: gabon.LocationId,
	})
	if !ok || genesis.level != solve.GenesisLevelCountry || genesis.region != "" {
		t.Fatalf("legacy country row: %+v, %v", genesis, ok)
	}
	if _, ok := resolver.providerGenesis(nil, nil); ok {
		t.Fatal("a provider with neither a probe nor a connection has a genesis")
	}
}

// A derived point maps to the nearest city anywhere (§6), and the row records
// whether that city is outside the genesis region or country.
func TestDeriveMappingCrossings(t *testing.T) {
	places := loadDeriveTestPlaces(t)
	estuaire := &deriveGenesis{countryCode: "ga", region: "Estuaire", regionGeonameId: 9101}
	// a row that predates the ids, matched by name
	estuaireByName := &deriveGenesis{countryCode: "ga", region: "Estuaire"}
	gabonOnly := &deriveGenesis{countryCode: "ga"}
	for _, test := range []struct {
		name           string
		genesis        *deriveGenesis
		position       solve.LatLon
		wantCity       string
		crossedRegion  bool
		crossedCountry bool
	}{
		{name: "stays in its region", genesis: estuaire, position: solve.LatLon{Latitude: 0.1, Longitude: 9.1}, wantCity: "West"},
		{name: "stays in its region, by name", genesis: estuaireByName, position: solve.LatLon{Latitude: 0.9, Longitude: 9.0}, wantCity: "Westport"},
		{name: "crosses into the next region", genesis: estuaire, position: solve.LatLon{Latitude: 0, Longitude: 11.0}, wantCity: "Middle", crossedRegion: true},
		{name: "crosses into the next region, by name", genesis: estuaireByName, position: solve.LatLon{Latitude: 0, Longitude: 11.0}, wantCity: "Middle", crossedRegion: true},
		{name: "crosses the country", genesis: estuaire, position: solve.LatLon{Latitude: 3.6, Longitude: 11.2}, wantCity: "Northgate", crossedRegion: true, crossedCountry: true},
		// a genesis placed only in its country has no region to cross
		{name: "a country genesis moves within its country", genesis: gabonOnly, position: solve.LatLon{Latitude: 0, Longitude: 13.4}, wantCity: "East"},
		{name: "a country genesis crosses the country", genesis: gabonOnly, position: solve.LatLon{Latitude: -4.4, Longitude: 11.2}, wantCity: "South", crossedRegion: true, crossedCountry: true},
	} {
		mapping, ok := mapDerivedPosition(test.position, test.genesis, places)
		if !ok {
			t.Fatalf("%s: unmapped", test.name)
		}
		if mapping.place.City != test.wantCity || mapping.crossedRegion != test.crossedRegion || mapping.crossedCountry != test.crossedCountry {
			t.Errorf("%s: %s, crossed region %v country %v; want %s, %v %v", test.name, mapping.place.City, mapping.crossedRegion, mapping.crossedCountry, test.wantCity, test.crossedRegion, test.crossedCountry)
		}
	}
}

// The correction's longitude is the short way round.
func TestDeriveLongitudeDelta(t *testing.T) {
	for _, test := range []struct {
		from float64
		to   float64
		want float64
	}{
		{from: 10, to: 12.5, want: 2.5},
		{from: 12.5, to: 10, want: -2.5},
		// the short way across the antimeridian
		{from: 179.5, to: -179.5, want: 1},
		{from: -179.5, to: 179.5, want: -1},
	} {
		if got := deriveLongitudeDelta(test.from, test.to); 1e-9 < math.Abs(got-test.want) {
			t.Errorf("delta %v to %v = %v, want %v", test.from, test.to, got, test.want)
		}
	}
}

// The synthetic graph the plan and the job are tested on:
// four extenders at West, East, North and South, anchored where they are, and
// two providers whose geneses are wrong -- one a region west of where it is,
// one a country south -- with the wide radius of a lookup that could not place
// them well. Every source pings every extender, `samples` times a pair.
type deriveTestGraph struct {
	settings  *solve.Settings
	places    *geo.Places
	extenders map[string]server.Id
	providers map[string]server.Id
	truth     map[string]solve.LatLon
	geneses   map[string]*deriveGenesis
}

// The graph, with fresh ids and the geneses described on the type.
func newDeriveTestGraph(t testing.TB, settings *solve.Settings) *deriveTestGraph {
	places := loadDeriveTestPlaces(t)
	graph := &deriveTestGraph{
		settings:  settings,
		places:    places,
		extenders: map[string]server.Id{},
		providers: map[string]server.Id{},
		truth:     map[string]solve.LatLon{},
		geneses:   map[string]*deriveGenesis{},
	}
	for name, geonameId := range map[string]uint32{"west": 9001, "east": 9003, "north": 9004, "south": 9005} {
		place := deriveTestCity(t, places, geonameId)
		id := server.NewId()
		graph.extenders[name] = id
		nodeId := deriveNodeId(model.DerivedLocationNodeKindExtender, id)
		graph.truth[nodeId] = deriveTestLatLon(place)
		graph.geneses[nodeId] = &deriveGenesis{
			source:          deriveGenesisSourceActivation,
			level:           solve.GenesisLevelCity,
			position:        deriveTestLatLon(place),
			radiusKm:        5,
			countryCode:     place.CountryCode,
			region:          place.Region,
			regionGeonameId: place.RegionGeonameId,
		}
	}
	for name, cities := range map[string][2]uint32{
		// at Middle, placed at West
		"region": {9002, 9001},
		// at Northgate, placed at Middle
		"country": {9009, 9002},
	} {
		truth := deriveTestCity(t, places, cities[0])
		genesis := deriveTestCity(t, places, cities[1])
		id := server.NewId()
		graph.providers[name] = id
		nodeId := deriveNodeId(model.DerivedLocationNodeKindProvider, id)
		graph.truth[nodeId] = deriveTestLatLon(truth)
		graph.geneses[nodeId] = &deriveGenesis{
			source:          deriveGenesisSourceConnection,
			level:           solve.GenesisLevelCity,
			position:        deriveTestLatLon(genesis),
			radiusKm:        1000,
			countryCode:     genesis.CountryCode,
			region:          genesis.Region,
			regionGeonameId: genesis.RegionGeonameId,
		}
	}
	return graph
}

// Every co-signed ping of the graph: each extender of every other, and each
// provider of every extender, `samples` times a pair.
func (self *deriveTestGraph) pings(samples int) []*model.NetworkPingTerm {
	terms := []*model.NetworkPingTerm{}
	add := func(pingerKind int, pingerId server.Id, pingerNodeId string, targetId server.Id) {
		targetNodeId := deriveNodeId(model.DerivedLocationNodeKindExtender, targetId)
		for range samples {
			terms = append(terms, &model.NetworkPingTerm{
				PingerKind:       pingerKind,
				PingerId:         pingerId,
				TargetExtenderId: targetId,
				RttMs:            deriveTestRttMs(self.settings, self.truth[pingerNodeId], self.truth[targetNodeId]),
			})
		}
	}
	for _, targetId := range self.extenders {
		for _, pingerId := range self.extenders {
			if pingerId != targetId {
				add(model.NetworkPingPingerKindExtender, pingerId, deriveNodeId(model.DerivedLocationNodeKindExtender, pingerId), targetId)
			}
		}
		for _, providerId := range self.providers {
			add(model.NetworkPingPingerKindProvider, providerId, deriveNodeId(model.DerivedLocationNodeKindProvider, providerId), targetId)
		}
	}
	return terms
}

// The inputs the graph's pings make, `samples` a pair.
func (self *deriveTestGraph) inputs(samples int) *deriveInputs {
	return deriveTestInputs(self.settings, self.pings(samples))
}

// The inputs one cursor over every row gathers, merged as the job merges its
// cursors'.
func deriveTestInputs(settings *solve.Settings, rows []*model.NetworkPingTerm) *deriveInputs {
	cursor := newDeriveCursorInputs(settings, newDeriveNodeIdTable(1))
	for _, row := range rows {
		cursor.addTerm(row)
	}
	cursor.finish()
	return mergeDeriveInputs([]*deriveCursorInputs{cursor})
}

// The plan's publication of a node, or nil.
func derivePublicationFor(plan *derivePlan, nodeId string) *derivePublication {
	for _, publication := range plan.publications {
		if publication.nodeResult.Id == nodeId {
			return publication
		}
	}
	return nil
}

// The plan on the synthetic graph: the two providers whose geneses were wrong
// are placed where their pings say, mapped to their true cities, and reported
// as crossing the region and the country lines; a warm start from the plan's
// own rows reaches the same answer. With the containment terms on, the same
// crossing costs the solve (§5.2) and the providers stop short of it.
func TestPlanDerivationOnASyntheticGraph(t *testing.T) {
	settings := deriveTestSettings()
	// let the crossings through, so the report of them is what is tested
	settings.LambdaRegion = 0
	settings.LambdaCountry = 0
	graph := newDeriveTestGraph(t, settings)
	inputs := graph.inputs(8)
	plan := planDerivation(inputs, graph.geneses, map[string]*model.DerivedLocation{}, graph.places, settings)

	connect.AssertEqual(t, len(plan.result.Nodes), 6)
	connect.AssertEqual(t, plan.providerNodes, 2)
	connect.AssertEqual(t, plan.extenderNodes, 4)
	connect.AssertEqual(t, len(plan.result.Excluded), 0)
	t.Logf(
		"residual %.3f km against %.3f km at genesis; sweeps %v; %d published",
		plan.result.ResidualKm, plan.result.GenesisResidualKm, plan.result.Sweeps, len(plan.publications),
	)
	if !(plan.result.ResidualKm < plan.result.GenesisResidualKm) {
		t.Fatalf("the derivation did not improve on genesis: %.3f km against %.3f km", plan.result.ResidualKm, plan.result.GenesisResidualKm)
	}

	for name, want := range map[string]struct {
		city           string
		crossedRegion  bool
		crossedCountry bool
	}{
		"region":  {city: "Middle", crossedRegion: true},
		"country": {city: "Northgate", crossedRegion: true, crossedCountry: true},
	} {
		nodeId := deriveNodeId(model.DerivedLocationNodeKindProvider, graph.providers[name])
		publication := derivePublicationFor(plan, nodeId)
		if publication == nil {
			t.Fatalf("%s: not published: %+v", name, plan.result.Node(nodeId))
		}
		errorKm := solve.DistanceKm(publication.nodeResult.Position, graph.truth[nodeId])
		genesisErrorKm := solve.DistanceKm(graph.geneses[nodeId].position, graph.truth[nodeId])
		t.Logf("%s: genesis %.1f km off, derived %.2f km off; mapped to %s", name, genesisErrorKm, errorKm, publication.mapping.place.City)
		if !(errorKm < 10) {
			t.Fatalf("%s: derived %.2f km from the truth", name, errorKm)
		}
		connect.AssertEqual(t, publication.mapping.place.City, want.city)
		connect.AssertEqual(t, publication.mapping.crossedRegion, want.crossedRegion)
		connect.AssertEqual(t, publication.mapping.crossedCountry, want.crossedCountry)

		// the row stores the genesis it was solved from and the correction
		// as a delta on it
		row := publication.derivedLocation(&model.Location{LocationId: server.NewId()}, server.NowUtc())
		connect.AssertEqual(t, row.NodeKind, model.DerivedLocationNodeKindProvider)
		connect.AssertEqual(t, row.NodeId, graph.providers[name])
		connect.AssertEqual(t, float64(row.GenesisAccuracyKm), 1000.0)
		if 1e-9 < math.Abs(row.GenesisLatitude+row.DeltaLatitude-row.Latitude) ||
			1e-9 < math.Abs(deriveLongitudeDelta(row.GenesisLongitude+row.DeltaLongitude, row.Longitude)) {
			t.Fatalf("%s: genesis ⊕ delta is not the point: %+v", name, row)
		}
		if !(0 < row.Reputation && row.Reputation <= 1) || row.PeerCount != 4 || row.PingCount != 4*8 {
			t.Fatalf("%s: row %+v", name, row)
		}
	}

	// a warm start from the plan's own rows reaches the same answer
	previous := map[string]*model.DerivedLocation{}
	for _, publication := range plan.publications {
		row := publication.derivedLocation(&model.Location{LocationId: server.NewId()}, server.NowUtc())
		previous[publication.nodeResult.Id] = row
	}
	warm := planDerivation(graph.inputs(8), graph.geneses, previous, graph.places, settings)
	for _, publication := range plan.publications {
		warmResult := warm.result.Node(publication.nodeResult.Id)
		if 0.1 < solve.DistanceKm(warmResult.Position, publication.nodeResult.Position) {
			t.Fatalf("%s: a warm start ended %.3f km from the cold one", publication.nodeResult.Id, solve.DistanceKm(warmResult.Position, publication.nodeResult.Position))
		}
	}
	t.Logf("sweeps per round: cold %v, warm %v", plan.result.Sweeps, warm.result.Sweeps)

	// the containment prices the same crossings: the providers stop short of
	// them, nearer their genesis regions than the pings alone would put them
	contained := deriveTestSettings()
	containedPlan := planDerivation(graph.inputs(8), graph.geneses, map[string]*model.DerivedLocation{}, graph.places, contained)
	for name, providerId := range graph.providers {
		nodeId := deriveNodeId(model.DerivedLocationNodeKindProvider, providerId)
		free := plan.result.Node(nodeId)
		held := containedPlan.result.Node(nodeId)
		genesis := graph.geneses[nodeId].position
		t.Logf("%s: moved %.1f km from genesis without containment, %.1f km with", name, solve.DistanceKm(genesis, free.Position), solve.DistanceKm(genesis, held.Position))
		if !(solve.DistanceKm(genesis, held.Position) < solve.DistanceKm(genesis, free.Position)) {
			t.Fatalf("%s: the containment did not hold the provider back", name)
		}
	}
}

// A node with no genesis is not solved, and the terms that name it are
// dropped rather than anchored to nothing.
func TestPlanDerivationLeavesOutANodeWithoutAGenesis(t *testing.T) {
	settings := deriveTestSettings()
	graph := newDeriveTestGraph(t, settings)
	unplaced := deriveNodeId(model.DerivedLocationNodeKindProvider, graph.providers["region"])
	delete(graph.geneses, unplaced)
	inputs := graph.inputs(4)
	plan := planDerivation(inputs, graph.geneses, map[string]*model.DerivedLocation{}, graph.places, settings)
	if plan.result.Node(unplaced) != nil {
		t.Fatal("a node without a genesis was solved")
	}
	summary := plan.summary(inputs)
	connect.AssertEqual(t, summary.NodesSeen, 6)
	connect.AssertEqual(t, summary.Nodes, 5)
	connect.AssertEqual(t, summary.NodesWithoutGenesis, 1)
	// its four terms, one toward each extender
	connect.AssertEqual(t, summary.DroppedTerms, 4)
}

// The schedule: every DeriveInterval, three derivations inside the day a ping
// lives. Pure.
func TestDeriveLocationsCadence(t *testing.T) {
	pingReportSettings := controller.DefaultExtenderPingReportSettings()
	connect.AssertEqual(t, pingReportSettings.DeriveInterval, 8*time.Hour)
	connect.AssertEqual(t, pingReportSettings.Retention/pingReportSettings.DeriveInterval, time.Duration(3))
}

// The database tests: the job on a seeded graph, and the sweep of its rows.

// Stores the synthetic graph as the network would: every extender activated at its city with a tight radius, every provider
// connected and located at its genesis with a wide one, and every ping
// co-signed. It returns the location rows by geoname id.
func seedDeriveTestGraph(t testing.TB, ctx context.Context, graph *deriveTestGraph, samples int, now time.Time) map[uint32]*model.Location {
	t.Helper()
	locations := map[uint32]*model.Location{}
	for place := range graph.places.Cities() {
		country := graph.places.Country(place.CountryCode)
		location := &model.Location{
			LocationType:     model.LocationTypeCity,
			City:             place.City,
			Region:           place.Region,
			Country:          country.Name,
			CountryCode:      place.CountryCode,
			Latitude:         place.Latitude,
			Longitude:        place.Longitude,
			Timezone:         place.TimeZone,
			CityGeonameId:    place.GeonameId,
			RegionGeonameId:  place.RegionGeonameId,
			CountryGeonameId: country.GeonameId,
		}
		model.CreateLocation(ctx, location)
		locations[place.GeonameId] = location
	}

	accuracyKm := func(km float32) *float32 {
		return &km
	}
	i := 0
	for name, extenderId := range graph.extenders {
		i += 1
		nodeId := deriveNodeId(model.DerivedLocationNodeKindExtender, extenderId)
		genesis := graph.geneses[nodeId]
		place, _ := graph.places.NearestCity(genesis.position.Latitude, genesis.position.Longitude, "")
		location := locations[place.GeonameId]
		activated := model.ActivateNetworkExtender(ctx, (&model.NetworkExtenderActivation{
			NetworkId:   server.NewId(),
			ClientId:    server.NewId(),
			PublicKey:   []byte(fmt.Sprintf("derive-test-extender-%s-%s", name, extenderId)),
			TcpPort:     443,
			UdpPort:     443,
			DnsPort:     53,
			DnsTld:      connect.DefaultExtenderDnsTld,
			CountryCode: place.CountryCode,
			IpVersion:   4,
			Ip:          netip.MustParseAddr(fmt.Sprintf("192.0.2.%d", 10+i)),
			Carriers:    []string{connect.ExtenderCarrierTcp},
			AccuracyKm:  accuracyKm(float32(genesis.radiusKm)),
		}).WithLocation(location), func(*model.NetworkExtender, []*model.NetworkExtenderAddress, time.Time) ([]byte, error) {
			return []byte("record"), nil
		})
		if activated == nil {
			t.Fatalf("extender %s was not activated", name)
		}
		// the graph's ids are the stored ones
		delete(graph.geneses, nodeId)
		delete(graph.truth, nodeId)
		truth := deriveTestLatLon(place)
		graph.extenders[name] = activated.Extender.ExtenderId
		nodeId = deriveNodeId(model.DerivedLocationNodeKindExtender, activated.Extender.ExtenderId)
		graph.geneses[nodeId] = genesis
		graph.truth[nodeId] = truth
	}
	for name, clientId := range graph.providers {
		nodeId := deriveNodeId(model.DerivedLocationNodeKindProvider, clientId)
		genesis := graph.geneses[nodeId]
		place, _ := graph.places.NearestCity(genesis.position.Latitude, genesis.position.Longitude, "")
		location := locations[place.GeonameId]
		model.Testing_CreateDevice(ctx, server.NewId(), server.NewId(), clientId, "", "")
		handlerId := model.CreateNetworkClientHandler(ctx)
		connectionId, _, _, _, err := model.ConnectNetworkClient(ctx, clientId, "192.0.2.99:0", handlerId)
		if err != nil {
			t.Fatalf("provider %s: %v", name, err)
		}
		if err := model.SetConnectionLocation(ctx, connectionId, location.LocationId, &model.ConnectionLocationScores{
			AccuracyKm:        accuracyKm(float32(genesis.radiusKm)),
			GenesisLocationId: &location.LocationId,
		}); err != nil {
			t.Fatalf("provider %s: %v", name, err)
		}
	}

	pings := []*model.NetworkPing{}
	for _, term := range graph.pings(samples) {
		pings = append(pings, &model.NetworkPing{
			PingerKind:       term.PingerKind,
			PingerId:         term.PingerId,
			TargetExtenderId: term.TargetExtenderId,
			ProbeNonce:       server.NewId().Bytes(),
			RttMs:            term.RttMs,
			ProbeTime:        now,
			Cosign:           model.NetworkPingCosignCosigned,
			PingerSignature:  []byte("pinger-signature"),
			Cosignature:      []byte("cosignature"),
			CreateTime:       now,
		})
	}
	connect.AssertEqual(t, model.AddNetworkPings(ctx, pings), len(pings))
	return locations
}

// The job on a seeded graph: nodes from activations and connection locations,
// terms from network_ping. The providers whose geneses were wrong are
// published at their true cities with their crossings, a row the derivation
// did not publish is removed, the run is recorded for the dashboard, and a
// second run warm starts from the first to the same rows.
func TestDeriveLocationsOnASeededGraph(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer model.Testing_PushPlaces(deriveTestPlaces)()
		places := model.CurrentPlaces()
		if places == nil {
			t.Fatal("the pushed place list is not current")
		}

		settings := deriveTestSettings()
		settings.LambdaRegion = 0
		settings.LambdaCountry = 0
		graph := newDeriveTestGraph(t, settings)
		graph.places = places
		now := server.NowUtc()
		locations := seedDeriveTestGraph(t, ctx, graph, 8, now)

		// a provider that pings but was never located has no genesis
		unlocated := server.NewId()
		connect.AssertEqual(t, model.AddNetworkPings(ctx, []*model.NetworkPing{{
			PingerKind:       model.NetworkPingPingerKindProvider,
			PingerId:         unlocated,
			TargetExtenderId: graph.extenders["west"],
			ProbeNonce:       server.NewId().Bytes(),
			RttMs:            12,
			ProbeTime:        now,
			Cosign:           model.NetworkPingCosignCosigned,
			PingerSignature:  []byte("pinger-signature"),
			Cosignature:      []byte("cosignature"),
			CreateTime:       now,
		}}), 1)
		// evidence and non-evidence refusals, and a relayed co-signed ping,
		// none of which is a term
		refusals := []*model.NetworkPing{}
		for _, reason := range []uint32{connect.ExtenderProbeVerdictReasonRttBelowObserved, connect.ExtenderProbeVerdictReasonRateLimited} {
			refusals = append(refusals, &model.NetworkPing{
				PingerKind:       model.NetworkPingPingerKindProvider,
				PingerId:         graph.providers["region"],
				TargetExtenderId: graph.extenders["east"],
				ProbeNonce:       server.NewId().Bytes(),
				RttMs:            3,
				ProbeTime:        now,
				Cosign:           model.NetworkPingCosignRejected,
				CosignReason:     int(reason),
				PingerSignature:  []byte("pinger-signature"),
				CreateTime:       now,
			})
		}
		refusals = append(refusals, &model.NetworkPing{
			PingerKind:       model.NetworkPingPingerKindProvider,
			PingerId:         graph.providers["region"],
			TargetExtenderId: graph.extenders["north"],
			ProbeNonce:       server.NewId().Bytes(),
			RttMs:            400,
			ProbeTime:        now,
			Cosign:           model.NetworkPingCosignCosigned,
			PingerSignature:  []byte("pinger-signature"),
			Cosignature:      []byte("cosignature"),
			HopCount:         1,
			CreateTime:       now,
		})
		connect.AssertEqual(t, model.AddNetworkPings(ctx, refusals), 3)

		// a row of a node this derivation will not publish
		staleNodeId := server.NewId()
		model.ReplaceDerivedLocations(ctx, []*model.DerivedLocation{{
			NodeKind:          model.DerivedLocationNodeKindExtender,
			NodeId:            staleNodeId,
			GenesisAccuracyKm: 5,
			LocationId:        locations[9001].LocationId,
			CityLocationId:    locations[9001].CityLocationId,
			RegionLocationId:  locations[9001].RegionLocationId,
			CountryLocationId: locations[9001].CountryLocationId,
			UpdateTime:        now.Add(-time.Hour),
		}})

		result, err := deriveLocations(ctx, places, settings, DefaultDeriveLocationsSettings(), now)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%+v", result)
		connect.AssertEqual(t, result.NodesSeen, 7)
		connect.AssertEqual(t, result.Nodes, 6)
		connect.AssertEqual(t, result.NodesWithoutGenesis, 1)
		connect.AssertEqual(t, result.ProviderNodes, 2)
		connect.AssertEqual(t, result.ExtenderNodes, 4)
		connect.AssertEqual(t, result.Refusals, 1)
		connect.AssertEqual(t, result.IgnoredRefusals, 1)
		connect.AssertEqual(t, result.ExcludedSources, 0)
		connect.AssertEqual(t, result.Removed, 1)
		if model.GetDerivedLocation(ctx, model.DerivedLocationNodeKindExtender, staleNodeId) != nil {
			t.Fatal("the row of a node the derivation did not publish is still there")
		}

		for name, want := range map[string]struct {
			geonameId      uint32
			genesisId      uint32
			crossedRegion  bool
			crossedCountry bool
		}{
			"region":  {geonameId: 9002, genesisId: 9001, crossedRegion: true},
			"country": {geonameId: 9009, genesisId: 9002, crossedRegion: true, crossedCountry: true},
		} {
			row := model.GetDerivedLocation(ctx, model.DerivedLocationNodeKindProvider, graph.providers[name])
			if row == nil {
				t.Fatalf("%s: not published", name)
			}
			city := locations[want.geonameId]
			connect.AssertEqual(t, row.LocationId, city.LocationId)
			connect.AssertEqual(t, row.CityLocationId, city.CityLocationId)
			connect.AssertEqual(t, row.RegionLocationId, city.RegionLocationId)
			connect.AssertEqual(t, row.CountryLocationId, city.CountryLocationId)
			connect.AssertEqual(t, row.CountryCode, city.CountryCode)
			connect.AssertEqual(t, row.CrossedRegion, want.crossedRegion)
			connect.AssertEqual(t, row.CrossedCountry, want.crossedCountry)
			// solved from the connection's genesis, with its radius; the
			// coordinates are the list's, which the location rows store
			genesis := deriveTestCity(t, places, want.genesisId)
			connect.AssertEqual(t, row.GenesisLatitude, genesis.Latitude)
			connect.AssertEqual(t, row.GenesisLongitude, genesis.Longitude)
			connect.AssertEqual(t, row.GenesisAccuracyKm, float32(1000))
			connect.AssertEqual(t, row.PeerCount, 4)
			connect.AssertEqual(t, row.UpdateTime.Unix(), now.Unix())
			truth := deriveTestCity(t, places, want.geonameId)
			errorKm := geo.DistanceKm(row.Latitude, row.Longitude, truth.Latitude, truth.Longitude)
			t.Logf("%s: derived %.2f km from its true city", name, errorKm)
			if !(errorKm < 10) {
				t.Fatalf("%s: derived %.2f km from its city", name, errorKm)
			}
		}

		run, ok := model.GetDeriveLocationsRun(ctx)
		if !ok {
			t.Fatal("the run was not recorded")
		}
		connect.AssertEqual(t, run.RunTime.Unix(), now.Unix())
		connect.AssertEqual(t, run.Published, result.Published)
		connect.AssertEqual(t, run.ExcludedSources, 0)
		connect.AssertEqual(t, run.ResidualKm, result.ResidualKm)
		connect.AssertEqual(t, run.GenesisResidualKm, result.GenesisResidualKm)
		connect.AssertEqual(t, run.Nodes, result.Nodes)
		// every solved node pings, so every node is a source
		connect.AssertEqual(t, run.Sources, result.Nodes)
		connect.AssertEqual(t, run.CosignedPings, result.CosignedPings)
		connect.AssertEqual(t, run.Converged, result.Converged)
		connect.AssertEqual(t, run.LastRoundSweeps, result.Sweeps[len(result.Sweeps)-1])
		connect.AssertEqual(t, run.SweepCap, settings.MaxIterations)
		// what the run cost, and the planner's projection of the next from it
		// at the same cores: its own solve time times the safety factor, and
		// its own peak
		jobSettings := DefaultDeriveLocationsSettings()
		sweeps := 0
		for _, roundSweeps := range result.Sweeps {
			sweeps += roundSweeps
		}
		connect.AssertEqual(t, run.Sweeps, sweeps)
		connect.AssertEqual(t, run.Cores, runtime.GOMAXPROCS(0))
		connect.AssertEqual(t, run.ReadCursors, jobSettings.DeriveReadCursors)
		connect.AssertEqual(t, run.SolveSeconds, result.SolveSeconds)
		connect.AssertEqual(t, run.IngestSeconds, result.IngestSeconds)
		if !(0 < run.SolveSeconds && 0 < run.IngestSeconds && 0 < run.SecondsPerTermSweep && 0 < run.SecondsPerNodeSweep) {
			t.Fatalf("the run measured nothing: %+v", run)
		}
		if 1e-9*run.ProjectedSeconds < math.Abs(run.ProjectedSeconds-jobSettings.SweepSafetyFactor*run.SolveSeconds) {
			t.Fatalf("the next run is projected at %.9fs from a solve of %.9fs", run.ProjectedSeconds, run.SolveSeconds)
		}
		if 0 < run.PeakBytes && 1 < math.Abs(float64(run.ProjectedBytes-run.PeakBytes)) {
			t.Fatalf("the next run is projected at %d bytes from a peak of %d", run.ProjectedBytes, run.PeakBytes)
		}
		connect.AssertEqual(t, run.MaxSolveSeconds, jobSettings.MaxSolveSeconds)
		connect.AssertEqual(t, run.MaxSolveBytes, jobSettings.MaxSolveBytes)
		// four extenders: each expects the other three, a provider all four
		connect.AssertEqual(t, run.ExpectedExtenderPeers, 3)
		connect.AssertEqual(t, run.ExpectedProviderPeers, 4)

		// the second run starts from the first run's rows and publishes the
		// providers at the same places. The anchored extenders sit at their
		// genesis to within the rounding either way, so whether a run publishes
		// one of them is not asserted.
		firsts := map[string]*model.DerivedLocation{}
		for name, clientId := range graph.providers {
			firsts[name] = model.GetDerivedLocation(ctx, model.DerivedLocationNodeKindProvider, clientId)
		}
		second, err := deriveLocations(ctx, places, settings, DefaultDeriveLocationsSettings(), now.Add(time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		for name, clientId := range graph.providers {
			first := firsts[name]
			again := model.GetDerivedLocation(ctx, model.DerivedLocationNodeKindProvider, clientId)
			if again == nil {
				t.Fatalf("%s: the warm started run did not publish it", name)
			}
			connect.AssertEqual(t, again.LocationId, first.LocationId)
			connect.AssertEqual(t, again.UpdateTime.Unix(), now.Add(time.Minute).Unix())
			if 0.1 < geo.DistanceKm(again.Latitude, again.Longitude, first.Latitude, first.Longitude) {
				t.Fatalf("%s: the warm started run moved it %.3f km", name, geo.DistanceKm(again.Latitude, again.Longitude, first.Latitude, first.Longitude))
			}
		}
		t.Logf("sweeps per round: first %v, warm %v", result.Sweeps, second.Sweeps)

		// both runs are in the history, newest first, for the monitor's
		// run-to-run comparisons
		runs := model.GetDeriveLocationsRuns(ctx, model.DeriveLocationsRunHistory)
		connect.AssertEqual(t, len(runs), 2)
		connect.AssertEqual(t, runs[0].RunTime.Unix(), now.Add(time.Minute).Unix())
		connect.AssertEqual(t, runs[0].Published, second.Published)
		connect.AssertEqual(t, runs[1].RunTime.Unix(), now.Unix())
	})
}

// The peer gate on a seeded graph (GEOMAP §5.4, D11): two providers truly at
// Middle, placed by a wide lookup at Westport, ping with the round trips
// Middle implies, one only West and North, the other East as well. The old
// gate of two publishes both -- the first at the mirror image of Middle
// across the line through its two peers, which only the gate stood between --
// and the first run under the default gate of three refuses the first as few
// peers, removes the row the old gate left it, and publishes the other at
// Middle, the one node resting on exactly the gate.
func TestDeriveLocationsRefusesTwoPeers(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer model.Testing_PushPlaces(deriveTestPlaces)()
		places := model.CurrentPlaces()
		if places == nil {
			t.Fatal("the pushed place list is not current")
		}

		settings := deriveTestSettings()
		connect.AssertEqual(t, settings.MinDerivePeers, 3)
		// let the providers cross from Estuaire into Moyen, where they are
		settings.LambdaRegion = 0
		settings.LambdaCountry = 0
		graph := newDeriveTestGraph(t, settings)
		graph.places = places
		now := server.NowUtc()
		locations := seedDeriveTestGraph(t, ctx, graph, 8, now)
		westport := locations[9006]
		middle := deriveTestCity(t, places, 9002)

		provider := func(peers ...string) server.Id {
			clientId := server.NewId()
			model.Testing_CreateDevice(ctx, server.NewId(), server.NewId(), clientId, "", "")
			handlerId := model.CreateNetworkClientHandler(ctx)
			connectionId, _, _, _, err := model.ConnectNetworkClient(ctx, clientId, "192.0.2.96:0", handlerId)
			if err != nil {
				t.Fatal(err)
			}
			accuracyKm := float32(1000)
			connect.AssertEqual(t, model.SetConnectionLocation(ctx, connectionId, westport.LocationId, &model.ConnectionLocationScores{
				AccuracyKm:        &accuracyKm,
				GenesisLocationId: &westport.LocationId,
			}), nil)
			pings := []*model.NetworkPing{}
			for _, name := range peers {
				extenderId := graph.extenders[name]
				rttMs := deriveTestRttMs(settings, deriveTestLatLon(middle), graph.truth[deriveNodeId(model.DerivedLocationNodeKindExtender, extenderId)])
				for range 8 {
					pings = append(pings, &model.NetworkPing{
						PingerKind:       model.NetworkPingPingerKindProvider,
						PingerId:         clientId,
						TargetExtenderId: extenderId,
						ProbeNonce:       server.NewId().Bytes(),
						RttMs:            rttMs,
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
		twoPeers := provider("west", "north")
		threePeers := provider("west", "north", "east")

		// the old gate: every other gate passes the provider with two peers
		gateOfTwo := *settings
		gateOfTwo.MinDerivePeers = 2
		old, err := deriveLocations(ctx, places, &gateOfTwo, DefaultDeriveLocationsSettings(), now)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("gate of two: %+v", old)
		connect.AssertEqual(t, old.PublishRefusals.FewPeers, 0)
		connect.AssertEqual(t, old.PublishedAtMinPeers, 1)
		oldRow := model.GetDerivedLocation(ctx, model.DerivedLocationNodeKindProvider, twoPeers)
		if oldRow == nil {
			t.Fatal("the gate of two did not publish the provider with two peers")
		}
		connect.AssertEqual(t, oldRow.PeerCount, 2)
		t.Logf("the gate of two published the provider with two peers %.1f km from Middle, where it is", geo.DistanceKm(oldRow.Latitude, oldRow.Longitude, middle.Latitude, middle.Longitude))

		result, err := deriveLocations(ctx, places, settings, DefaultDeriveLocationsSettings(), now.Add(time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("gate of three: %+v", result)
		connect.AssertEqual(t, result.PublishRefusals.FewPeers, 1)
		connect.AssertEqual(t, result.PublishedAtMinPeers, 1)
		if model.GetDerivedLocation(ctx, model.DerivedLocationNodeKindProvider, twoPeers) != nil {
			t.Fatal("the row the gate of two published survived the gate of three")
		}
		row := model.GetDerivedLocation(ctx, model.DerivedLocationNodeKindProvider, threePeers)
		if row == nil {
			t.Fatal("the provider with three peers was not published")
		}
		connect.AssertEqual(t, row.PeerCount, 3)
		connect.AssertEqual(t, row.CityLocationId, locations[9002].CityLocationId)
		errorKm := geo.DistanceKm(row.Latitude, row.Longitude, middle.Latitude, middle.Longitude)
		t.Logf("the provider with three peers derived %.2f km from Middle", errorKm)
		if !(errorKm < 10) {
			t.Fatalf("the provider with three peers derived %.2f km from Middle", errorKm)
		}

		// the run carries the refusal to the monitor (SIGNALS.md §2.19c)
		run, ok := model.GetDeriveLocationsRun(ctx)
		if !ok {
			t.Fatal("the run was not recorded")
		}
		connect.AssertEqual(t, run.RunTime.Unix(), now.Add(time.Minute).Unix())
		connect.AssertEqual(t, run.RefusedFewPeers, 1)
	})
}

// Without a place list nothing is derived, and nothing is removed.
func TestDeriveLocationsWithoutAPlaceListRefuses(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer clientSession.Cancel()
		defer model.Testing_PushPlaces("not: a place list")()

		row := &model.DerivedLocation{
			NodeKind:   model.DerivedLocationNodeKindProvider,
			NodeId:     server.NewId(),
			UpdateTime: server.NowUtc(),
		}
		model.ReplaceDerivedLocations(ctx, []*model.DerivedLocation{row})
		result, err := DeriveLocations(&DeriveLocationsArgs{}, clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Refused, "no place list")
		if model.GetDerivedLocation(ctx, row.NodeKind, row.NodeId) == nil {
			t.Fatal("a refused derivation removed a row")
		}
	})
}

// The chain re-arms at the derive cadence.
func TestDeriveLocationsPostRearmsTheChain(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer clientSession.Cancel()

		before := server.NowUtc()
		server.Tx(ctx, func(tx server.PgTx) {
			if err := DeriveLocationsPost(
				&DeriveLocationsArgs{},
				&DeriveLocationsResult{},
				clientSession,
				tx,
			); err != nil {
				t.Fatalf("DeriveLocationsPost: %v", err)
			}
		})

		runAt := testExtenderTaskRunAt(t, ctx, "derive_locations")
		want := before.Add(controller.DefaultExtenderPingReportSettings().DeriveInterval)
		if runAt.Before(want.Add(-time.Second)) || want.Add(5*time.Second).Before(runAt) {
			t.Fatalf("derive run_at = %s, want about %s", runAt, want)
		}
	})
}
