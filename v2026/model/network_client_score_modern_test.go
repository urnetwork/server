package model

// The modern-first page partition (network_client_score_modern.go),
// modern-first priority and modern-only requests (FP2-MODERN.md). The pages
// are published through the export's own encoder and fan-out, so the readers
// see what UpdateClientScores writes.

import (
	"bytes"
	"crypto/sha256"
	"encoding/gob"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
)

// A synthetic score: a native of both modes or an online member, in one
// family facet, modern or not.
func modernTestScore(modern bool, facet ipFamilyFacet, native bool) *ClientScore {
	score := onlineBackfillScore(!native, 1)
	score.Online = true
	if facet == ipFamilyFacetDualstack {
		score.IpFamilies = ClientScoreIpFamilyV4 | ClientScoreIpFamilyV6
	}
	score.Modern = modern
	return score
}

func modernTestScores(n int, modern bool, facet ipFamilyFacet, native bool) []*ClientScore {
	scores := make([]*ClientScore, 0, n)
	for range n {
		scores = append(scores, modernTestScore(modern, facet, native))
	}
	return scores
}

// Pushes provider.yml for the test, read by the request settings at once.
func modernTestProviderConfig(t testing.TB, config string) {
	t.Helper()
	pop := server.Config.PushSimpleResource(providerConfigResourceName, []byte(config))
	requestEgressIndexSettingsSnapshot.Store(nil)
	t.Cleanup(func() {
		pop()
		requestEgressIndexSettingsSnapshot.Store(nil)
	})
}

const modernTestNativeReaderConfig = "egress_index:\n  native_reader_enabled: true\n"
const modernTestUnionReaderConfig = "egress_index:\n  native_reader_enabled: false\n"

// Publishes one location's scores through the export's encoder and fan-out,
// both pools and both modes, as UpdateClientScores writes a target, with the
// hard-exclusion set ready.
func modernTestPublishLocation(t testing.TB, location server.Id, scores []*ClientScore) {
	t.Helper()
	ctx := t.Context()
	clientScores := map[server.Id]*ClientScore{}
	for _, score := range scores {
		clientScores[score.ClientId] = score
	}
	callerIds := []server.Id{{}}
	callerIds = append(callerIds, slices.Collect(maps.Values(countryCodeLocationIds()))...)
	target := clientScoreTarget{id: location, clientScores: clientScores}
	server.Redis(ctx, func(r server.RedisClient) {
		for _, forceMinimum := range []bool{false, true} {
			for _, mode := range []RankMode{RankModeQuality, RankModeSpeed} {
				keys := newClientScoreTargetKeys(forceMinimum, mode, target)
				err := writeClientScoreRedisStream(ctx, r, time.Hour, func(emit func(clientScoreRedisSet) error) error {
					var nativeFanout *clientScoreNativeFanout
					if !forceMinimum {
						nativeFanout = newClientScoreNativeFanout(ctx, r, keys.counts, time.Hour, nil, emit)
					}
					return emitClientScoreTargetFanout(
						callerIds,
						clientScores,
						map[server.Id]map[server.Id]bool{},
						keys,
						func(clientScores map[server.Id]*ClientScore) clientScoreExportPayload {
							return exportClientScores(forceMinimum, mode, clientScores)
						},
						false,
						false,
						emit,
						nativeFanout,
					)
				})
				if err != nil {
					t.Fatal(err)
				}
			}
		}
		if err := r.SAdd(ctx, providerHardExclusionsKey, providerHardExclusionsReadyMember).Err(); err != nil {
			t.Fatal(err)
		}
	})
}

func modernTestFind(t testing.TB, args *FindProviders2Args) *FindProviders2Result {
	t.Helper()
	clientSession := testingCreateProviderSearchSession(t.Context(), session.NewByJwt(server.NewId(), server.NewId(), "modern-test", false, false))
	result, err := FindProviders2(args, clientSession)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func modernTestClientIds(scores ...[]*ClientScore) map[server.Id]bool {
	clientIds := map[server.Id]bool{}
	for _, group := range scores {
		for _, score := range group {
			clientIds[score.ClientId] = true
		}
	}
	return clientIds
}

// The sum of one counter vector's series whose label has the value.
func modernTestCounterSum(t testing.TB, name string, label string, value string) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	sum := 0.0
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.GetMetric() {
			for _, pair := range metric.GetLabel() {
				if pair.GetName() == label && pair.GetValue() == value {
					sum += metric.GetCounter().GetValue()
				}
			}
		}
	}
	return sum
}

func modernTestDecodePage(t testing.TB, data []byte) []*ClientScore {
	t.Helper()
	page, err := decodeClientScoreSample(data)
	if err != nil {
		t.Fatal(err)
	}
	return page
}

