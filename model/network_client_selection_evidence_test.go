// rank_measured_first (network_client_selection_evidence.go), pure: one
// country's providers exported as UpdateClientScores builds and caches them,
// and the draws FindProviders2 makes from the cache as the request decodes it.
package model

import (
	"fmt"
	"maps"
	"math"
	mathrand "math/rand"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-playground/assert/v2"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
)

// One provider as the score export reads it.
type evidenceTestProvider struct {
	name string
	// a fixed client id, for a draw a seeded source must reproduce; a new
	// one when zero
	clientId server.Id
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
	// the modern provider gate turns on by itself on 2026-10-21; these
	// fixtures rank providers the gate has no say over
	modernGateOff := false
	settings.QualitySpeedRequiresModernProvider = &modernGateOff
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
		clientId := provider.clientId
		if clientId == (server.Id{}) {
			clientId = server.NewId()
		}
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

	targetMaps := newClientScoreTargetMaps(rows, nil, countFilter.hasHardEgressFailure)
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

// A uniform source of its own, seeded, so a draw is the same on every run.
func evidenceTestUniform(seed int64) func() float64 {
	return mathrand.New(mathrand.NewSource(seed)).Float64
}

// A uniform source for an order that must draw nothing.
func evidenceTestNoDraw(t testing.TB) func() float64 {
	return func() float64 {
		t.Helper()
		t.Fatal("the order before rank_measured_first drew from a random source")
		return 0
	}
}

// A fixed client id, the same on every run.
func evidenceTestFixedId(i int) server.Id {
	var clientId server.Id
	clientId[0] = 0xe7
	clientId[14] = byte(i >> 8)
	clientId[15] = byte(i)
	return clientId
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
// alone the least-known providers rank first: an unprobed provider has the
// neutral URL weight 1, the most a measured one reaches, and one without a
// reliability score the neutral reliability weight 1, a provider's steady
// best. With rank_measured_first on, URL-probe evidence comes first, then a
// reliability score, and the unmeasured still fill the rest. Every rank here
// is drawn whole or not at all, and a draw is listed in the old order.
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
		lent := evidenceTestNames(selectOnlineClientScores(pool, facets, n, evidence, evidenceTestUniform(int64(n))), names)
		if !slices.Equal(lent, want[:min(n, len(want))]) {
			t.Fatalf("with %d places the Online fallback lent %v, want %v", n, lent, want[:min(n, len(want))])
		}
		legacy := legacySelectOnlineForTest(pool, facets, n)
		assert.Equal(t, len(lent), len(legacy))
	}
}

// With rank_measured_first off, the default, the Online fallback's order is
// exactly the one before, which on this fixture lends the unprobed first, and
// draws nothing: absent, false, and a value that is not a bool, which keeps
// the block's defaults.
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
			got := selectOnlineClientScores(pool, facets, n, evidence, evidenceTestNoDraw(t))
			want := legacySelectOnlineForTest(pool, facets, n)
			if !slices.Equal(got, want) {
				t.Fatalf("provider.yml %q, %d places: %v, before %v", config, n, evidenceTestNames(got, names), evidenceTestNames(want, names))
			}
		}
	}
	assert.Equal(t, evidenceTestNames(selectOnlineClientScores(pool, facets, 3, nil, evidenceTestNoDraw(t)), names), []string{"unprobed_unscored_a", "unprobed_unscored_b", "unprobed_unscored_c"})
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

