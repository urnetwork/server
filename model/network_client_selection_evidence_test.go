// rank_measured_first (network_client_selection_evidence.go), pure: one
// country's providers exported as UpdateClientScores builds and caches them,
// and the draws FindProviders2 makes from the cache as the request decodes it.
package model

import (
	mathrand "math/rand"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-playground/assert/v2"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
)

// One provider as the score export reads it.
type evidenceTestProvider struct {
	name string
	// accepted URL outcomes in the evidence window, none when urlTotal is 0
	urlOk    int
	urlTotal int
	// score rows, lookback index to {reliability_weight,
	// independent_reliability_weight}; nil for a provider without rows
	lookbacks map[int][2]float64
	// out of Quality only, so a Quality request borrows it from Speed
	arinNonQuality bool
	hasLatencyTest bool
	latencyMillis  int
	bytesPerSecond ByteCount
	dualstack      bool
}

// Score rows that pass every reliability floor, at the given weight.
func evidenceTestScored(reliabilityWeight float64) map[int][2]float64 {
	return map[int][2]float64{
		1: {reliabilityWeight, 0.99},
		2: {reliabilityWeight, 0.9},
	}
}

// Runs the providers, all published in one country, through the export as
// UpdateClientScores does: a source row per lookback, with the source query's
// neutral lookback for a provider without score rows, the target maps, the
// shared egress decision and the ranking. Then encodes the country's union
// pool in `mode` as the cache holds it and decodes it as a request reads it.
// Returns the pool and each client id's name.
func evidenceTestExportedPool(
	t testing.TB,
	now time.Time,
	mode RankMode,
	providers []*evidenceTestProvider,
) (map[server.Id]*ClientScore, map[server.Id]string) {
	t.Helper()
	settings := DefaultEgressIndexSettings()
	countFilter := providerCountFilter{
		now:                     now,
		arinRisk:                map[server.Id]bool{},
		arinNonQuality:          map[server.Id]bool{},
		reliabilityFailed:       map[server.Id]bool{},
		healthCounts:            map[server.Id]ProviderEgressHealthCounts{},
		healthWindowEnd:         now,
		countryCodes:            map[server.Id]string{},
		tlsAuthenticationFailed: map[server.Id]bool{},
		modernEvidence:          map[server.Id]*ProviderModernEvidenceRecord{},
	}
	countryLocationId := server.NewId()
	countryCode := "us"
	names := map[server.Id]string{}
	rows := []clientScoreSourceRow{}
	for _, provider := range providers {
		clientId := server.NewId()
		networkId := server.NewId()
		names[clientId] = provider.name
		if provider.arinNonQuality {
			countFilter.arinNonQuality[clientId] = true
		}
		var egressIndex *int
		var egressQuality *bool
		if 0 < provider.urlTotal {
			measuredAt := now.Add(-time.Hour)
			countFilter.healthCounts[clientId] = ProviderEgressHealthCounts{
				MeasuredAt:      measuredAt,
				FirstMeasuredAt: now.Add(-7 * time.Hour),
				OKCount:         provider.urlOk,
				Total:           provider.urlTotal,
			}
			index := ComputeEgressIndex(&EgressHealthRun{MeasuredAt: measuredAt, OkCount: provider.urlOk, Total: provider.urlTotal}, now, settings)
			egressIndex = &index.Index
			egressQuality = index.QualityVerdict()
		}
		lookbacks := provider.lookbacks
		if lookbacks == nil {
			// LEFT JOIN client_connection_reliability_score, COALESCE(.., 0),
			// COALESCE(.., 1), COALESCE(.., 1)
			lookbacks = map[int][2]float64{0: {1, 1}}
		}
		ipFamilies := ClientScoreIpFamilyV4
		if provider.dualstack {
			ipFamilies |= ClientScoreIpFamilyV6
		}
		for lookbackIndex, weights := range lookbacks {
			score := &ClientScore{
				ClientId:                     clientId,
				LookbackIndex:                lookbackIndex,
				NetworkId:                    networkId,
				IpFamilies:                   ipFamilies,
				ReliabilityWeight:            weights[0],
				IndependentReliabilityWeight: weights[1],
				MinRelativeLatencyMillis:     provider.latencyMillis,
				MaxBytesPerSecond:            provider.bytesPerSecond,
				HasLatencyTest:               provider.hasLatencyTest,
				HasSpeedTest:                 true,
				Scores:                       map[string]int{},
				Tiers:                        map[string]int{},
			}
			rankModeBases, rankModeMinimumBases := clientScoreRankModeBases(0, 0, egressIndex)
			setClientScoreRanks(score, rankModeBases, rankModeMinimumBases, provider.latencyMillis, provider.bytesPerSecond, provider.hasLatencyTest, true)
			rows = append(rows, clientScoreSourceRow{
				score:     score,
				countryId: &countryLocationId,
				egress: &clientScoreEgress{
					egressIndex:          egressIndex,
					egressQuality:        egressQuality,
					publishedCountryCode: &countryCode,
				},
			})
		}
	}

	targetMaps := newClientScoreTargetMaps(rows, nil, countFilter.hasHardEgressFailure, nil)
	clientScores := targetMaps.locationClientScores[countryLocationId]
	minimums := map[int]float64{1: 0.95, 2: 0.7, 3: 0.6}
	for clientId, clientScore := range clientScores {
		egress := targetMaps.clientIdEgresses[clientId]
		decision := decideProviderEgress(
			countFilter.egressFacts(clientId, egress.publishedCountryCode, egress.egressIndex, egress.egressQuality, settings),
			false,
		)
		rankClientScore(clientScore, decision, countFilter, settings, minimums)
	}

	payload := exportClientScores(false, mode, clientScores)
	pool := map[server.Id]*ClientScore{}
	for i := range payload.counts {
		sample, err := decodeClientScoreSample(payload.encodeSample(i))
		if err != nil {
			t.Fatal(err)
		}
		mergeClientScoreSample(pool, sample)
	}
	if len(pool) != len(providers) {
		t.Fatalf("the exported pool holds %d providers, want %d", len(pool), len(providers))
	}
	return pool, names
}