func TestSplitClientScoreSamplesModernFirstPagesEachClassApart(t *testing.T) {
	modernScores := modernTestScores(450, true, ipFamilyFacetV4Only, true)
	otherScores := modernTestScores(330, false, ipFamilyFacetV4Only, true)
	scores := append(slices.Clone(otherScores), modernScores...)
	payload := splitClientScoreSamplesModernFirst(scores)

	// three modern pages of 150, then two other pages of 165
	connect.AssertEqual(t, payload.modernPageCount, 3)
	connect.AssertEqual(t, payload.counts, []int{150, 150, 150, 165, 165})
	var counts []int
	if err := gob.NewDecoder(bytes.NewReader(payload.countsBytes)).Decode(&counts); err != nil {
		t.Fatal(err)
	}
	connect.AssertEqual(t, counts, payload.counts)

	seen := map[server.Id]bool{}
	for i, count := range payload.counts {
		page := modernTestDecodePage(t, payload.encodeSample(i))
		connect.AssertEqual(t, len(page), count)
		for _, score := range page {
			if score.Modern != (i < payload.modernPageCount) {
				t.Fatalf("page %d holds a provider of the other class", i)
			}
			if seen[score.ClientId] {
				t.Fatalf("a provider is in two pages")
			}
			seen[score.ClientId] = true
		}
	}
	connect.AssertEqual(t, len(seen), len(scores))
}

func TestSplitClientScoreSamplesModernFirstWithOneClass(t *testing.T) {
	payload := splitClientScoreSamplesModernFirst(modernTestScores(10, false, ipFamilyFacetV4Only, true))
	connect.AssertEqual(t, payload.modernPageCount, 0)
	connect.AssertEqual(t, payload.counts, []int{10})

	payload = splitClientScoreSamplesModernFirst(modernTestScores(10, true, ipFamilyFacetV4Only, true))
	connect.AssertEqual(t, payload.modernPageCount, 1)
	connect.AssertEqual(t, payload.counts, []int{10})

	payload = splitClientScoreSamplesModernFirst(nil)
	connect.AssertEqual(t, payload.modernPageCount, 0)
	connect.AssertEqual(t, len(payload.counts), 0)
}

func TestClientScoreUnionFacetPartitionStates(t *testing.T) {
	payload := splitClientScoreSamplesModernFirst(append(
		modernTestScores(250, true, ipFamilyFacetV4Only, true),
		modernTestScores(100, false, ipFamilyFacetV4Only, true)...,
	))
	modernPageChecksums := [][sha256.Size]byte{}
	for i := range payload.modernPageCount {
		modernPageChecksums = append(modernPageChecksums, sha256.Sum256(payload.encodeSample(i)))
	}
	encode := func(partition clientScoreModernPartition) []byte {
		var b bytes.Buffer
		if err := gob.NewEncoder(&b).Encode(partition); err != nil {
			t.Fatal(err)
		}
		return b.Bytes()
	}
	facets := map[ipFamilyFacet]clientScoreFacetPayload{ipFamilyFacetV4Only: payload}
	partitionBytes := encode(newClientScoreModernPartition(facets, map[ipFamilyFacet][][sha256.Size]byte{ipFamilyFacetV4Only: modernPageChecksums}))

	state, checksums := clientScoreUnionFacetPartition(partitionBytes, ipFamilyFacetV4Only, payload.countsBytes, payload.counts)
	connect.AssertEqual(t, state, clientScoreModernPartitioned)
	connect.AssertEqual(t, checksums, modernPageChecksums)

	// written by an export before the partition
	state, _ = clientScoreUnionFacetPartition(nil, ipFamilyFacetV4Only, payload.countsBytes, payload.counts)
	connect.AssertEqual(t, state, clientScoreModernAbsent)

	// a partition of other counts, of another facet, unreadable, or naming
	// more modern pages than the counts hold
	otherCountsBytes := encodeClientScoreGobValue(updateClientScoresPhaseMetrics, []int{175, 175, 100})
	for _, stale := range []struct {
		partitionBytes []byte
		facet          ipFamilyFacet
		countsBytes    []byte
		counts         []int
	}{
		{partitionBytes: partitionBytes, facet: ipFamilyFacetV4Only, countsBytes: otherCountsBytes, counts: []int{175, 175, 100}},
		{partitionBytes: partitionBytes, facet: ipFamilyFacetDualstack, countsBytes: payload.countsBytes, counts: payload.counts},
		{partitionBytes: []byte("not a partition"), facet: ipFamilyFacetV4Only, countsBytes: payload.countsBytes, counts: payload.counts},
		{partitionBytes: partitionBytes, facet: ipFamilyFacetV4Only, countsBytes: payload.countsBytes, counts: payload.counts[:1]},
	} {
		state, checksums := clientScoreUnionFacetPartition(stale.partitionBytes, stale.facet, stale.countsBytes, stale.counts)
		if state != clientScoreModernStale || checksums != nil {
			t.Errorf("partition read %s with %d checksums, want stale", state, len(checksums))
		}
	}
}

