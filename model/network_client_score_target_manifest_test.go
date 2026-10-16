package model

import (
	"context"
	"maps"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// What Redis holds across score export passes: every document a pass does not
// rewrite stays readable until its ttl, and a native snapshot publication
// replaces the pointer key it is recorded under.
type clientScoreExportStore struct {
	values       map[string][]byte
	nativeFacets map[string]map[ipFamilyFacet]clientScoreFacetPayload
}

func newClientScoreExportStore() *clientScoreExportStore {
	return &clientScoreExportStore{
		values:       map[string][]byte{},
		nativeFacets: map[string]map[ipFamilyFacet]clientScoreFacetPayload{},
	}
}

func (self *clientScoreExportStore) apply(capture *clientScoreExportCapture) {
	for pointerKey := range capture.nativeFacets {
		delete(self.values, pointerKey)
	}
	maps.Copy(self.values, capture.values)
	maps.Copy(self.nativeFacets, capture.nativeFacets)
}

func (self *clientScoreExportStore) get(key string) *redis.StringCmd {
	if value, ok := self.values[key]; ok {
		return redis.NewStringResult(string(value), nil)
	}
	return redis.NewStringResult("", redis.Nil)
}

// The client ids on the union pages one target serves a caller in one mode,
// read as the Online fallback reads the non-forced union.
func (self *clientScoreExportStore) unionClientIds(t testing.TB, locationId server.Id, caller server.Id, rankMode RankMode) map[server.Id]bool {
	t.Helper()
	facets, err := ipFamilyFacetsForFilter("")
	if err != nil {
		t.Fatal(err)
	}
	locationReads, locationGroupReads := newClientScoreTargetReads(
		false,
		rankMode,
		map[server.Id]bool{locationId: true},
		nil,
		caller,
		facets,
		self.get,
	)
	observation := &findProviders2LoadObservation{}
	groupSampleKeyCounts, _, err := clientScoreTargetPageCounts(
		false,
		rankMode,
		caller,
		facets,
		locationReads,
		locationGroupReads,
		observation,
	)
	if err != nil {
		t.Fatal(err)
	}
	clientIds := map[server.Id]bool{}
	for _, page := range newClientScoreCursor(groupSampleKeyCounts, observation).pages {
		sample, err := decodeClientScoreSample(self.values[page.key])
		if err != nil {
			t.Fatalf("page %s does not decode: %v", page.key, err)
		}
		for _, clientScore := range sample {
			clientIds[clientScore.ClientId] = true
		}
	}
	return clientIds
}

// The client ids on the native Speed pages one target serves a caller,
// resolving the caller's baseline alias as loadNativeClientScoresWithCursor
// does.
func (self *clientScoreExportStore) nativeClientIds(t testing.TB, locationId server.Id, caller server.Id) map[server.Id]bool {
	t.Helper()
	facets, err := ipFamilyFacetsForFilter("")
	if err != nil {
		t.Fatal(err)
	}
	keys := newClientScoreTargetKeys(false, RankModeSpeed, clientScoreTarget{id: locationId})
	nativeKey := clientScoreNativeKey(keys.counts(caller))
	if string(self.values[nativeKey]) == clientScoreNativeBaseline {
		nativeKey = clientScoreNativeKey(keys.counts(server.Id{}))
	}
	clientIds := map[server.Id]bool{}
	for _, facet := range facets {
		nativeFacet := self.nativeFacets[nativeKey][facet]
		for i := range nativeFacet.counts {
			sample, err := decodeClientScoreSample(nativeFacet.encodeSample(i))
			if err != nil {
				t.Fatalf("native page %s %s %d does not decode: %v", nativeKey, facet, i, err)
			}
			for _, clientScore := range sample {
				clientIds[clientScore.ClientId] = true
			}
		}
	}
	return clientIds
}

// One provider's row from the per-location score query.
func clientScoreStaleTargetSourceRow(clientId server.Id, networkId server.Id, cityId *server.Id, countryId *server.Id) clientScoreSourceRow {
	return clientScoreSourceRow{
		score: &ClientScore{
			ClientId:      clientId,
			NetworkId:     networkId,
			LookbackIndex: 1,
			IpFamilies:    ClientScoreIpFamilyV4,
			Scores:        map[string]int{},
			Tiers:         map[string]int{},
		},
		cityId:    cityId,
		countryId: countryId,
		egress:    &clientScoreEgress{},
	}
}

// One complete export pass over the connected providers' rows, given the
// manifest the previous pass published. Every admitted provider is ranked as
// a native Speed member, online in both modes, as rankClientScore ranks a
// provider with a passing URL ratio and no ARIN quality verification. Returns
// the documents the pass wrote and the manifest it publishes once complete.
func exportClientScoreStaleTargetPass(
	t testing.TB,
	clientLocationIds []server.Id,
	rows []clientScoreSourceRow,
	previous *clientScoreTargetManifest,
	exportedAt time.Time,
) (*clientScoreExportCapture, *clientScoreTargetManifest) {
	t.Helper()
	targetMaps := newClientScoreTargetMaps(rows, nil, func(server.Id) bool { return false })
	for _, clientScores := range targetMaps.locationClientScores {
		for _, clientScore := range clientScores {
			clientScore.Online = true
			clientScore.PassesMinimums = map[string]bool{RankModeSpeed: true}
			clientScore.ScaledWeights = map[string]float32{RankModeSpeed: 1}
			clientScore.ReliabilityWeight = 1
		}
	}
	capture := exportClientScoreTargetsForTest(t, &clientScoreEmptyTargetFixture{
		clientLocationIds:         clientLocationIds,
		excludeLocationNetworkIds: map[server.Id]map[server.Id]bool{},
		targetMaps:                targetMaps,
		previousTargets:           previous,
		exportedAt:                exportedAt,
	})
	return capture, targetMaps.targetManifest(exportedAt)
}

// A location target exists only while a source row names it
// (newClientScoreTargetMaps), and the score export writes only its targets. So
// when the last connected provider of a city leaves the score source, the next
// export neither rewrites nor clears the city's documents, and they stay
// readable until the cache ttl (300 minutes, taskworker/work
// UpdateClientScores). In that time FindProviders2 keeps drawing the city's
// previous union and native pages and lends a provider that is no longer
// connected, which no gate admits. The country, which still has a provider,
// is the control: the export rewrites it without the departed provider.
//
// Two export passes run through the production target builder, encoder and
// fanout. Redis keeps what the second pass does not rewrite. The second pass
// reads the manifest the first published, the export's record of the targets
// it wrote. The city is read back through the reader's own key builders and
// count decode, as FindProviders2's native read and its Online fallback read
// it.
func TestScoreExportStopsServingALocationWhoseProvidersAllLeft(t *testing.T) {
	cityId, countryId := server.NewId(), server.NewId()
	departedClientId, departedNetworkId := server.NewId(), server.NewId()
	stayingClientId, stayingNetworkId := server.NewId(), server.NewId()
	callerId := server.NewId()
	clientLocationIds := []server.Id{{}, callerId}
	exportedAt := time.Date(2026, 10, 15, 18, 0, 0, 0, time.UTC)

	// The first pass sees both providers. Before the second, the city's only
	// provider disconnects, so the score query no longer returns its row.
	first, manifest := exportClientScoreStaleTargetPass(t, clientLocationIds, []clientScoreSourceRow{
		clientScoreStaleTargetSourceRow(departedClientId, departedNetworkId, &cityId, &countryId),
		clientScoreStaleTargetSourceRow(stayingClientId, stayingNetworkId, nil, &countryId),
	}, nil, exportedAt)
	second, _ := exportClientScoreStaleTargetPass(t, clientLocationIds, []clientScoreSourceRow{
		clientScoreStaleTargetSourceRow(stayingClientId, stayingNetworkId, nil, &countryId),
	}, manifest, exportedAt.Add(4*time.Minute))
	// Redis keeps every document the second pass does not rewrite until its ttl.
	store := newClientScoreExportStore()
	store.apply(first)
	store.apply(second)

	for _, caller := range clientLocationIds {
		callerName := "the zero caller"
		if caller != (server.Id{}) {
			callerName = "a country caller"
		}
		for _, rankMode := range []RankMode{RankModeQuality, RankModeSpeed} {
			// the control: a target with a remaining row is rewritten
			countryClientIds := store.unionClientIds(t, countryId, caller, rankMode)
			if !countryClientIds[stayingClientId] || countryClientIds[departedClientId] {
				t.Fatalf("%s %s: the country serves %v, want only the remaining provider", callerName, rankMode, countryClientIds)
			}
			if cityClientIds := store.unionClientIds(t, cityId, caller, rankMode); 0 < len(cityClientIds) {
				t.Errorf(
					"%s %s: the city whose only provider left still serves the previous export's union pages (departed provider served: %t)",
					callerName,
					rankMode,
					cityClientIds[departedClientId],
				)
			}
		}
		if countryNativeClientIds := store.nativeClientIds(t, countryId, caller); !countryNativeClientIds[stayingClientId] || countryNativeClientIds[departedClientId] {
			t.Fatalf("%s speed: the country's native pages serve %v, want only the remaining provider", callerName, countryNativeClientIds)
		}
		if cityNativeClientIds := store.nativeClientIds(t, cityId, caller); 0 < len(cityNativeClientIds) {
			t.Errorf("%s speed: the city whose only provider left still serves the previous export's native pages (departed provider served: %t)", callerName, cityNativeClientIds[departedClientId])
		}
	}
}

// The other half of the rule: a city that still has its provider is rewritten
// with it in every pass, never cleared, and a city whose provider comes back
// serves it again on the next export. Three passes: both cities served, the
// first city's provider gone, then back.
func TestScoreExportServesALocationAgainWhenItsProviderReturns(t *testing.T) {
	returningCityId, keptCityId, countryId := server.NewId(), server.NewId(), server.NewId()
	returningClientId, returningNetworkId := server.NewId(), server.NewId()
	keptClientId, keptNetworkId := server.NewId(), server.NewId()
	callerId := server.NewId()
	clientLocationIds := []server.Id{{}, callerId}
	exportedAt := time.Date(2026, 10, 15, 18, 0, 0, 0, time.UTC)

	returningRow := func() clientScoreSourceRow {
		return clientScoreStaleTargetSourceRow(returningClientId, returningNetworkId, &returningCityId, &countryId)
	}
	keptRow := func() clientScoreSourceRow {
		return clientScoreStaleTargetSourceRow(keptClientId, keptNetworkId, &keptCityId, &countryId)
	}

	store := newClientScoreExportStore()
	var manifest *clientScoreTargetManifest
	for i, pass := range []struct {
		name          string
		rows          []clientScoreSourceRow
		returningCity bool
	}{
		{name: "both providers connected", rows: []clientScoreSourceRow{returningRow(), keptRow()}, returningCity: true},
		{name: "the returning provider gone", rows: []clientScoreSourceRow{keptRow()}, returningCity: false},
		{name: "the returning provider back", rows: []clientScoreSourceRow{returningRow(), keptRow()}, returningCity: true},
	} {
		var capture *clientScoreExportCapture
		capture, manifest = exportClientScoreStaleTargetPass(t, clientLocationIds, pass.rows, manifest, exportedAt.Add(time.Duration(i)*4*time.Minute))
		store.apply(capture)

		for _, caller := range clientLocationIds {
			for _, rankMode := range []RankMode{RankModeQuality, RankModeSpeed} {
				// the control: the kept city serves its provider in every pass
				if keptClientIds := store.unionClientIds(t, keptCityId, caller, rankMode); len(keptClientIds) != 1 || !keptClientIds[keptClientId] {
					t.Errorf("pass %d (%s) caller %t %s: the kept city serves %v, want its provider", i+1, pass.name, caller != (server.Id{}), rankMode, keptClientIds)
				}
				returningClientIds := store.unionClientIds(t, returningCityId, caller, rankMode)
				if pass.returningCity && (len(returningClientIds) != 1 || !returningClientIds[returningClientId]) {
					t.Errorf("pass %d (%s) caller %t %s: the returning city serves %v, want its provider", i+1, pass.name, caller != (server.Id{}), rankMode, returningClientIds)
				}
				if !pass.returningCity && 0 < len(returningClientIds) {
					t.Errorf("pass %d (%s) caller %t %s: the returning city serves %v while its provider is gone", i+1, pass.name, caller != (server.Id{}), rankMode, returningClientIds)
				}
			}
			if keptClientIds := store.nativeClientIds(t, keptCityId, caller); len(keptClientIds) != 1 || !keptClientIds[keptClientId] {
				t.Errorf("pass %d (%s) caller %t: the kept city's native pages serve %v, want its provider", i+1, pass.name, caller != (server.Id{}), keptClientIds)
			}
			returningClientIds := store.nativeClientIds(t, returningCityId, caller)
			if pass.returningCity && (len(returningClientIds) != 1 || !returningClientIds[returningClientId]) {
				t.Errorf("pass %d (%s) caller %t: the returning city's native pages serve %v, want its provider", i+1, pass.name, caller != (server.Id{}), returningClientIds)
			}
			if !pass.returningCity && 0 < len(returningClientIds) {
				t.Errorf("pass %d (%s) caller %t: the returning city's native pages serve %v while its provider is gone", i+1, pass.name, caller != (server.Id{}), returningClientIds)
			}
		}
	}
}

// The manifest remembers a departed target for one cache ttl after it was
// last written from rows, and no longer: its documents have expired by then.
// A manifest published ahead of an export that never finished still names the
// targets it had to clear, and the manifest a complete export publishes names
// its row targets only, so the memory never grows past one ttl of targets.
func TestClientScoreTargetManifestRemembersDepartedTargetsForOneTtl(t *testing.T) {
	now := time.Date(2026, 10, 15, 18, 0, 0, 0, time.UTC)
	ttl := clientScoreExportTestTtl
	rowLocationId, departedLocationId, expiredLocationId, boundaryLocationId := server.NewId(), server.NewId(), server.NewId(), server.NewId()
	knownGroupId, departedGroupId, expiredGroupId := server.NewId(), server.NewId(), server.NewId()
	countryId := server.NewId()
	clientId, networkId := server.NewId(), server.NewId()

	targetMaps := newClientScoreTargetMaps(
		[]clientScoreSourceRow{clientScoreStaleTargetSourceRow(clientId, networkId, &rowLocationId, &countryId)},
		[]server.Id{knownGroupId},
		func(server.Id) bool { return false },
	)
	departedAt := now.Add(-time.Hour).Unix()
	previous := &clientScoreTargetManifest{
		locationIds: map[server.Id]int64{
			rowLocationId:      now.Add(-4 * time.Minute).Unix(),
			departedLocationId: departedAt,
			// written exactly one ttl ago: its documents have just expired
			expiredLocationId: now.Add(-ttl).Unix(),
			// one second younger: still readable
			boundaryLocationId: now.Add(-ttl + time.Second).Unix(),
		},
		locationGroupIds: map[server.Id]int64{
			knownGroupId:    now.Add(-4 * time.Minute).Unix(),
			departedGroupId: departedAt,
			expiredGroupId:  now.Add(-2 * ttl).Unix(),
		},
	}

	type targetKey struct {
		id            server.Id
		locationGroup bool
	}
	departed, writeAhead := targetMaps.departedTargets(previous, now, ttl)
	departedKeys := map[targetKey]bool{}
	for _, target := range departed {
		if len(target.clientScores) != 0 {
			t.Errorf("departed target %s has %d providers, want none", target.id, len(target.clientScores))
		}
		departedKeys[targetKey{id: target.id, locationGroup: target.locationGroup}] = true
	}
	wantDeparted := map[targetKey]bool{
		{id: departedLocationId}:                   true,
		{id: boundaryLocationId}:                   true,
		{id: departedGroupId, locationGroup: true}: true,
	}
	if !maps.Equal(departedKeys, wantDeparted) {
		t.Errorf("departed targets = %v, want %v", departedKeys, wantDeparted)
	}

	// published before any document: the row targets at now, the departed
	// targets at the time they were last written from rows
	wantWriteAhead := &clientScoreTargetManifest{
		locationIds: map[server.Id]int64{
			rowLocationId:      now.Unix(),
			countryId:          now.Unix(),
			departedLocationId: departedAt,
			boundaryLocationId: previous.locationIds[boundaryLocationId],
		},
		locationGroupIds: map[server.Id]int64{
			knownGroupId:    now.Unix(),
			departedGroupId: departedAt,
		},
	}
	if !maps.Equal(writeAhead.locationIds, wantWriteAhead.locationIds) || !maps.Equal(writeAhead.locationGroupIds, wantWriteAhead.locationGroupIds) {
		t.Errorf("manifest published ahead = %v, want %v", writeAhead, wantWriteAhead)
	}

	// if that export dies, the next one still clears the departed targets
	// from the manifest it published ahead
	nextMaps := newClientScoreTargetMaps(
		[]clientScoreSourceRow{clientScoreStaleTargetSourceRow(clientId, networkId, &rowLocationId, &countryId)},
		[]server.Id{knownGroupId},
		func(server.Id) bool { return false },
	)
	retried, _ := nextMaps.departedTargets(writeAhead, now.Add(4*time.Minute), ttl)
	retriedKeys := map[targetKey]bool{}
	for _, target := range retried {
		retriedKeys[targetKey{id: target.id, locationGroup: target.locationGroup}] = true
	}
	// the boundary target's documents expire within those four minutes
	delete(wantDeparted, targetKey{id: boundaryLocationId})
	if !maps.Equal(retriedKeys, wantDeparted) {
		t.Errorf("after an export that died, departed targets = %v, want %v", retriedKeys, wantDeparted)
	}

	// a complete export publishes its row targets only
	finishedAt := now.Add(3 * time.Minute)
	complete := targetMaps.targetManifest(finishedAt)
	if !maps.Equal(complete.locationIds, map[server.Id]int64{rowLocationId: finishedAt.Unix(), countryId: finishedAt.Unix()}) ||
		!maps.Equal(complete.locationGroupIds, map[server.Id]int64{knownGroupId: finishedAt.Unix()}) {
		t.Errorf("manifest of a complete export = %v, want the row targets at its end", complete)
	}

	// without a previous manifest nothing departs
	if departed, manifest := targetMaps.departedTargets(nil, now, ttl); len(departed) != 0 || len(manifest.locationIds) != 2 || len(manifest.locationGroupIds) != 1 {
		t.Errorf("without a previous manifest: %d departed targets and manifest %v, want none and the row targets", len(departed), manifest)
	}
}

// The manifest's Redis document round-trips, and a document that does not
// decode is an error the reader turns into "no previous manifest".
func TestClientScoreTargetManifestDocumentRoundTrip(t *testing.T) {
	manifest := &clientScoreTargetManifest{
		locationIds:      map[server.Id]int64{server.NewId(): 1792000000, server.NewId(): 1792000240},
		locationGroupIds: map[server.Id]int64{server.NewId(): 1792000000},
	}
	value, err := encodeClientScoreTargetManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeClientScoreTargetManifest(value)
	if err != nil {
		t.Fatal(err)
	}
	if !maps.Equal(decoded.locationIds, manifest.locationIds) || !maps.Equal(decoded.locationGroupIds, manifest.locationGroupIds) {
		t.Errorf("decoded manifest = %v, want %v", decoded, manifest)
	}

	empty, err := encodeClientScoreTargetManifest(newClientScoreTargetManifest())
	if err != nil {
		t.Fatal(err)
	}
	if decoded, err := decodeClientScoreTargetManifest(empty); err != nil || len(decoded.locationIds) != 0 || len(decoded.locationGroupIds) != 0 {
		t.Errorf("empty manifest decodes to %v, %v", decoded, err)
	}

	for _, value := range []string{
		"not json",
		`{"location_ids": {"not an id": 1792000000}}`,
		`{"location_group_ids": {"not an id": 1792000000}}`,
		`{"location_ids": ["a list"]}`,
	} {
		if _, err := decodeClientScoreTargetManifest([]byte(value)); err == nil {
			t.Errorf("%q decodes", value)
		}
	}
}

// Two real exports over the database and Redis: the only provider of a city
// leaves the score source between them. The second export publishes the
// city's documents encoded empty, so FindProviders2 reads the city as
// cache_empty instead of lending the departed provider. The city remains in
// cleanup metadata through its old payload deadline in case an older export
// is still publishing; its already-cleared payload stays empty.
func TestUpdateClientScoresClearsALocationWhoseProvidersAllLeft(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		t.Cleanup(server.Config.PushSimpleResource(
			providerConfigResourceName,
			[]byte("enable_egress_test: false\n"),
		))

		city := &Location{
			LocationType: LocationTypeCity,
			City:         "Palo Alto",
			Region:       "California",
			Country:      "United States",
			CountryCode:  "us",
		}
		CreateLocation(ctx, city)
		clientIds := testing_connectQualifyingProviders(ctx, t, city, 1)

		// the forced union carries every admitted provider, whatever its rank
		forcedProviderCount := func() int {
			providerCount := 0
			server.Redis(ctx, func(r server.RedisClient) {
				for _, rankMode := range []RankMode{RankModeQuality, RankModeSpeed} {
					for _, facet := range ipFamilyFacets {
						value, err := r.Get(ctx, clientScoreLocationFacetCountsKey(true, rankMode, city.LocationId, server.Id{}, facet)).Bytes()
						if err != nil {
							t.Fatalf("city %s facet %s count document is not published: %v", rankMode, facet, err)
						}
						for _, count := range decodeClientScoreCountsForTest(t, value) {
							providerCount += count
						}
					}
				}
			})
			return providerCount
		}
		manifestNamesCity := func() bool {
			manifest, err := readClientScoreTargetManifest(ctx)
			if err != nil || manifest == nil {
				t.Fatalf("target manifest = %v, %v", manifest, err)
			}
			_, ok := manifest.locationIds[city.LocationId]
			return ok
		}

		connect.AssertEqual(t, UpdateClientScores(ctx, time.Hour, 1), nil)
		if providerCount := forcedProviderCount(); providerCount != 2 {
			t.Fatalf("first export: the city's forced pools hold %d providers, want its provider in both modes", providerCount)
		}
		if !manifestNamesCity() {
			t.Fatal("first export: the target manifest does not name the city")
		}

		// the provider disconnects: the score source no longer returns it
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`UPDATE network_client_location_reliability SET connected = false WHERE client_id = $1`,
				clientIds[0],
			))
		})
		connect.AssertEqual(t, UpdateClientScores(ctx, time.Hour, 1), nil)
		if providerCount := forcedProviderCount(); providerCount != 0 {
			t.Fatalf("second export: the city's forced pools still hold %d providers of the previous export", providerCount)
		}
		if !manifestNamesCity() {
			t.Fatal("second export: completion forgot a target an older export may still publish")
		}

		clientSession := testingCreateProviderSearchSession(ctx, session.NewByJwt(server.NewId(), server.NewId(), "departed-target-test", false, false))
		reasonCount := func(reason string) float64 {
			count := float64(0)
			for _, targetKind := range []string{"city", "location_unknown"} {
				count += selectionMetricCount(t, "urnetwork_findproviders2_selection_outcomes_total", map[string]string{
					"target_kind": targetKind, "request_class": "default_minimum", "ip_family": "any",
					"rank_mode": "quality", "outcome": "zero", "reason": reason,
				})
			}
			return count
		}
		emptyBefore, missingBefore := reasonCount("cache_empty"), reasonCount("cache_missing")
		result, err := FindProviders2(&FindProviders2Args{Specs: []*ProviderSpec{{LocationId: &city.LocationId}}}, clientSession)
		if err != nil || result == nil || len(result.Providers) != 0 {
			t.Fatalf("result = %v, err = %v, want an empty result", result, err)
		}
		if emptyAfter, missingAfter := reasonCount("cache_empty"), reasonCount("cache_missing"); emptyAfter != emptyBefore+1 || missingAfter != missingBefore {
			t.Fatalf("cache_empty +%g and cache_missing +%g, want +1 and +0", emptyAfter-emptyBefore, missingAfter-missingBefore)
		}
	})
}