// The evidence FindProviders2 would order its draws by under `config`.
func evidenceTestRequestEvidence(config string, forceMinimum bool, now time.Time) func(*ClientScore) clientScoreEvidenceRank {
	pop := server.Config.PushSimpleResource(providerConfigResourceName, []byte(config))
	defer pop()
	return findProviders2Evidence(egressIndexSettings(), forceMinimum, now)
}

func evidenceTestNames(clientIds []server.Id, names map[server.Id]string) []string {
	out := make([]string, 0, len(clientIds))
	for _, clientId := range clientIds {
		out = append(out, names[clientId])
	}
	return out
}

// Main's provider.yml on 2026-10-15, then with the setting on.
const (
	evidenceTestMainConfig            = "subscriber_quality_policy_version: 2\nenable_egress_test: false\negress_index: {min_scored_loads: 26, native_reader_enabled: true}\n"
	evidenceTestMainConfigRankingOn   = "subscriber_quality_policy_version: 2\nenable_egress_test: false\negress_index: {min_scored_loads: 26, native_reader_enabled: true, rank_measured_first: true}\n"
	evidenceTestMainConfigRankingOff  = "subscriber_quality_policy_version: 2\nenable_egress_test: false\negress_index: {min_scored_loads: 26, native_reader_enabled: true, rank_measured_first: false}\n"
	evidenceTestRankingUnreadableBool = "egress_index: {native_reader_enabled: true, rank_measured_first: soon}\n"
)