func TestClientScorePhasedCursorTakesTheModernPhaseWhole(t *testing.T) {
	modernGroups := []map[string]clientScorePage{
		{"m0": {key: "m0", expectedCount: 150}},
		{"m1": {key: "m1", expectedCount: 150}},
	}
	otherGroups := []map[string]clientScorePage{
		{"o0": {key: "o0", expectedCount: 200}, "o1": {key: "o1", expectedCount: 200}},
	}

	cursor := newClientScorePhasedCursor(modernGroups, otherGroups, nil)
	connect.AssertEqual(t, cursor.modernPageCount, 2)
	// the group order holds within the modern phase, and a take ends at its
	// boundary however many rows it asks for
	pages := cursor.take(1000)
	connect.AssertEqual(t, []string{pages[0].key, pages[1].key}, []string{"m0", "m1"})
	connect.AssertEqual(t, cursor.readCount, 300)
	connect.AssertEqual(t, cursor.modernReadCount, 300)
	connect.AssertEqual(t, cursor.inModernPhase(), false)
	connect.AssertEqual(t, cursor.hasOtherPhase(), true)
	connect.AssertEqual(t, len(cursor.take(1000)), 2)
	connect.AssertEqual(t, cursor.modernReadCount, 300)

	// an allowance ending inside the modern phase leaves its unread pages
	cursor = newClientScorePhasedCursor(modernGroups, otherGroups, nil)
	connect.AssertEqual(t, cursor.take(100)[0].key, "m0")
	cursor.skipModernPhase()
	connect.AssertEqual(t, cursor.inModernPhase(), false)
	for _, page := range cursor.take(1000) {
		if page.key == "m1" {
			t.Fatal("a skipped modern page was read after the skip")
		}
	}

	// a stale partition moves every unread page into the modern phase
	cursor = newClientScorePhasedCursor(modernGroups, otherGroups, nil)
	cursor.take(100)
	stale := modernTestCounterSum(t, "urnetwork_findproviders2_modern_partition_total", "state", clientScoreModernStale)
	cursor.markModernStale()
	cursor.markModernStale()
	connect.AssertEqual(t, modernTestCounterSum(t, "urnetwork_findproviders2_modern_partition_total", "state", clientScoreModernStale), stale+1)
	connect.AssertEqual(t, cursor.modernPageCount, 4)
	connect.AssertEqual(t, len(cursor.take(1000)), 3)
	connect.AssertEqual(t, cursor.modernReadCount, 700)
}

func TestSelectClientScoresModernFirst(t *testing.T) {
	modernScores := modernTestScores(5, true, ipFamilyFacetV4Only, true)
	otherScores := modernTestScores(10, false, ipFamilyFacetV4Only, true)
	candidates := map[server.Id]*ClientScore{}
	for _, score := range append(slices.Clone(modernScores), otherScores...) {
		candidates[score.ClientId] = score
	}
	// a draw in id order, so the classes are what decides
	draw := func(candidateClientScores map[server.Id]*ClientScore, n int) []server.Id {
		clientIds := slices.Collect(maps.Keys(candidateClientScores))
		slices.SortFunc(clientIds, func(a server.Id, b server.Id) int { return a.Cmp(b) })
		return clientIds[:min(n, len(clientIds))]
	}
	modernClientIds := modernTestClientIds(modernScores)

	connect.AssertEqual(t, selectClientScoresModernFirst(candidates, 8, false, draw), draw(candidates, 8))

	clientIds := selectClientScoresModernFirst(candidates, 8, true, draw)
	connect.AssertEqual(t, len(clientIds), 8)
	for i, clientId := range clientIds {
		if modernClientIds[clientId] != (i < 5) {
			t.Fatalf("position %d is not in modern-first order", i)
		}
	}
	for _, clientId := range selectClientScoresModernFirst(candidates, 3, true, draw) {
		if !modernClientIds[clientId] {
			t.Fatal("a non-modern provider was drawn while modern candidates remained")
		}
	}
}

func TestClientScoreRequestFilterRefusesNonModernForModernRequests(t *testing.T) {
	modern := modernTestScore(true, ipFamilyFacetV4Only, true)
	other := modernTestScore(false, ipFamilyFacetV4Only, true)
	facets := []ipFamilyFacet{ipFamilyFacetDualstack, ipFamilyFacetV4Only}

	filter := clientScoreRequestFilter{facets: facets, requireModern: true}
	connect.AssertEqual(t, filter.rejection(modern.ClientId, modern), 0)
	connect.AssertEqual(t, filter.rejection(other.ClientId, other), 4)
	// refused before any read: unchecked leaves it out
	connect.AssertEqual(t, filter.unchecked(map[server.Id]*ClientScore{modern.ClientId: modern, other.ClientId: other}, map[server.Id]bool{}), []server.Id{modern.ClientId})

	filter = clientScoreRequestFilter{facets: facets}
	connect.AssertEqual(t, filter.rejection(other.ClientId, other), 0)
}