// The family preference stays the first key (connect/IPV6.md B2: the server
// draws dualstack first, so v4-only exits only top up), and the evidence
// orders each family: an unprobed dualstack provider is lent before a
// measured v4-only one, and within each family the measured come first. The
// Online fallback and the native draw both keep it.
func TestDrawsKeepTheFamilyPreferenceAheadOfEvidence(t *testing.T) {
	now := server.NowUtc()
	facets, err := ipFamilyFacetsForFilter("")
	if err != nil {
		t.Fatal(err)
	}
	on := evidenceTestRequestEvidence(evidenceTestMainConfigRankingOn, false, now)

	online, onlineNames := evidenceTestExportedPool(t, now, RankModeQuality, []*evidenceTestProvider{
		{name: "measured_dualstack", dualstack: true, urlOk: 5, urlTotal: 10, lookbacks: evidenceTestScored(0.5), hasLatencyTest: true, latencyMillis: 20, bytesPerSecond: 50 * Mib},
		{name: "unprobed_dualstack", dualstack: true, hasLatencyTest: true, latencyMillis: 20, bytesPerSecond: 50 * Mib},
		{name: "measured_v4", urlOk: 5, urlTotal: 10, lookbacks: evidenceTestScored(0.5), hasLatencyTest: true, latencyMillis: 20, bytesPerSecond: 50 * Mib},
		{name: "unprobed_v4", hasLatencyTest: true, latencyMillis: 20, bytesPerSecond: 50 * Mib},
	})
	assert.Equal(t, evidenceTestNames(selectOnlineClientScores(online, facets, 4, on, evidenceTestUniform(1)), onlineNames), []string{"measured_dualstack", "unprobed_dualstack", "measured_v4", "unprobed_v4"})
	// two places: both dualstack providers, the unprobed one included
	assert.Equal(t, evidenceTestNames(selectOnlineClientScores(online, facets, 2, on, evidenceTestUniform(1)), onlineNames), []string{"measured_dualstack", "unprobed_dualstack"})
	// off: each family in the order before, the unprobed first
	assert.Equal(t, evidenceTestNames(selectOnlineClientScores(online, facets, 4, nil, evidenceTestNoDraw(t)), onlineNames), []string{"unprobed_dualstack", "measured_dualstack", "unprobed_v4", "measured_v4"})

	natives, nativeNames := evidenceTestExportedPool(t, now, RankModeSpeed, []*evidenceTestProvider{
		{name: "scored_dualstack", dualstack: true, urlOk: 10, urlTotal: 10, lookbacks: evidenceTestScored(0.9), hasLatencyTest: true, latencyMillis: 10, bytesPerSecond: 50 * Mib},
		{name: "unscored_dualstack", dualstack: true, urlOk: 10, urlTotal: 10, hasLatencyTest: true, latencyMillis: 10, bytesPerSecond: 50 * Mib},
		{name: "scored_v4", urlOk: 10, urlTotal: 10, lookbacks: evidenceTestScored(0.9), hasLatencyTest: true, latencyMillis: 10, bytesPerSecond: 50 * Mib},
	})
	speedNatives := map[server.Id]*ClientScore{}
	for clientId, clientScore := range natives {
		if clientScore.PassesMinimums[RankModeSpeed] {
			speedNatives[clientId] = clientScore
		}
	}
	assert.Equal(t, len(speedNatives), 3)
	assert.Equal(t, evidenceTestNames(selectClientScoresByWeight(speedNatives, RankModeSpeed, facets, 3, on), nativeNames), []string{"scored_dualstack", "unscored_dualstack", "scored_v4"})
	for range 20 {
		drawn := evidenceTestNames(selectClientScoresByWeight(speedNatives, RankModeSpeed, facets, 2, on), nativeNames)
		slices.Sort(drawn)
		assert.Equal(t, drawn, []string{"scored_dualstack", "unscored_dualstack"})
	}
}