// Three Online providers of each kind of evidence in one country, none native
// in either bucket: measured with a failing URL ratio, with and without a
// reliability score, and unprobed, with and without one. Within each kind,
// a, b, c is the order of the weights, and no two providers tie on them.
func evidenceTestOnlineProviders() []*evidenceTestProvider {
	performance := func(provider *evidenceTestProvider) *evidenceTestProvider {
		provider.hasLatencyTest = true
		if provider.latencyMillis == 0 {
			provider.latencyMillis = 20
		}
		provider.bytesPerSecond = 50 * Mib
		return provider
	}
	return []*evidenceTestProvider{
		// URL weight 0.1 + 0.9 * 5/10 = 0.55, times the reliability weight
		performance(&evidenceTestProvider{name: "measured_scored_a", urlOk: 5, urlTotal: 10, lookbacks: evidenceTestScored(0.9)}),
		performance(&evidenceTestProvider{name: "measured_scored_b", urlOk: 5, urlTotal: 10, lookbacks: evidenceTestScored(0.8)}),
		performance(&evidenceTestProvider{name: "measured_scored_c", urlOk: 5, urlTotal: 10, lookbacks: evidenceTestScored(0.7)}),
		// the neutral reliability weight 1 times the URL weight
		performance(&evidenceTestProvider{name: "measured_unscored_a", urlOk: 7, urlTotal: 10}),
		performance(&evidenceTestProvider{name: "measured_unscored_b", urlOk: 6, urlTotal: 10}),
		performance(&evidenceTestProvider{name: "measured_unscored_c", urlOk: 5, urlTotal: 10}),
		// the neutral URL weight 1 times the reliability weight
		performance(&evidenceTestProvider{name: "unprobed_scored_a", lookbacks: evidenceTestScored(0.96)}),
		performance(&evidenceTestProvider{name: "unprobed_scored_b", lookbacks: evidenceTestScored(0.92)}),
		performance(&evidenceTestProvider{name: "unprobed_scored_c", lookbacks: evidenceTestScored(0.88)}),
		// both neutral: 1, ordered by the speed score alone (latency)
		performance(&evidenceTestProvider{name: "unprobed_unscored_a", latencyMillis: 20}),
		performance(&evidenceTestProvider{name: "unprobed_unscored_b", latencyMillis: 25}),
		performance(&evidenceTestProvider{name: "unprobed_unscored_c", latencyMillis: 30}),
	}
}

// The Online fallback of FindProviders2 before rank_measured_first, verbatim:
// the reference the order with the setting off must equal.
func legacySelectOnlineForTest(candidateClientScores map[server.Id]*ClientScore, facets []ipFamilyFacet, n int) []server.Id {
	clientIds := []server.Id{}
	for _, facet := range facets {
		remainingCount := n - len(clientIds)
		if remainingCount <= 0 {
			break
		}
		facetClientIds := []server.Id{}
		for clientId, clientScore := range candidateClientScores {
			if clientScore.ipFamilyFacet() == facet {
				facetClientIds = append(facetClientIds, clientId)
			}
		}
		// full ties in no particular order
		mathrand.Shuffle(len(facetClientIds), func(i int, j int) {
			facetClientIds[i], facetClientIds[j] = facetClientIds[j], facetClientIds[i]
		})
		slices.SortStableFunc(facetClientIds, func(a server.Id, b server.Id) int {
			clientScoreA := candidateClientScores[a]
			clientScoreB := candidateClientScores[b]
			weightA, weightB := clientScoreA.UrlProbeSuccessWeight, clientScoreB.UrlProbeSuccessWeight
			if weightA == 0 {
				weightA = 1
			}
			if weightB == 0 {
				weightB = 1
			}
			switch {
			case clientScoreB.ReliabilityWeight*weightB < clientScoreA.ReliabilityWeight*weightA:
				return -1
			case clientScoreA.ReliabilityWeight*weightA < clientScoreB.ReliabilityWeight*weightB:
				return 1
			}
			if d := clientScoreA.Tiers[RankModeSpeed] - clientScoreB.Tiers[RankModeSpeed]; d != 0 {
				return d
			}
			return clientScoreA.Scores[RankModeSpeed] - clientScoreB.Scores[RankModeSpeed]
		})
		clientIds = append(clientIds, facetClientIds[:min(remainingCount, len(facetClientIds))]...)
	}
	return clientIds
}