func TestFindProviders2OutcomeCapability(t *testing.T) {
	for _, c := range []struct {
		requireModernProvider bool
		postQuantumEncryption bool
		capability            string
	}{
		{capability: "none"},
		{requireModernProvider: true, capability: "modern"},
		{postQuantumEncryption: true, capability: "post_quantum"},
		{requireModernProvider: true, postQuantumEncryption: true, capability: "modern_post_quantum"},
	} {
		args := &FindProviders2Args{RequireModernProvider: c.requireModernProvider, PostQuantumEncryption: c.postQuantumEncryption}
		connect.AssertEqual(t, findProviders2OutcomeCapability(args), c.capability)
	}

	groupId := server.NewId()
	args := &FindProviders2Args{Specs: []*ProviderSpec{{LocationGroupId: &groupId}}, RequireModernProvider: true}
	counter, err := findProviders2OutcomesTotal.GetMetricWithLabelValues("any", "group", "us", RankModeQuality, "false", "0", "modern")
	if err != nil {
		t.Fatal(err)
	}
	before := testutil.ToFloat64(counter)
	recordFindProviders2Outcome(args, "US", 0)
	connect.AssertEqual(t, testutil.ToFloat64(counter), before+1)
}

func TestDecideProviderEgressSetsTheModernFlag(t *testing.T) {
	passing := true
	failing := false
	for _, c := range []struct {
		facts  providerEgressFacts
		modern bool
	}{
		// a hard exclusion is in no pool
		{facts: providerEgressFacts{reliabilityFailed: true, modernOutcome: ProviderModernOutcomeModern}, modern: false},
		{facts: providerEgressFacts{noUrlSuccess: true, modernOutcome: ProviderModernOutcomeModern}, modern: true},
		// online only, unprobed and failing, whatever the buckets
		{facts: providerEgressFacts{modernOutcome: ProviderModernOutcomeModern}, modern: true},
		{facts: providerEgressFacts{egressQuality: &failing, modernOutcome: ProviderModernOutcomeNotPostQuantum}, modern: false},
		{facts: providerEgressFacts{egressQuality: &passing, qualityRunsMet: true, speedRunsMet: true, modernOutcome: ProviderModernOutcomeModern}, modern: true},
		{facts: providerEgressFacts{egressQuality: &passing, qualityRunsMet: true, speedRunsMet: true, modernOutcome: ProviderModernOutcomeStaleEvidence}, modern: false},
		{facts: providerEgressFacts{egressQuality: &passing, qualityRunsMet: true, speedRunsMet: true}, modern: false},
	} {
		connect.AssertEqual(t, decideProviderEgress(&c.facts, false).modern, c.modern)
	}
}

func TestEgressIndexSettingsRankModernFirst(t *testing.T) {
	connect.AssertEqual(t, DefaultEgressIndexSettings().RankModernFirst, true)
	for _, c := range []struct {
		config          string
		rankModernFirst bool
	}{
		{config: "egress_index:\n  rank_modern_first: false\n", rankModernFirst: false},
		{config: "egress_index:\n  rank_modern_first: true\n", rankModernFirst: true},
		{config: "egress_index:\n  rank_measured_first: true\n", rankModernFirst: true},
	} {
		modernTestProviderConfig(t, c.config)
		connect.AssertEqual(t, egressIndexSettings().RankModernFirst, c.rankModernFirst)
		connect.AssertEqual(t, testutil.ToFloat64(findProviders2RankModernFirstEnabled) == 1, c.rankModernFirst)
	}
}

// A score and a manifest written with the new fields decode in a reader that
// predates them, and the reverse decodes as not modern and not partitioned.
func TestModernFieldsDecodeAcrossVersions(t *testing.T) {
	type clientScoreBeforeModern struct {
		ClientId server.Id
		Online   bool
	}
	type nativeManifestBeforeModern struct {
		Generation string
		Facets     map[ipFamilyFacet][]clientScoreNativePageInfo
	}
	encode := func(value any) []byte {
		var b bytes.Buffer
		if err := gob.NewEncoder(&b).Encode(value); err != nil {
			t.Fatal(err)
		}
		return b.Bytes()
	}

	score := modernTestScore(true, ipFamilyFacetV4Only, true)
	var oldScores []*clientScoreBeforeModern
	if err := gob.NewDecoder(bytes.NewReader(encode([]*ClientScore{score}))).Decode(&oldScores); err != nil {
		t.Fatal(err)
	}
	connect.AssertEqual(t, oldScores[0].ClientId, score.ClientId)
	var newScores []*ClientScore
	if err := gob.NewDecoder(bytes.NewReader(encode([]*clientScoreBeforeModern{{ClientId: score.ClientId, Online: true}}))).Decode(&newScores); err != nil {
		t.Fatal(err)
	}
	connect.AssertEqual(t, newScores[0].Modern, false)

	manifest := clientScoreNativeManifest{
		Generation:        "g",
		Facets:            map[ipFamilyFacet][]clientScoreNativePageInfo{ipFamilyFacetV4Only: {{Count: 1}}},
		ModernPartitioned: true,
		ModernPages:       map[ipFamilyFacet]int{ipFamilyFacetV4Only: 1},
	}
	var oldManifest nativeManifestBeforeModern
	if err := gob.NewDecoder(bytes.NewReader(encode(manifest))).Decode(&oldManifest); err != nil {
		t.Fatal(err)
	}
	connect.AssertEqual(t, len(oldManifest.Facets), 1)
	var newManifest clientScoreNativeManifest
	if err := gob.NewDecoder(bytes.NewReader(encode(nativeManifestBeforeModern{Generation: "g", Facets: manifest.Facets}))).Decode(&newManifest); err != nil {
		t.Fatal(err)
	}
	connect.AssertEqual(t, newManifest.ModernPartitioned, false)
}