// Thirty measured providers with failing URL ratios hold the Online fallback
// of one country, which lends ten places to each of a thousand Quality
// answers. Their keys never tie, so a sort would lend every answer the same
// ten. The draw weighted by the key spreads the answers over all thirty, the
// better-weighted more often, and every answer still takes ten of them.
func TestOnlineFallbackSpreadsAnswersOverEachEvidenceRank(t *testing.T) {
	now := server.NowUtc()
	providers := []*evidenceTestProvider{}
	for i := range 30 {
		providers = append(providers, &evidenceTestProvider{
			name:           fmt.Sprintf("measured_%02d", i),
			clientId:       evidenceTestFixedId(i),
			urlOk:          5 + i%3,
			urlTotal:       10,
			lookbacks:      evidenceTestScored(0.8 + 0.005*float64(i)),
			hasLatencyTest: true,
			latencyMillis:  20,
			bytesPerSecond: 50 * Mib,
		})
	}
	pool, names := evidenceTestExportedPool(t, now, RankModeQuality, providers)
	facets, err := ipFamilyFacetsForFilter("")
	if err != nil {
		t.Fatal(err)
	}
	evidence := evidenceTestRequestEvidence(evidenceTestMainConfigRankingOn, false, now)
	uniform := evidenceTestUniform(1)
	lent := map[server.Id]int{}
	for range 1000 {
		answer := selectOnlineClientScores(pool, facets, 10, evidence, uniform)
		distinct := map[server.Id]bool{}
		for _, clientId := range answer {
			distinct[clientId] = true
			lent[clientId] += 1
		}
		if len(answer) != 10 || len(distinct) != 10 {
			t.Fatalf("an answer lent %d providers, %d of them distinct, want 10", len(answer), len(distinct))
		}
	}

	byWeight := slices.Collect(maps.Keys(pool))
	slices.SortFunc(byWeight, func(a server.Id, b server.Id) int {
		return compareOnlineFallback(pool[a], pool[b])
	})
	for _, clientId := range byWeight {
		if count := lent[clientId]; count < 100 || 700 < count {
			t.Errorf("%s (weight %.3f) was lent to %d of 1000 answers, want 100 to 700", names[clientId], pool[clientId].ReliabilityWeight*onlineFallbackUrlWeight(pool[clientId]), count)
		}
	}
	best, worst := 0, 0
	for i := range 10 {
		best += lent[byWeight[i]]
		worst += lent[byWeight[len(byWeight)-1-i]]
	}
	if best <= worst {
		t.Errorf("the ten best-weighted providers were lent %d times, the ten worst %d: the draw ignores the weights", best, worst)
	}
	counts := []int{}
	for _, clientId := range byWeight {
		counts = append(counts, lent[clientId])
	}
	t.Logf("1000 answers of 10 from 30 measured providers, lent per provider from the best-weighted: %v (ten best %d, ten worst %d)", counts, best, worst)
}

// Within an evidence rank the Online fallback is taken speed tier by speed
// tier, past the speed cutoffs last (GEOMAP §10.3), and drawn by the key
// within a tier. Here the measured providers past the speed cutoffs carry the
// larger keys, so a draw by the key alone would lend them more often than the
// fast ones.
func TestOnlineFallbackDrawsSpeedTierByTier(t *testing.T) {
	now := server.NowUtc()
	providers := []*evidenceTestProvider{}
	add := func(kind string, i int, reliabilityWeight float64, urlOk int, hasLatencyTest bool, latencyMillis int) {
		providers = append(providers, &evidenceTestProvider{
			name:           fmt.Sprintf("%s_%d", kind, i),
			clientId:       evidenceTestFixedId(len(providers)),
			urlOk:          urlOk,
			urlTotal:       10,
			lookbacks:      evidenceTestScored(reliabilityWeight),
			hasLatencyTest: hasLatencyTest,
			latencyMillis:  latencyMillis,
			bytesPerSecond: 50 * Mib,
		})
	}
	for i := range 5 {
		// speed tier 0, with the smaller keys
		add("fast", i, 0.80+0.01*float64(i), 5, true, 10)
		// past the Speed latency cutoff, with the larger keys
		add("cutoff", i, 0.90+0.01*float64(i), 7, true, 60)
	}
	for i := range 2 {
		// speed tier 2: no latency test
		add("untested", i, 0.85, 6, false, 0)
	}
	pool, names := evidenceTestExportedPool(t, now, RankModeQuality, providers)
	kinds := func(clientIds []server.Id) []string {
		kinds := []string{}
		for _, clientId := range clientIds {
			name := names[clientId]
			kinds = append(kinds, name[:strings.LastIndex(name, "_")])
		}
		return kinds
	}
	tiers := map[string]int{}
	for clientId, clientScore := range pool {
		tiers[kinds([]server.Id{clientId})[0]] = clientScore.Tiers[RankModeSpeed]
	}
	assert.Equal(t, tiers, map[string]int{"fast": 0, "untested": 2, "cutoff": ClientScoreCutoffTier})
	facets, err := ipFamilyFacetsForFilter("")
	if err != nil {
		t.Fatal(err)
	}
	evidence := evidenceTestRequestEvidence(evidenceTestMainConfigRankingOn, false, now)
	uniform := evidenceTestUniform(1)
	for range 200 {
		// five places: the five fast providers, whatever the keys
		if lent := kinds(selectOnlineClientScores(pool, facets, 5, evidence, uniform)); !slices.Equal(lent, []string{"fast", "fast", "fast", "fast", "fast"}) {
			t.Fatalf("five places lent %v, want the five fast providers", lent)
		}
		// eight places: then both untested, then one past the cutoffs
		if lent := kinds(selectOnlineClientScores(pool, facets, 8, evidence, uniform)); !slices.Equal(lent, []string{"fast", "fast", "fast", "fast", "fast", "untested", "untested", "cutoff"}) {
			t.Fatalf("eight places lent %v, want the fast, then the untested, then one past the cutoffs", lent)
		}
	}
	// off, the order before: by the key, so past the cutoffs first here
	assert.Equal(t, kinds(selectOnlineClientScores(pool, facets, 5, nil, evidenceTestNoDraw(t))), []string{"cutoff", "cutoff", "cutoff", "cutoff", "cutoff"})
}