// The weighted draw of FindProviders2 before rank_measured_first, verbatim.
func legacySelectProvidersForTest(candidateClientScores map[server.Id]*ClientScore, mode RankMode, facets []ipFamilyFacet, n int) []server.Id {
	clientIds := []server.Id{}
	for _, facet := range facets {
		remainingCount := n - len(clientIds)
		if remainingCount <= 0 {
			break
		}
		facetClientIds := []server.Id{}
		for clientId, clientScore := range candidateClientScores {
			if clientScore.ipFamilyFacet() == facet {
				facetClientIds = append(facetClientIds, clientId)
			}
		}
		mathrand.Shuffle(len(facetClientIds), func(i int, j int) {
			facetClientIds[i], facetClientIds[j] = facetClientIds[j], facetClientIds[i]
		})

		connect.WeightedSelectFunc(facetClientIds, remainingCount, func(clientId server.Id) float32 {
			clientScore := candidateClientScores[clientId]
			return clientScore.ScaledWeights[mode]
		})
		facetClientIds = facetClientIds[:min(remainingCount, len(facetClientIds))]

		// band by tier
		slices.SortStableFunc(facetClientIds, func(a server.Id, b server.Id) int {
			clientScoreA := candidateClientScores[a]
			clientScoreB := candidateClientScores[b]

			return clientScoreA.Tiers[mode] - clientScoreB.Tiers[mode]
		})
		clientIds = append(clientIds, facetClientIds...)
	}
	return clientIds
}

// A Quality request whose own bucket and the Speed bucket are empty lends its
// answer from the Online fallback, all of it from one country. By the weights
// alone the least known providers rank first: an unprobed provider has the
// neutral URL weight 1, the most a measured one reaches, and one without a
// reliability score the neutral reliability weight 1, a provider's steady
// best. With
// rank_measured_first on, URL-probe evidence comes first, then a reliability
// score, each group in its old order, and the unmeasured still fill the rest.
func TestOnlineFallbackLendsMeasuredFirst(t *testing.T) {
	now := server.NowUtc()
	pool, names := evidenceTestExportedPool(t, now, RankModeQuality, evidenceTestOnlineProviders())
	for clientId, clientScore := range pool {
		if !clientScore.Online || clientScore.PassesMinimums[RankModeQuality] || clientScore.PassesMinimums[RankModeSpeed] {
			t.Fatalf("%s is not an Online provider outside both native buckets", names[clientId])
		}
	}
	facets, err := ipFamilyFacetsForFilter("")
	if err != nil {
		t.Fatal(err)
	}
	evidence := evidenceTestRequestEvidence(evidenceTestMainConfigRankingOn, false, now)
	if evidence == nil {
		t.Fatal("rank_measured_first: true in the egress_index block did not turn the ranking on")
	}

	want := []string{
		"measured_scored_a", "measured_scored_b", "measured_scored_c",
		"measured_unscored_a", "measured_unscored_b", "measured_unscored_c",
		"unprobed_scored_a", "unprobed_scored_b", "unprobed_scored_c",
		"unprobed_unscored_a", "unprobed_unscored_b", "unprobed_unscored_c",
	}
	// three, six and nine places left, every provider, and more places than
	// providers: the answer is never shorter than without the ranking
	for _, n := range []int{3, 6, 9, 12, 20} {
		lent := evidenceTestNames(selectOnlineClientScores(pool, facets, n, evidence), names)
		if !slices.Equal(lent, want[:min(n, len(want))]) {
			t.Fatalf("with %d places the Online fallback lent %v, want %v", n, lent, want[:min(n, len(want))])
		}
		legacy := legacySelectOnlineForTest(pool, facets, n)
		assert.Equal(t, len(lent), len(legacy))
	}
}

// With rank_measured_first off, the default, the Online fallback's order is
// exactly the one before, which on this fixture lends the unprobed first:
// absent, false, and a value that is not a bool, which keeps the block's
// defaults.
func TestOnlineFallbackOrderUnchangedWithRankingOff(t *testing.T) {
	now := server.NowUtc()
	pool, names := evidenceTestExportedPool(t, now, RankModeQuality, evidenceTestOnlineProviders())
	facets, err := ipFamilyFacetsForFilter("")
	if err != nil {
		t.Fatal(err)
	}
	for _, config := range []string{"", evidenceTestMainConfig, evidenceTestMainConfigRankingOff, evidenceTestRankingUnreadableBool} {
		evidence := evidenceTestRequestEvidence(config, false, now)
		if evidence != nil {
			t.Fatalf("provider.yml %q turned the ranking on", config)
		}
		for n := 0; n <= 13; n += 1 {
			got := selectOnlineClientScores(pool, facets, n, evidence)
			want := legacySelectOnlineForTest(pool, facets, n)
			if !slices.Equal(got, want) {
				t.Fatalf("provider.yml %q, %d places: %v, before %v", config, n, evidenceTestNames(got, names), evidenceTestNames(want, names))
			}
		}
	}
	assert.Equal(t, evidenceTestNames(selectOnlineClientScores(pool, facets, 3, nil), names), []string{"unprobed_unscored_a", "unprobed_unscored_b", "unprobed_unscored_c"})
}