// The export writes every facet's union and native pages modern first, single
// class, with a partition that describes the pages as written.
func TestModernPartitionExportDescribesThePagesWritten(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		location := server.NewId()
		modernScores := modernTestScores(230, true, ipFamilyFacetV4Only, true)
		otherScores := modernTestScores(410, false, ipFamilyFacetV4Only, true)
		dualstackScores := modernTestScores(30, false, ipFamilyFacetDualstack, true)
		modernTestPublishLocation(t, location, append(append(slices.Clone(modernScores), otherScores...), dualstackScores...))
		modernClientIds := modernTestClientIds(modernScores)

		server.Redis(ctx, func(r server.RedisClient) {
			countsKey := clientScoreLocationFacetCountsKey(false, RankModeSpeed, location, server.Id{}, ipFamilyFacetV4Only)
			countsBytes, err := r.Get(ctx, countsKey).Bytes()
			if err != nil {
				t.Fatal(err)
			}
			var counts []int
			if err := gob.NewDecoder(bytes.NewReader(countsBytes)).Decode(&counts); err != nil {
				t.Fatal(err)
			}
			partitionBytes, err := r.Get(ctx, clientScoreLocationModernKey(false, RankModeSpeed, location, server.Id{})).Bytes()
			if err != nil {
				t.Fatal(err)
			}
			state, modernPageChecksums := clientScoreUnionFacetPartition(partitionBytes, ipFamilyFacetV4Only, countsBytes, counts)
			connect.AssertEqual(t, state, clientScoreModernPartitioned)
			connect.AssertEqual(t, len(modernPageChecksums), 2)
			for i := range counts {
				data, err := r.Get(ctx, clientScoreLocationFacetSampleKey(false, RankModeSpeed, location, server.Id{}, ipFamilyFacetV4Only, i)).Bytes()
				if err != nil {
					t.Fatal(err)
				}
				if i < len(modernPageChecksums) && sha256.Sum256(data) != modernPageChecksums[i] {
					t.Fatalf("modern page %d does not match its checksum", i)
				}
				for _, score := range modernTestDecodePage(t, data) {
					if score.Modern != (i < len(modernPageChecksums)) || modernClientIds[score.ClientId] != score.Modern {
						t.Fatalf("union page %d holds a provider of the other class", i)
					}
				}
			}
			state, modernPageChecksums = clientScoreUnionFacetPartition(partitionBytes, ipFamilyFacetDualstack, clientScoreCommandBytes(r.Get(ctx, clientScoreLocationFacetCountsKey(false, RankModeSpeed, location, server.Id{}, ipFamilyFacetDualstack))), []int{30})
			connect.AssertEqual(t, state, clientScoreModernPartitioned)
			connect.AssertEqual(t, len(modernPageChecksums), 0)

			// the native manifest, bound to its generation
			nativeKey := clientScoreNativeKey(clientScoreLocationCountsKey(false, RankModeSpeed, location, server.Id{}))
			pointer, err := r.Get(ctx, nativeKey).Result()
			if err != nil {
				t.Fatal(err)
			}
			manifest, slotKey, err := readClientScoreNativeManifest(ctx, r, nativeKey, pointer)
			if err != nil {
				t.Fatal(err)
			}
			connect.AssertEqual(t, manifest.ModernPartitioned, true)
			connect.AssertEqual(t, manifest.ModernPages, map[ipFamilyFacet]int{ipFamilyFacetDualstack: 0, ipFamilyFacetV4Only: 2, ipFamilyFacetV6Only: 0})
			for index := range manifest.Facets[ipFamilyFacetV4Only] {
				values, err := r.HMGet(ctx, slotKey, clientScoreNativePageField(ipFamilyFacetV4Only, index)).Result()
				if err != nil {
					t.Fatal(err)
				}
				data := []byte(values[0].(string))[sha256.Size:]
				for _, score := range modernTestDecodePage(t, data) {
					if score.Modern != (index < 2) {
						t.Fatalf("native page %d holds a provider of the other class", index)
					}
				}
			}
		})
	})
}

