// A score export target whose every provider is hard-excluded, or a known
// location group no source row names, must still be published as encoded
// empty documents. Otherwise the reader reports cache_missing, the same reason
// a broken writer produces, and a target that loses its last admitted provider
// keeps serving its previous payload until the cache ttl.
package model

import (
	"bytes"
	"context"
	"encoding/gob"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// P1 is hard-excluded and the only provider of location L1 and group G1. The
// admitted P2 is the only provider of location L2 and group G3. G2 is a known
// group that no source row names. G1 is deliberately not a known group, so
// only its excluded row can create it and each half of the export's target
// rule is tested on its own. Every identity is generated per test.
type clientScoreEmptyTargetFixture struct {
	excludedClientId        server.Id
	excludedNetworkId       server.Id
	eligibleClientId        server.Id
	eligibleNetworkId       server.Id
	excludedLocationId      server.Id
	eligibleLocationId      server.Id
	excludedLocationGroupId server.Id
	emptyLocationGroupId    server.Id
	eligibleLocationGroupId server.Id
	// the zero caller, a caller without exclusions, and one caller blocking
	// each provider's network
	clientLocationIds         []server.Id
	excludeLocationNetworkIds map[server.Id]map[server.Id]bool
	blockEligibleCallerId     server.Id
	targetMaps                *clientScoreTargetMaps
	// the manifest the previous export published, if any, and this export's
	// instant (network_client_score_target_manifest.go)
	previousTargets *clientScoreTargetManifest
	exportedAt      time.Time
}

// the taskworker's score cache ttl (taskworker/work UpdateClientScores)
const clientScoreExportTestTtl = 300 * time.Minute

// Builds the target maps through the production builder, then marks the
// admitted P2 online as the ranking pass would.
func newClientScoreEmptyTargetFixture() *clientScoreEmptyTargetFixture {
	self := &clientScoreEmptyTargetFixture{
		excludedClientId:        server.NewId(),
		excludedNetworkId:       server.NewId(),
		eligibleClientId:        server.NewId(),
		eligibleNetworkId:       server.NewId(),
		excludedLocationId:      server.NewId(),
		eligibleLocationId:      server.NewId(),
		excludedLocationGroupId: server.NewId(),
		emptyLocationGroupId:    server.NewId(),
		eligibleLocationGroupId: server.NewId(),
		blockEligibleCallerId:   server.NewId(),
	}
	unfilteredCallerId := server.NewId()
	blockExcludedCallerId := server.NewId()
	self.clientLocationIds = []server.Id{{}, unfilteredCallerId, blockExcludedCallerId, self.blockEligibleCallerId}
	self.excludeLocationNetworkIds = map[server.Id]map[server.Id]bool{
		blockExcludedCallerId:      {self.excludedNetworkId: true},
		self.blockEligibleCallerId: {self.eligibleNetworkId: true},
	}

	// each source query yields its own lookback copy of a provider
	lookbackClientScore := func(clientId server.Id, networkId server.Id) *ClientScore {
		return &ClientScore{
			ClientId:      clientId,
			NetworkId:     networkId,
			LookbackIndex: 1,
			Scores:        map[string]int{},
			Tiers:         map[string]int{},
		}
	}
	rows := []clientScoreSourceRow{
		{
			score:     lookbackClientScore(self.excludedClientId, self.excludedNetworkId),
			countryId: &self.excludedLocationId,
			egress:    &clientScoreEgress{},
		},
		{
			score:            lookbackClientScore(self.excludedClientId, self.excludedNetworkId),
			egress:           &clientScoreEgress{},
			locationGroup:    true,
			locationGroupIds: []server.Id{self.excludedLocationGroupId},
		},
		{
			score:     lookbackClientScore(self.eligibleClientId, self.eligibleNetworkId),
			countryId: &self.eligibleLocationId,
			egress:    &clientScoreEgress{},
		},
		{
			score:            lookbackClientScore(self.eligibleClientId, self.eligibleNetworkId),
			egress:           &clientScoreEgress{},
			locationGroup:    true,
			locationGroupIds: []server.Id{self.eligibleLocationGroupId},
		},
	}
	self.targetMaps = newClientScoreTargetMaps(
		rows,
		[]server.Id{self.emptyLocationGroupId, self.eligibleLocationGroupId},
		func(clientId server.Id) bool {
			return clientId == self.excludedClientId
		},
	)
	// what rankClientScore decides for an admitted provider without native
	// membership: online, so both modes' union pools carry it
	for _, targetClientScores := range []map[server.Id]map[server.Id]*ClientScore{
		self.targetMaps.locationClientScores,
		self.targetMaps.locationGroupClientScores,
	} {
		for _, clientScores := range targetClientScores {
			for _, clientScore := range clientScores {
				clientScore.Online = true
			}
		}
	}
	return self
}

// The documents one export pass emits, by key. A native snapshot is recorded
// by its pointer key instead of being published to Redis.
type clientScoreExportCapture struct {
	values       map[string][]byte
	nativeFacets map[string]map[ipFamilyFacet]clientScoreFacetPayload
}

// Runs every target through the production key builders, encoder and fanout
// in all four export modes, as one UpdateClientScores pass does after its
// alias and ip family migrations completed: the targets of the maps, then the
// departed targets of the previous manifest.
func exportClientScoreTargetsForTest(t testing.TB, fixture *clientScoreEmptyTargetFixture) *clientScoreExportCapture {
	t.Helper()
	capture := &clientScoreExportCapture{
		values:       map[string][]byte{},
		nativeFacets: map[string]map[ipFamilyFacet]clientScoreFacetPayload{},
	}
	emit := func(set clientScoreRedisSet) error {
		if !set.flush {
			capture.values[set.key] = set.value
		}
		return nil
	}
	departedTargets, _ := fixture.targetMaps.departedTargets(fixture.previousTargets, fixture.exportedAt, clientScoreExportTestTtl)
	for _, target := range append(fixture.targetMaps.targets(), departedTargets...) {
		for _, forceMinimum := range []bool{false, true} {
			for rankMode := range clientScorePerformanceTargets {
				keys := newClientScoreTargetKeys(forceMinimum, rankMode, target)
				var nativeFanout *clientScoreNativeFanout
				if !forceMinimum {
					// production aliases; only the Redis snapshot write is replaced
					nativeFanout = newClientScoreNativeFanout(context.Background(), nil, keys.counts, time.Hour, nil, emit)
					nativeFanout.publish = func(callerId server.Id, facets map[ipFamilyFacet]clientScoreFacetPayload) error {
						capture.nativeFacets[clientScoreNativeKey(keys.counts(callerId))] = facets
						return nil
					}
				}
				err := emitClientScoreTargetFanout(
					fixture.clientLocationIds,
					target.clientScores,
					fixture.excludeLocationNetworkIds,
					keys,
					func(clientScores map[server.Id]*ClientScore) clientScoreExportPayload {
						return exportClientScores(forceMinimum, rankMode, clientScores)
					},
					false,
					false,
					emit,
					nativeFanout,
				)
				if err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	return capture
}

// The page counts of one count document, decoded as the reader does.
func decodeClientScoreCountsForTest(t testing.TB, value []byte) []int {
	t.Helper()
	var counts []int
	if err := gob.NewDecoder(bytes.NewReader(value)).Decode(&counts); err != nil {
		t.Fatalf("count document does not decode: %v", err)
	}
	return counts
}

// Every target a source row names is created even when the row's provider is
// hard-excluded, and every known group is a target even when no row names it.
// Only an admitted provider joins a target's client set.
func TestClientScoreTargetMapsKeepHardExcludedAndKnownEmptyTargets(t *testing.T) {
	fixture := newClientScoreEmptyTargetFixture()

	targetClientIds := map[server.Id]map[server.Id]bool{}
	targetLocationGroups := map[server.Id]bool{}
	for _, target := range fixture.targetMaps.targets() {
		if _, ok := targetClientIds[target.id]; ok {
			t.Fatalf("target %s is listed twice", target.id)
		}
		clientIds := map[server.Id]bool{}
		for clientId := range target.clientScores {
			clientIds[clientId] = true
		}
		targetClientIds[target.id] = clientIds
		targetLocationGroups[target.id] = target.locationGroup
	}

	for _, c := range []struct {
		name          string
		targetId      server.Id
		locationGroup bool
		clientIds     []server.Id
	}{
		{name: "location L1 of the hard-excluded P1", targetId: fixture.excludedLocationId},
		{name: "group G1 of the hard-excluded P1", targetId: fixture.excludedLocationGroupId, locationGroup: true},
		{name: "known group G2 without rows", targetId: fixture.emptyLocationGroupId, locationGroup: true},
		{name: "location L2 of the admitted P2", targetId: fixture.eligibleLocationId, clientIds: []server.Id{fixture.eligibleClientId}},
		{name: "group G3 of the admitted P2", targetId: fixture.eligibleLocationGroupId, locationGroup: true, clientIds: []server.Id{fixture.eligibleClientId}},
	} {
		clientIds, ok := targetClientIds[c.targetId]
		if !ok {
			t.Errorf("%s is not an export target", c.name)
			continue
		}
		if targetLocationGroups[c.targetId] != c.locationGroup {
			t.Errorf("%s location group = %t, want %t", c.name, targetLocationGroups[c.targetId], c.locationGroup)
		}
		if len(clientIds) != len(c.clientIds) {
			t.Errorf("%s has %d providers, want %d", c.name, len(clientIds), len(c.clientIds))
		}
		for _, clientId := range c.clientIds {
			if !clientIds[clientId] {
				t.Errorf("%s is missing its admitted provider", c.name)
			}
		}
	}
	if len(targetClientIds) != 5 {
		t.Errorf("export has %d targets, want the 5 named by rows or known groups", len(targetClientIds))
	}

	// the exclusion and egress bookkeeping is unchanged
	if !fixture.targetMaps.hardExcludedClientIds[fixture.excludedClientId] || len(fixture.targetMaps.hardExcludedClientIds) != 1 {
		t.Errorf("hard-excluded providers = %v, want only P1", fixture.targetMaps.hardExcludedClientIds)
	}
	if _, ok := fixture.targetMaps.clientIdEgresses[fixture.excludedClientId]; ok {
		t.Error("the hard-excluded P1 received an egress decision")
	}
	if _, ok := fixture.targetMaps.clientIdEgresses[fixture.eligibleClientId]; !ok {
		t.Error("the admitted P2 has no egress decision")
	}
	if eligibleClientScore := fixture.targetMaps.locationClientScores[fixture.eligibleLocationId][fixture.eligibleClientId]; eligibleClientScore == nil || eligibleClientScore.CountryLocationId == nil || *eligibleClientScore.CountryLocationId != fixture.eligibleLocationId {
		t.Error("the admitted P2 lost its location ids")
	}
}

// An empty target is exported in every mode as encoded-empty count documents
// and a zero filter under the shared zero-caller baseline, with no sample
// pages, and every other caller gets the baseline alias, in the legacy and the
// native schema. The admitted target is the control: its caller that blocks
// P2's network still gets its own override.
func TestClientScoreEmptyTargetsPublishEncodedEmptyDocuments(t *testing.T) {
	fixture := newClientScoreEmptyTargetFixture()
	capture := exportClientScoreTargetsForTest(t, fixture)

	for _, c := range []struct {
		name          string
		targetId      server.Id
		locationGroup bool
		empty         bool
		providerCount int
	}{
		{name: "location L1", targetId: fixture.excludedLocationId, empty: true},
		{name: "group G1", targetId: fixture.excludedLocationGroupId, locationGroup: true, empty: true},
		{name: "group G2", targetId: fixture.emptyLocationGroupId, locationGroup: true, empty: true},
		{name: "location L2", targetId: fixture.eligibleLocationId, providerCount: 1},
		{name: "group G3", targetId: fixture.eligibleLocationGroupId, locationGroup: true, providerCount: 1},
	} {
		target := clientScoreTarget{id: c.targetId, locationGroup: c.locationGroup}
		for _, forceMinimum := range []bool{false, true} {
			for rankMode := range clientScorePerformanceTargets {
				keys := newClientScoreTargetKeys(forceMinimum, rankMode, target)
				mode := "normal"
				if forceMinimum {
					mode = "forced"
				}

				providerCount := 0
				for _, facet := range ipFamilyFacets {
					value, ok := capture.values[keys.facetCounts(server.Id{}, facet)]
					if !ok {
						t.Errorf("%s %s %s: facet %s count document is absent", c.name, mode, rankMode, facet)
						continue
					}
					counts := decodeClientScoreCountsForTest(t, value)
					if c.empty && len(counts) != 0 {
						t.Errorf("%s %s %s: facet %s counts = %v, want encoded empty", c.name, mode, rankMode, facet, counts)
					}
					for _, count := range counts {
						providerCount += count
					}
					if c.empty {
						if _, ok := capture.values[keys.facetSample(server.Id{}, facet, 0)]; ok {
							t.Errorf("%s %s %s: facet %s has a sample page", c.name, mode, rankMode, facet)
						}
					}
				}
				if providerCount != c.providerCount {
					t.Errorf("%s %s %s: baseline holds %d providers, want %d", c.name, mode, rankMode, providerCount, c.providerCount)
				}

				if value, ok := capture.values[keys.filter(server.Id{})]; !ok {
					t.Errorf("%s %s %s: filter document is absent", c.name, mode, rankMode)
				} else {
					var filter ClientFilter
					if err := gob.NewDecoder(bytes.NewReader(value)).Decode(&filter); err != nil {
						t.Fatalf("%s %s %s: filter does not decode: %v", c.name, mode, rankMode, err)
					}
					if c.empty && filter.Count != 0 {
						t.Errorf("%s %s %s: filter count = %d, want 0", c.name, mode, rankMode, filter.Count)
					}
				}

				for _, callerId := range fixture.clientLocationIds[1:] {
					wantAlias := clientScoreAliasBaselineValue
					if !c.empty && callerId == fixture.blockEligibleCallerId {
						wantAlias = clientScoreAliasCallerValue
					}
					if alias := string(capture.values[keys.alias(callerId)]); alias != wantAlias {
						t.Errorf("%s %s %s: caller alias = %q, want %q", c.name, mode, rankMode, alias, wantAlias)
					}
					if _, ok := capture.values[keys.facetCounts(callerId, ipFamilyFacetV4Only)]; ok != (wantAlias == clientScoreAliasCallerValue) {
						t.Errorf("%s %s %s: caller-specific counts present = %t with alias %q", c.name, mode, rankMode, ok, wantAlias)
					}
				}

				if forceMinimum {
					continue
				}
				nativeFacets, ok := capture.nativeFacets[clientScoreNativeKey(keys.counts(server.Id{}))]
				if !ok {
					t.Errorf("%s %s %s: native baseline snapshot is absent", c.name, mode, rankMode)
				} else if c.empty {
					for _, facet := range ipFamilyFacets {
						if nativeFacet, ok := nativeFacets[facet]; !ok || len(nativeFacet.counts) != 0 {
							t.Errorf("%s %s %s: native facet %s = %v (present %t), want present and empty", c.name, mode, rankMode, facet, nativeFacet.counts, ok)
						}
					}
				}
				for _, callerId := range fixture.clientLocationIds[1:] {
					nativeAlias, ok := capture.values[clientScoreNativeKey(keys.counts(callerId))]
					if c.empty || callerId != fixture.blockEligibleCallerId {
						if !ok || string(nativeAlias) != clientScoreNativeBaseline {
							t.Errorf("%s %s %s: caller native alias = %q (present %t), want baseline", c.name, mode, rankMode, nativeAlias, ok)
						}
					}
				}
			}
		}
	}
}

// The reader's own key builders and count decode turn the published empty
// documents into a cache_empty observation, for every caller, export mode, ip
// family filter, and both rank modes FindProviders2 reads for one request.
// Without them the same reads find nothing and report cache_missing, which a
// broken writer also produces.
func TestClientScoreEmptyTargetsReadAsCacheEmpty(t *testing.T) {
	fixture := newClientScoreEmptyTargetFixture()
	capture := exportClientScoreTargetsForTest(t, fixture)
	get := func(key string) *redis.StringCmd {
		if value, ok := capture.values[key]; ok {
			return redis.NewStringResult(string(value), nil)
		}
		return redis.NewStringResult("", redis.Nil)
	}

	for _, c := range []struct {
		name             string
		locationIds      map[server.Id]bool
		locationGroupIds map[server.Id]bool
		spec             *ProviderSpec
	}{
		{
			name:        "location L1",
			locationIds: map[server.Id]bool{fixture.excludedLocationId: true},
			spec:        &ProviderSpec{LocationId: &fixture.excludedLocationId},
		},
		{
			name:             "group G1",
			locationGroupIds: map[server.Id]bool{fixture.excludedLocationGroupId: true},
			spec:             &ProviderSpec{LocationGroupId: &fixture.excludedLocationGroupId},
		},
		{
			name:             "group G2",
			locationGroupIds: map[server.Id]bool{fixture.emptyLocationGroupId: true},
			spec:             &ProviderSpec{LocationGroupId: &fixture.emptyLocationGroupId},
		},
	} {
		for _, forceMinimum := range []bool{false, true} {
			for _, ipFamilyFilter := range []connect.IpFamilyFilter{
				connect.IpFamilyFilterDefault,
				connect.IpFamilyFilterV6Capable,
				connect.IpFamilyFilterDualstack,
				connect.IpFamilyFilterV4Only,
				connect.IpFamilyFilterV6Only,
			} {
				ipFamily := string(ipFamilyFilter)
				facets, err := ipFamilyFacetsForFilter(ipFamily)
				if err != nil {
					t.Fatal(err)
				}
				for _, callerId := range fixture.clientLocationIds {
					observation := newFindProviders2SelectionObservation(&FindProviders2Args{
						Specs:        []*ProviderSpec{c.spec},
						ForceMinimum: forceMinimum,
						IpFamily:     ipFamily,
					})
					observation.discovery = true
					for _, rankMode := range []RankMode{RankModeQuality, RankModeSpeed} {
						locationReads, locationGroupReads := newClientScoreTargetReads(
							forceMinimum,
							rankMode,
							c.locationIds,
							c.locationGroupIds,
							callerId,
							facets,
							get,
						)
						groupSampleKeyCounts, sourceIncomplete, err := clientScoreTargetPageCounts(
							forceMinimum,
							rankMode,
							callerId,
							facets,
							locationReads,
							locationGroupReads,
							&observation.load,
						)
						if err != nil {
							t.Fatalf("%s: count decode failed: %v", c.name, err)
						}
						if sourceIncomplete {
							t.Errorf("%s forced=%t family=%q %s: source incomplete", c.name, forceMinimum, ipFamily, rankMode)
						}
						if newClientScoreCursor(groupSampleKeyCounts, &observation.load).hasMore() {
							t.Errorf("%s forced=%t family=%q %s: empty target has sample pages", c.name, forceMinimum, ipFamily, rankMode)
						}
					}
					observation.complete(0)
					observation.finish(context.Background())
					if observation.reason != "cache_empty" {
						t.Errorf("%s forced=%t family=%q caller=%t: reason = %s with %d missing targets, want cache_empty", c.name, forceMinimum, ipFamily, callerId != (server.Id{}), observation.reason, observation.load.missingTargets)
					}
				}
			}
		}
	}
}

// One real pass over the database: the only provider of a city is
// hard-excluded, one group holds that city and another has no members. All
// three targets now read through FindProviders2 as cache_empty, never
// cache_missing.
func TestUpdateClientScoresPublishesEmptyTargets(t *testing.T) {
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
		excludedGroup := &LocationGroup{
			Name: "Synthetic Excluded Supply Group",
			MemberLocationIds: []server.Id{
				city.CityLocationId,
				city.RegionLocationId,
				city.CountryLocationId,
			},
		}
		CreateLocationGroup(ctx, excludedGroup)
		emptyGroup := &LocationGroup{
			Name:              "Synthetic Memberless Group",
			MemberLocationIds: []server.Id{},
		}
		CreateLocationGroup(ctx, emptyGroup)

		clientIds := testing_connectQualifyingProviders(ctx, t, city, 1)
		SetProviderEgressHealth(ctx, &ProviderEgressHealth{
			ClientId: clientIds[0], MeasuredAt: server.NowUtc(),
			OKCount: 130, Total: 131, TLSAuthenticationFailure: true,
		})
		connect.AssertEqual(t, UpdateClientScores(ctx, time.Hour, 1), nil)

		server.Redis(ctx, func(r server.RedisClient) {
			for _, key := range []string{
				clientScoreLocationFacetCountsKey(false, RankModeQuality, city.LocationId, server.Id{}, ipFamilyFacetV4Only),
				clientScoreLocationGroupFacetCountsKey(false, RankModeQuality, excludedGroup.LocationGroupId, server.Id{}, ipFamilyFacetV4Only),
				clientScoreLocationGroupFacetCountsKey(true, RankModeSpeed, emptyGroup.LocationGroupId, server.Id{}, ipFamilyFacetDualstack),
			} {
				value, err := r.Get(ctx, key).Bytes()
				if err != nil {
					t.Fatalf("empty target count document is not published: %v", err)
				}
				if counts := decodeClientScoreCountsForTest(t, value); len(counts) != 0 {
					t.Fatalf("empty target counts = %v, want encoded empty", counts)
				}
			}
		})

		clientSession := testingCreateProviderSearchSession(ctx, session.NewByJwt(server.NewId(), server.NewId(), "empty-target-test", false, false))
		for _, c := range []struct {
			name        string
			spec        *ProviderSpec
			targetKinds []string
		}{
			{name: "city", spec: &ProviderSpec{LocationId: &city.LocationId}, targetKinds: []string{"city", "location_unknown"}},
			{name: "excluded group", spec: &ProviderSpec{LocationGroupId: &excludedGroup.LocationGroupId}, targetKinds: []string{"group"}},
			{name: "memberless group", spec: &ProviderSpec{LocationGroupId: &emptyGroup.LocationGroupId}, targetKinds: []string{"group"}},
		} {
			reasonCount := func(reason string) float64 {
				count := float64(0)
				for _, targetKind := range c.targetKinds {
					count += selectionMetricCount(t, "urnetwork_findproviders2_selection_outcomes_total", map[string]string{
						"target_kind": targetKind, "request_class": "default_minimum", "ip_family": "any",
						"rank_mode": "quality", "outcome": "zero", "reason": reason,
					})
				}
				return count
			}
			emptyBefore, missingBefore := reasonCount("cache_empty"), reasonCount("cache_missing")
			result, err := FindProviders2(&FindProviders2Args{Specs: []*ProviderSpec{c.spec}}, clientSession)
			if err != nil || result == nil || len(result.Providers) != 0 {
				t.Fatalf("%s: result = %v, err = %v, want an empty result", c.name, result, err)
			}
			if emptyAfter, missingAfter := reasonCount("cache_empty"), reasonCount("cache_missing"); emptyAfter != emptyBefore+1 || missingAfter != missingBefore {
				t.Fatalf("%s: cache_empty +%g and cache_missing +%g, want +1 and +0", c.name, emptyAfter-emptyBefore, missingAfter-missingBefore)
			}
		}
	})
}