// force_minimum keeps its draw of every admitted provider alike.
func TestForceMinimumDrawIgnoresRanking(t *testing.T) {
	now := server.NowUtc()
	assert.Equal(t, evidenceTestRequestEvidence(evidenceTestMainConfigRankingOn, true, now) == nil, true)
	assert.Equal(t, evidenceTestRequestEvidence(evidenceTestMainConfigRankingOn, false, now) == nil, false)
}

// Four Speed natives a Quality request borrows (ARIN non-quality, so native
// in Speed alone), all with passing URL evidence. Two have no reliability
// score, as an intent provider probed in its qualification grace, which
// gives them the largest selection weight and here the better Speed tiers.
func evidenceTestNativeProviders() []*evidenceTestProvider {
	return []*evidenceTestProvider{
		// Speed tier 0
		{name: "unscored_tier0", urlOk: 10, urlTotal: 10, arinNonQuality: true, hasLatencyTest: true, latencyMillis: 10, bytesPerSecond: 50 * Mib},
		// tier 1: 25 points short of the throughput threshold
		{name: "unscored_tier1", urlOk: 10, urlTotal: 10, arinNonQuality: true, hasLatencyTest: true, latencyMillis: 10, bytesPerSecond: 15 * Mib},
		// tier 2: no latency test
		{name: "scored_tier2", urlOk: 10, urlTotal: 10, arinNonQuality: true, lookbacks: evidenceTestScored(0.9), bytesPerSecond: 50 * Mib},
		// past the Speed latency cutoff
		{name: "scored_tier3", urlOk: 10, urlTotal: 10, arinNonQuality: true, lookbacks: evidenceTestScored(0.9), hasLatencyTest: true, latencyMillis: 60, bytesPerSecond: 50 * Mib},
	}
}

// A weighted draw ranks measured ahead of unmeasured too, applied before the
// draw rather than to its result: every scored native is drawn before an
// unscored one, then each group is banded by tier as before. Off, the draw is
// the one before, banded by tier alone.
func TestBorrowedNativesDrawMeasuredFirst(t *testing.T) {
	now := server.NowUtc()
	pool, names := evidenceTestExportedPool(t, now, RankModeSpeed, evidenceTestNativeProviders())
	borrowable := map[server.Id]*ClientScore{}
	tiers := map[string]int{}
	for clientId, clientScore := range pool {
		if clientScore.PassesMinimums[RankModeSpeed] && !clientScore.PassesMinimums[RankModeQuality] {
			borrowable[clientId] = clientScore
		}
		tiers[names[clientId]] = clientScore.Tiers[RankModeSpeed]
	}
	assert.Equal(t, len(borrowable), 4)
	assert.Equal(t, tiers, map[string]int{"unscored_tier0": 0, "unscored_tier1": 1, "scored_tier2": 2, "scored_tier3": ClientScoreCutoffTier})
	facets, err := ipFamilyFacetsForFilter("")
	if err != nil {
		t.Fatal(err)
	}

	evidence := evidenceTestRequestEvidence(evidenceTestMainConfigRankingOn, false, now)
	assert.Equal(t, evidenceTestNames(selectClientScoresByWeight(borrowable, RankModeSpeed, facets, 4, evidence), names), []string{"scored_tier2", "scored_tier3", "unscored_tier0", "unscored_tier1"})
	// fewer places than scored natives: only scored natives, whatever the weights
	for i := 0; i < 20; i += 1 {
		drawn := evidenceTestNames(selectClientScoresByWeight(borrowable, RankModeSpeed, facets, 2, evidence), names)
		slices.Sort(drawn)
		assert.Equal(t, drawn, []string{"scored_tier2", "scored_tier3"})
	}

	off := evidenceTestRequestEvidence(evidenceTestMainConfig, false, now)
	assert.Equal(t, off == nil, true)
	assert.Equal(t, evidenceTestNames(selectClientScoresByWeight(borrowable, RankModeSpeed, facets, 4, off), names), []string{"unscored_tier0", "unscored_tier1", "scored_tier2", "scored_tier3"})
	assert.Equal(t, selectClientScoresByWeight(borrowable, RankModeSpeed, facets, 4, off), legacySelectProvidersForTest(borrowable, RankModeSpeed, facets, 4))
	assert.Equal(t, len(selectClientScoresByWeight(borrowable, RankModeSpeed, facets, 0, evidence)), 0)
}