// The cross-facet target of the peer review: a facet-ordered first load of
// the preferred family holds no modern provider, while a whole other family
// of them would go unread. Priority takes the modern providers, through the
// native and the union reader alike, and turning it off restores the facet
// preference.
func TestModernPriorityTakesModernProvidersAcrossFacets(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		for _, config := range []string{modernTestNativeReaderConfig, modernTestUnionReaderConfig} {
			modernTestProviderConfig(t, config)
			location := server.NewId()
			otherScores := modernTestScores(1000, false, ipFamilyFacetDualstack, true)
			modernScores := modernTestScores(100, true, ipFamilyFacetV4Only, true)
			modernTestPublishLocation(t, location, append(slices.Clone(otherScores), modernScores...))
			modernClientIds := modernTestClientIds(modernScores)

			result := modernTestFind(t, &FindProviders2Args{Specs: []*ProviderSpec{{LocationId: &location}}, RankMode: RankModeSpeed})
			connect.AssertEqual(t, len(result.Providers), 20)
			for _, provider := range result.Providers {
				if !modernClientIds[provider.ClientId] || provider.Tier != 0 {
					t.Fatalf("config %q answered a non-modern provider or a borrowed tier while 100 modern natives were eligible", config)
				}
			}

			modernTestProviderConfig(t, config+"  rank_modern_first: false\n")
			result = modernTestFind(t, &FindProviders2Args{Specs: []*ProviderSpec{{LocationId: &location}}, RankMode: RankModeSpeed})
			connect.AssertEqual(t, len(result.Providers), 20)
			for _, provider := range result.Providers {
				if modernClientIds[provider.ClientId] {
					t.Fatalf("config %q with priority off reached past the preferred family", config)
				}
			}
		}
	})
}

// Every modern candidate of the band is taken before the others fill it,
// those past the first load and those behind refused rows included.
func TestModernPriorityFillsFromTheOthersOnlyAfterTheModern(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		for _, config := range []string{modernTestNativeReaderConfig, modernTestUnionReaderConfig} {
			modernTestProviderConfig(t, config)
			location := server.NewId()
			// 1,600 modern natives another network's callers cannot contract
			// with, refused by the request filter, among which 10 public ones
			refusedScores := modernTestScores(1600, true, ipFamilyFacetV4Only, true)
			for _, score := range refusedScores {
				score.NetworkOnly = true
			}
			modernScores := modernTestScores(10, true, ipFamilyFacetV4Only, true)
			otherScores := modernTestScores(1000, false, ipFamilyFacetDualstack, true)
			modernTestPublishLocation(t, location, append(append(slices.Clone(refusedScores), modernScores...), otherScores...))

			result := modernTestFind(t, &FindProviders2Args{Specs: []*ProviderSpec{{LocationId: &location}}, RankMode: RankModeSpeed})
			connect.AssertEqual(t, len(result.Providers), 20)
			answered := map[server.Id]bool{}
			for _, provider := range result.Providers {
				answered[provider.ClientId] = true
			}
			for _, score := range modernScores {
				if !answered[score.ClientId] {
					t.Fatalf("config %q left a modern native unanswered while it filled from the others", config)
				}
			}
			for _, score := range refusedScores {
				if answered[score.ClientId] {
					t.Fatalf("config %q answered a refused provider", config)
				}
			}
		}
	})
}

// A union allowance that ends inside the modern phase keeps availability: the
// band fills from the other phase with an allowance of its own, and the
// answer counts as incomplete priority.
func TestModernPriorityAllowanceEndingInTheModernPhaseStillFills(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		modernTestProviderConfig(t, modernTestUnionReaderConfig)
		location := server.NewId()
		refusedScores := modernTestScores(3000, true, ipFamilyFacetV4Only, true)
		for _, score := range refusedScores {
			score.NetworkOnly = true
		}
		otherScores := modernTestScores(100, false, ipFamilyFacetV4Only, true)
		modernTestPublishLocation(t, location, append(slices.Clone(refusedScores), otherScores...))

		incomplete := testutil.ToFloat64(findProviders2ModernPriorityIncomplete.WithLabelValues(RankModeSpeed))
		result := modernTestFind(t, &FindProviders2Args{Specs: []*ProviderSpec{{LocationId: &location}}, RankMode: RankModeSpeed})
		connect.AssertEqual(t, len(result.Providers), 20)
		otherClientIds := modernTestClientIds(otherScores)
		for _, provider := range result.Providers {
			if !otherClientIds[provider.ClientId] {
				t.Fatal("a refused provider was answered")
			}
		}
		connect.AssertEqual(t, testutil.ToFloat64(findProviders2ModernPriorityIncomplete.WithLabelValues(RankModeSpeed)), incomplete+1)
	})
}