// The weighted draw takes up to n distinct ids, every positive weight before
// any other, each in proportion to its weight, and the same ids from the same
// source whatever order the ids come in.
func TestWeightedClientIdDraw(t *testing.T) {
	ids := []server.Id{evidenceTestFixedId(1), evidenceTestFixedId(2), evidenceTestFixedId(3), evidenceTestFixedId(4)}
	weights := map[server.Id]float64{ids[0]: 3, ids[1]: 1, ids[2]: 0, ids[3]: math.NaN()}
	weight := func(clientId server.Id) float64 {
		return weights[clientId]
	}
	uniform := evidenceTestUniform(1)
	assert.Equal(t, len(weightedClientIdDraw(ids, 0, weight, uniform)), 0)
	assert.Equal(t, len(weightedClientIdDraw(ids, -1, weight, uniform)), 0)
	assert.Equal(t, len(weightedClientIdDraw(nil, 2, weight, uniform)), 0)
	for range 50 {
		every := weightedClientIdDraw(ids, 9, weight, uniform)
		assert.Equal(t, len(every), 4)
		// the positive weights first, then the rest, which still fill
		positive := slices.Clone(every[:2])
		slices.SortFunc(positive, func(a server.Id, b server.Id) int { return a.Cmp(b) })
		assert.Equal(t, positive, ids[:2])
		rest := slices.Clone(every[2:])
		slices.SortFunc(rest, func(a server.Id, b server.Id) int { return a.Cmp(b) })
		assert.Equal(t, rest, ids[2:])
	}
	// three to one
	first := 0
	for range 4000 {
		if weightedClientIdDraw(ids, 1, weight, uniform)[0] == ids[0] {
			first += 1
		}
	}
	if first < 2800 || 3200 < first {
		t.Errorf("weight 3 against 1 was drawn first %d of 4000 times, want about 3000", first)
	}
	reversed := slices.Clone(ids)
	slices.Reverse(reversed)
	for seed := range int64(20) {
		assert.Equal(t, weightedClientIdDraw(ids, 2, weight, evidenceTestUniform(seed)), weightedClientIdDraw(reversed, 2, weight, evidenceTestUniform(seed)))
	}
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

// rank_measured_first reads the export's verdict as filterPool and the live
// api's reader do, at the same instant: a native whose oldest run left the
// window after the export, and a measured Online provider like it, rank as
// measured until the export's verdict ends at the bound, then by their
// reliability score alone.
func TestEvidenceRankFollowsTheExportVerdict(t *testing.T) {
	snapshot := egressValidityTestSnapshot
	bound := snapshot.Add(clientScoreEgressMaxStaleness)
	union, _ := egressValidityTestExport(t, DefaultEgressIndexSettings(), RankModeQuality, []*egressValidityTestProvider{
		{name: "native", newestAge: 3 * time.Minute, oldestAge: ProviderEgressHealthMaxAge - 5*time.Minute, urlOk: 20, urlTotal: 20},
		{name: "measured_online", newestAge: 3 * time.Minute, oldestAge: ProviderEgressHealthMaxAge - 5*time.Minute, urlOk: 2, urlTotal: 10},
	})
	assert.Equal(t, union["native"].PassesMinimums[RankModeQuality], true)
	assert.Equal(t, union["measured_online"].PassesMinimums[RankModeQuality], false)
	for name, clientScore := range union {
		for _, c := range []struct {
			at   time.Time
			want clientScoreEvidenceRank
		}{
			{at: snapshot.Add(10 * time.Minute), want: clientScoreEvidenceUrlAndReliability},
			{at: bound.Add(-time.Nanosecond), want: clientScoreEvidenceUrlAndReliability},
			{at: bound, want: clientScoreEvidenceReliabilityOnly},
			{at: bound.Add(time.Minute), want: clientScoreEvidenceReliabilityOnly},
		} {
			current := clientScore.urlEvidenceCurrent(c.at)
			if expired := egressValidityTestExpired(clientScore, c.at); current == expired {
				t.Fatalf("%s at the export + %s: urlEvidenceCurrent=%t but the live api's reader expired=%t", name, c.at.Sub(snapshot), current, expired)
			}
			if got := evidenceTestRequestEvidence(evidenceTestMainConfigRankingOn, false, c.at)(clientScore); got != c.want {
				t.Errorf("%s at the export + %s: rank %s, want %s", name, c.at.Sub(snapshot), got, c.want)
			}
		}
	}
}

// Without evidence a facet's draw sees its candidates themselves, in one
// group; with it, the ranks best first, each in the facet's order, and no
// empty group.
func TestClientScoreEvidenceGroups(t *testing.T) {
	now := server.NowUtc()
	future := now.Add(time.Hour)
	measured := &ClientScore{ClientId: evidenceTestFixedId(1), EgressValidUntil: &future}
	unknown := &ClientScore{ClientId: evidenceTestFixedId(2), LookbackClientScores: map[int]*ClientScore{0: {ReliabilityWeight: 1, IndependentReliabilityWeight: 1}}}
	alsoMeasured := &ClientScore{ClientId: evidenceTestFixedId(3), EgressValidUntil: &future}
	clientScores := map[server.Id]*ClientScore{measured.ClientId: measured, unknown.ClientId: unknown, alsoMeasured.ClientId: alsoMeasured}
	clientIds := []server.Id{unknown.ClientId, alsoMeasured.ClientId, measured.ClientId}

	groups := clientScoreEvidenceGroups(clientIds, clientScores, nil)
	assert.Equal(t, len(groups), 1)
	assert.Equal(t, &groups[0][0] == &clientIds[0], true)
	assert.Equal(t, len(groups[0]), len(clientIds))

	evidence := func(clientScore *ClientScore) clientScoreEvidenceRank {
		return clientScoreEvidence(clientScore, now)
	}
	groups = clientScoreEvidenceGroups(clientIds, clientScores, evidence)
	assert.Equal(t, groups, [][]server.Id{
		{alsoMeasured.ClientId, measured.ClientId},
		{unknown.ClientId},
	})
	assert.Equal(t, len(clientScoreEvidenceGroups([]server.Id{}, clientScores, evidence)), 0)
}

// The setting's gauge reads the request path's settings, as the draws do.
func TestFindProviders2RankMeasuredFirstGauge(t *testing.T) {
	read := func(config string) float64 {
		pop := server.Config.PushSimpleResource(providerConfigResourceName, []byte(config))
		defer pop()
		requestEgressIndexSettingsSnapshot.Store(nil)
		defer requestEgressIndexSettingsSnapshot.Store(nil)
		return testutil.ToFloat64(findProviders2RankMeasuredFirstEnabled)
	}
	assert.Equal(t, read(evidenceTestMainConfig), float64(0))
	assert.Equal(t, read(evidenceTestMainConfigRankingOn), float64(1))
	assert.Equal(t, read(evidenceTestMainConfigRankingOff), float64(0))
	assert.Equal(t, read(evidenceTestRankingUnreadableBool), float64(0))
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