// The evidence is the first key, ahead of the family preference: a measured
// v4-only provider is lent before an unprobed dualstack one, which the
// preference alone would put first.
func TestOnlineFallbackEvidenceOutranksFamilyPreference(t *testing.T) {
	now := server.NowUtc()
	pool, names := evidenceTestExportedPool(t, now, RankModeQuality, []*evidenceTestProvider{
		{name: "measured_v4", urlOk: 5, urlTotal: 10, lookbacks: evidenceTestScored(0.5), hasLatencyTest: true, latencyMillis: 20, bytesPerSecond: 50 * Mib},
		{name: "unprobed_dualstack", dualstack: true, hasLatencyTest: true, latencyMillis: 20, bytesPerSecond: 50 * Mib},
	})
	facets, err := ipFamilyFacetsForFilter("")
	if err != nil {
		t.Fatal(err)
	}
	on := evidenceTestRequestEvidence(evidenceTestMainConfigRankingOn, false, now)
	assert.Equal(t, evidenceTestNames(selectOnlineClientScores(pool, facets, 2, on), names), []string{"measured_v4", "unprobed_dualstack"})
	assert.Equal(t, evidenceTestNames(selectOnlineClientScores(pool, facets, 2, nil), names), []string{"unprobed_dualstack", "measured_v4"})
}

// What a draw knows of a score: URL evidence the score still vouches for,
// then a reliability score, read from the cached record alone.
func TestClientScoreEvidenceRank(t *testing.T) {
	now := server.NowUtc()
	future := now.Add(time.Minute)
	past := now.Add(-time.Minute)
	scored := map[int]*ClientScore{1: {ReliabilityWeight: 1, IndependentReliabilityWeight: 1}, 2: {ReliabilityWeight: 1, IndependentReliabilityWeight: 1}}
	neutral := map[int]*ClientScore{0: {ReliabilityWeight: 1, IndependentReliabilityWeight: 1}}
	for _, c := range []struct {
		name       string
		validUntil *time.Time
		lookbacks  map[int]*ClientScore
		want       clientScoreEvidenceRank
	}{
		{"current url, scored", &future, scored, clientScoreEvidenceUrlAndReliability},
		{"current url, unscored", &future, neutral, clientScoreEvidenceUrlOnly},
		{"expired url, scored", &past, scored, clientScoreEvidenceReliabilityOnly},
		// the exact boundary is stale, as filterPool reads it
		{"url expiring now, scored", &now, scored, clientScoreEvidenceReliabilityOnly},
		{"unprobed, scored", nil, scored, clientScoreEvidenceReliabilityOnly},
		{"unprobed, unscored", nil, neutral, clientScoreEvidenceNone},
		{"expired url, unscored", &past, neutral, clientScoreEvidenceNone},
		// a record without lookbacks, as a synthetic or older one, reads as scored
		{"current url, no lookbacks", &future, nil, clientScoreEvidenceUrlAndReliability},
		// a scored 5 minute row below full weight is a score
		{"current url, 5 minute row", &future, map[int]*ClientScore{0: {ReliabilityWeight: 0.5, IndependentReliabilityWeight: 0.9}}, clientScoreEvidenceUrlAndReliability},
		{"current url, every lookback", &future, map[int]*ClientScore{0: {ReliabilityWeight: 1, IndependentReliabilityWeight: 1}, 1: {ReliabilityWeight: 1, IndependentReliabilityWeight: 1}, 2: {ReliabilityWeight: 1, IndependentReliabilityWeight: 1}}, clientScoreEvidenceUrlAndReliability},
	} {
		score := &ClientScore{EgressValidUntil: c.validUntil, LookbackClientScores: c.lookbacks}
		if got := clientScoreEvidence(score, now); got != c.want {
			t.Fatalf("%s: rank %s, want %s", c.name, got, c.want)
		}
	}
	assert.Equal(t, []string{
		clientScoreEvidenceUrlAndReliability.String(),
		clientScoreEvidenceUrlOnly.String(),
		clientScoreEvidenceReliabilityOnly.String(),
		clientScoreEvidenceNone.String(),
	}, []string{"url_reliability", "url", "reliability", "none"})
}