// A modern-only request answers modern providers only, from every band, the
// Online fallback included, and from rank_mode online and force_minimum.
func TestRequireModernProviderAnswersOnlyModernProviders(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		for _, config := range []string{modernTestNativeReaderConfig, modernTestUnionReaderConfig} {
			modernTestProviderConfig(t, config)
			location := server.NewId()
			modernNatives := modernTestScores(8, true, ipFamilyFacetV4Only, true)
			modernOnline := modernTestScores(5, true, ipFamilyFacetV4Only, false)
			others := append(modernTestScores(40, false, ipFamilyFacetV4Only, true), modernTestScores(30, false, ipFamilyFacetV4Only, false)...)
			modernTestPublishLocation(t, location, append(append(slices.Clone(modernNatives), modernOnline...), others...))
			modernClientIds := modernTestClientIds(modernNatives, modernOnline)

			result := modernTestFind(t, &FindProviders2Args{Specs: []*ProviderSpec{{LocationId: &location}}, RankMode: RankModeSpeed, RequireModernProvider: true})
			connect.AssertEqual(t, len(result.Providers), 13)
			nativeTiers := 0
			for _, provider := range result.Providers {
				if !modernClientIds[provider.ClientId] {
					t.Fatalf("config %q answered a non-modern provider to a modern-only request", config)
				}
				if provider.Tier == 0 {
					nativeTiers += 1
				}
			}
			connect.AssertEqual(t, nativeTiers, 8)

			for _, args := range []*FindProviders2Args{
				{Specs: []*ProviderSpec{{LocationId: &location}}, RankMode: RankModeOnline, RequireModernProvider: true},
				{Specs: []*ProviderSpec{{LocationId: &location}}, RankMode: RankModeSpeed, ForceMinimum: true, RequireModernProvider: true},
			} {
				result := modernTestFind(t, args)
				if len(result.Providers) == 0 {
					t.Fatalf("config %q rank mode %s force_minimum %t answered nothing", config, args.RankMode, args.ForceMinimum)
				}
				for _, provider := range result.Providers {
					if !modernClientIds[provider.ClientId] {
						t.Fatalf("config %q rank mode %s force_minimum %t answered a non-modern provider", config, args.RankMode, args.ForceMinimum)
					}
				}
			}
		}
	})
}

// A modern-only request to a target without a modern page answers empty and
// says why, rather than reporting an empty cache.
func TestRequireModernProviderEmptyAnswerReportsCapability(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		modernTestProviderConfig(t, modernTestNativeReaderConfig)
		location := server.NewId()
		modernTestPublishLocation(t, location, modernTestScores(50, false, ipFamilyFacetV4Only, true))

		before := modernTestCounterSum(t, "urnetwork_findproviders2_selection_outcomes_total", "reason", "filtered_capability")
		result := modernTestFind(t, &FindProviders2Args{Specs: []*ProviderSpec{{LocationId: &location}}, RankMode: RankModeSpeed, RequireModernProvider: true})
		connect.AssertEqual(t, len(result.Providers), 0)
		connect.AssertEqual(t, modernTestCounterSum(t, "urnetwork_findproviders2_selection_outcomes_total", "reason", "filtered_capability"), before+1)
	})
}

// Pages an older export wrote carry no partition and read as absent; a
// partition that does not describe the pages read is stale, and costs reads,
// never priority.
func TestModernPartitionAbsentAndStaleTargets(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		modernTestProviderConfig(t, modernTestUnionReaderConfig)

		// an older export: no partition document
		absentLocation := server.NewId()
		modernTestPublishLocation(t, absentLocation, append(modernTestScores(30, true, ipFamilyFacetV4Only, true), modernTestScores(30, false, ipFamilyFacetV4Only, true)...))
		server.Redis(ctx, func(r server.RedisClient) {
			for _, mode := range []RankMode{RankModeQuality, RankModeSpeed} {
				if err := r.Del(ctx, clientScoreLocationModernKey(false, mode, absentLocation, server.Id{})).Err(); err != nil {
					t.Fatal(err)
				}
			}
		})
		absent := modernTestCounterSum(t, "urnetwork_findproviders2_modern_partition_total", "state", clientScoreModernAbsent)
		result := modernTestFind(t, &FindProviders2Args{Specs: []*ProviderSpec{{LocationId: &absentLocation}}, RankMode: RankModeSpeed})
		connect.AssertEqual(t, len(result.Providers), 20)
		if modernTestCounterSum(t, "urnetwork_findproviders2_modern_partition_total", "state", clientScoreModernAbsent) <= absent {
			t.Fatal("a target without a partition was not read as absent")
		}
		// its pages are read, and a modern-only request finds no modern page
		result = modernTestFind(t, &FindProviders2Args{Specs: []*ProviderSpec{{LocationId: &absentLocation}}, RankMode: RankModeSpeed, RequireModernProvider: true})
		connect.AssertEqual(t, len(result.Providers), 0)

		// a partition of other counts: every page joins the modern phase, so
		// the modern providers are still all taken first
		staleLocation := server.NewId()
		modernScores := modernTestScores(15, true, ipFamilyFacetV4Only, true)
		otherScores := modernTestScores(1000, false, ipFamilyFacetV4Only, true)
		modernTestPublishLocation(t, staleLocation, append(slices.Clone(modernScores), otherScores...))
		server.Redis(ctx, func(r server.RedisClient) {
			partition := clientScoreModernPartition{Facets: map[ipFamilyFacet]clientScoreModernPartitionFacet{
				ipFamilyFacetV4Only: {CountsChecksum: sha256.Sum256([]byte("other counts"))},
			}}
			var b bytes.Buffer
			if err := gob.NewEncoder(&b).Encode(partition); err != nil {
				t.Fatal(err)
			}
			if err := r.Set(ctx, clientScoreLocationModernKey(false, RankModeSpeed, staleLocation, server.Id{}), b.Bytes(), time.Hour).Err(); err != nil {
				t.Fatal(err)
			}
		})
		stale := modernTestCounterSum(t, "urnetwork_findproviders2_modern_partition_total", "state", clientScoreModernStale)
		result = modernTestFind(t, &FindProviders2Args{Specs: []*ProviderSpec{{LocationId: &staleLocation}}, RankMode: RankModeSpeed})
		connect.AssertEqual(t, len(result.Providers), 20)
		answered := map[server.Id]bool{}
		for _, provider := range result.Providers {
			answered[provider.ClientId] = true
		}
		for _, score := range modernScores {
			if !answered[score.ClientId] {
				t.Fatal("a stale partition cost a modern provider its priority")
			}
		}
		if modernTestCounterSum(t, "urnetwork_findproviders2_modern_partition_total", "state", clientScoreModernStale) <= stale {
			t.Fatal("a partition of other counts was not read as stale")
		}
	})
}