// Without evidence the draw sees the candidates themselves, in one group;
// with it, the ranks best first and no empty group.
func TestClientScoreEvidenceGroups(t *testing.T) {
	now := server.NowUtc()
	future := now.Add(time.Hour)
	measured := &ClientScore{ClientId: server.NewId(), EgressValidUntil: &future}
	unknown := &ClientScore{ClientId: server.NewId(), LookbackClientScores: map[int]*ClientScore{0: {ReliabilityWeight: 1, IndependentReliabilityWeight: 1}}}
	candidates := map[server.Id]*ClientScore{measured.ClientId: measured, unknown.ClientId: unknown}

	groups := clientScoreEvidenceGroups(candidates, nil)
	assert.Equal(t, len(groups), 1)
	assert.Equal(t, reflect.ValueOf(groups[0]).Pointer(), reflect.ValueOf(candidates).Pointer())

	groups = clientScoreEvidenceGroups(candidates, func(clientScore *ClientScore) clientScoreEvidenceRank {
		return clientScoreEvidence(clientScore, now)
	})
	assert.Equal(t, groups, []map[server.Id]*ClientScore{
		{measured.ClientId: measured},
		{unknown.ClientId: unknown},
	})
	assert.Equal(t, len(clientScoreEvidenceGroups(map[server.Id]*ClientScore{}, func(*ClientScore) clientScoreEvidenceRank { return 0 })), 0)
}

// reliabilityUnscored reads the neutral lookback both score source queries
// give a provider without score rows; it must change with them.
func TestClientScoreSourceNeutralLookback(t *testing.T) {
	updateClientScoresSource, err := os.ReadFile("network_client_location_model.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{clientScoreLocationGroupSourceSQL, string(updateClientScoresSource)} {
		for _, fragment := range []string{
			"LEFT JOIN client_connection_reliability_score ON",
			"COALESCE(client_connection_reliability_score.lookback_index, 0)",
			"COALESCE(client_connection_reliability_score.reliability_weight, 1)",
			"COALESCE(client_connection_reliability_score.independent_reliability_weight, 1)",
		} {
			if !strings.Contains(source, fragment) {
				t.Fatalf("a score source query lost %q", fragment)
			}
		}
	}
}

// rank_measured_first in the egress_index block, off by default and when it
// cannot be read.
func TestEgressIndexSettingsRankMeasuredFirst(t *testing.T) {
	load := func(config string) *EgressIndexSettings {
		pop := server.Config.PushSimpleResource(providerConfigResourceName, []byte(config))
		defer pop()
		return egressIndexSettings()
	}
	assert.Equal(t, DefaultEgressIndexSettings().RankMeasuredFirst, false)
	assert.Equal(t, load("").RankMeasuredFirst, false)
	assert.Equal(t, load(evidenceTestMainConfig).RankMeasuredFirst, false)
	assert.Equal(t, load(evidenceTestMainConfigRankingOff).RankMeasuredFirst, false)
	on := load(evidenceTestMainConfigRankingOn)
	assert.Equal(t, on.RankMeasuredFirst, true)
	// the rest of the block still applies
	assert.Equal(t, on.MinScoredLoads, 26)
	assert.Equal(t, on.NativeReaderEnabled, true)
	assert.Equal(t, load("egress_index:\n  rank_measured_first: true\n").RankMeasuredFirst, true)
	// a value that is not a bool fails the block, which keeps the defaults
	assert.Equal(t, load(evidenceTestRankingUnreadableBool).RankMeasuredFirst, false)
	// only the block's key counts
	assert.Equal(t, load("rank_measured_first: true\n").RankMeasuredFirst, false)
}