// Quality natives need live validation before they can be drawn: with
// priority the modern candidates are validated first, so they fill the band.
func TestModernPriorityValidatesModernQualityCandidatesFirst(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		modernTestProviderConfig(t, "subscriber_quality_policy_version: 2\n"+modernTestNativeReaderConfig)
		location := server.NewId()
		modernScores := modernTestScores(25, true, ipFamilyFacetDualstack, true)
		otherScores := modernTestScores(200, false, ipFamilyFacetDualstack, true)
		scores := append(slices.Clone(modernScores), otherScores...)
		writeSubscriberFactsForScores(ctx, scores)
		modernTestPublishLocation(t, location, scores)
		modernClientIds := modernTestClientIds(modernScores)

		result := modernTestFind(t, &FindProviders2Args{Specs: []*ProviderSpec{{LocationId: &location}}, RankMode: RankModeQuality})
		connect.AssertEqual(t, len(result.Providers), 20)
		for _, provider := range result.Providers {
			if !modernClientIds[provider.ClientId] || provider.Tier != 0 {
				t.Fatal("a Quality answer held a non-modern native while validated modern natives were eligible")
			}
		}
	})
}

// Every combination of the gate and modern-first priority
// (FP2-MODERN.md, "Provider status"): a provider that is not modern reports
// not_modern with the gate on, and with the gate off only while priority is
// on, keeping its buckets; a modern provider never does.
func TestProviderStatusNotModernUnderPriorityAndGate(t *testing.T) {
	facts := testingProviderStatusFacts()
	filter := testingProviderStatusFilter(facts)
	statusOf := func(gate bool, rankModernFirst bool, filter providerCountFilter) *ProviderStatus {
		settings := modernGateTestSettings(gate)
		settings.RankModernFirst = rankModernFirst
		evaluation := evaluateProviderStatus(facts, filter, settings, false, testingProviderStatusMinimums)
		return newProviderStatus(facts, evaluation, filter, settings, testingProviderStatusMinimums, testingProviderStatusNow)
	}
	for _, c := range []struct {
		gate            bool
		rankModernFirst bool
		reason          string
	}{
		{gate: true, rankModernFirst: true, reason: ProviderStatusReasonNotModern},
		{gate: true, rankModernFirst: false, reason: ProviderStatusReasonNotModern},
		{gate: false, rankModernFirst: true, reason: ProviderStatusReasonNotModern},
		{gate: false, rankModernFirst: false, reason: ProviderStatusReasonNone},
	} {
		status := statusOf(c.gate, c.rankModernFirst, filter)
		if status.Reason != c.reason {
			t.Errorf("gate %t priority %t: reason %s, want %s", c.gate, c.rankModernFirst, status.Reason, c.reason)
		}
		// priority keeps the provider in its buckets; the gate takes it out
		weight := testingProviderStatusNumber(status, ProviderStatusNumberWeightQuality)
		if weight == nil || weight.Passes == c.gate {
			t.Errorf("gate %t priority %t: quality weight %+v", c.gate, c.rankModernFirst, weight)
		}
	}

	filter.modernEvidence = map[server.Id]*ProviderModernEvidenceRecord{facts.clientId: {
		PostQuantum:     true,
		KeyExchange:     "X25519MLKEM768",
		WindowResizable: true,
		MeasuredAt:      testingProviderStatusNow.Add(-time.Hour),
		FirstMeasuredAt: testingProviderStatusNow.Add(-time.Hour),
		RunCount:        1,
	}}
	for _, gate := range []bool{true, false} {
		for _, rankModernFirst := range []bool{true, false} {
			if reason := statusOf(gate, rankModernFirst, filter).Reason; reason != ProviderStatusReasonNone {
				t.Errorf("modern provider, gate %t priority %t: reason %s", gate, rankModernFirst, reason)
			}
		}
	}
}
