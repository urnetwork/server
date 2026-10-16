// The draws FindProviders2 makes for a Quality or Speed answer, and the
// evidence rank_measured_first orders them by.
//
// A provider nothing has measured reads as the best in both weights the draws
// order by: the URL weight of a provider without URL-probe evidence is the
// neutral 1, the most a measured one can reach (providerUrlProbeSuccessWeight,
// and filterPool once the evidence a score vouches for has expired), and a
// provider without a reliability score gets the source query's neutral
// lookback, both weights 1 (UpdateClientScores), the weight of a provider
// steady through the whole window. The Online fallback, which orders by their
// product, therefore lent the least-known providers first.
//
// While rank_measured_first is on, the evidence is the first key within each
// family facet of every draw. The request's family preference still comes
// first (connect/IPV6.md B2: dualstack first, so v4-only exits only top up),
// and within a facet the draws take the candidates rank by rank, best first.
// The native draws keep their weighted draw within each rank. The Online
// fallback, a strict sort by its key while the setting is off, takes each
// rank speed tier by speed tier, past the speed cutoffs last, and draws each
// tier weighted by that key instead: the measured providers' keys rarely tie,
// so a sort would lend every answer the same few. Supply is unchanged; the
// providers without evidence fill whatever the measured leave.
package model

import (
	"cmp"
	"maps"
	"math"
	mathrand "math/rand"
	"slices"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// What a draw knows of a provider, best first. URL-probe evidence outranks a
// reliability score: the network check is what the provider status tells an
// unchecked provider it waits for ("clients are offered this device only when
// checked providers run out", ProviderStatusReasonEgressUnprobed), and since
// URL probe admission needs reliability history (providerUrlProbeAdmissionSql)
// nearly every provider without a score is unprobed as well.
type clientScoreEvidenceRank int

const (
	// URL-probe evidence and a reliability score
	clientScoreEvidenceUrlAndReliability clientScoreEvidenceRank = iota
	// URL-probe evidence without a reliability score, as for an intent
	// provider probed in its qualification grace
	clientScoreEvidenceUrlOnly
	// a reliability score without URL-probe evidence
	clientScoreEvidenceReliabilityOnly
	// neither
	clientScoreEvidenceNone
	clientScoreEvidenceRankCount
)

// The rank as a metric label.
func (self clientScoreEvidenceRank) String() string {
	switch self {
	case clientScoreEvidenceUrlAndReliability:
		return "url_reliability"
	case clientScoreEvidenceUrlOnly:
		return "url"
	case clientScoreEvidenceReliabilityOnly:
		return "reliability"
	default:
		return "none"
	}
}

// Whether the score still vouches for URL-probe evidence at now: the export
// saw accepted URL outcomes in the evidence window, the only case in which it
// sets EgressValidUntil, and neither has the newest of them left the window
// since nor has the export's verdict outlived its bounded staleness
// (clientScoreEgressValidUntil). filterPool gives a score without it the
// neutral URL weight and takes its native membership away.
func (self *ClientScore) urlEvidenceCurrent(now time.Time) bool {
	return self.EgressValidUntil != nil && now.Before(*self.EgressValidUntil)
}

// Whether the score source found no reliability score row for the provider
// (client_connection_reliability_score). Its LEFT JOIN then gives the
// provider a single lookback, index 0 with both weights 1 (the COALESCE
// defaults of the per-location source query in UpdateClientScores and of
// clientScoreLocationGroupSourceSQL), the weights of a provider steady
// through the whole window. A scored provider has a longer lookback as
// well: the lookback windows nest, so the score pass that writes a provider's
// 5 minute row writes its hour and 12 hour rows with it, and the 12 hour row
// is the last to go. The one score that reads the same is a provider scored
// over the last 5 minutes alone, at full weight, which the pass writes only
// while the longer windows hold no usable block; it reads as unscored until
// the next pass. A lookback copy, which carries no lookbacks of its own, and
// a record without lookbacks read as scored.
func (self *ClientScore) reliabilityUnscored() bool {
	if len(self.LookbackClientScores) != 1 {
		return false
	}
	lookback, ok := self.LookbackClientScores[0]
	return ok && lookback != nil && lookback.ReliabilityWeight == 1 && lookback.IndependentReliabilityWeight == 1
}

// What a draw knows of the provider at now.
func clientScoreEvidence(clientScore *ClientScore, now time.Time) clientScoreEvidenceRank {
	rank := clientScoreEvidenceUrlAndReliability
	if !clientScore.urlEvidenceCurrent(now) {
		rank = clientScoreEvidenceReliabilityOnly
	}
	if clientScore.reliabilityUnscored() {
		rank += 1
	}
	return rank
}

// The evidence the draws of one FindProviders2 answer order by, judged at
// now: nil, the order before rank_measured_first, while the setting is off,
// and for force_minimum, whose draw takes every admitted provider alike.
func findProviders2Evidence(settings *EgressIndexSettings, forceMinimum bool, now time.Time) func(*ClientScore) clientScoreEvidenceRank {
	if !settings.RankMeasuredFirst || forceMinimum {
		return nil
	}
	return func(clientScore *ClientScore) clientScoreEvidenceRank {
		return clientScoreEvidence(clientScore, now)
	}
}

// One facet's candidates by evidence rank, best first, without the empty
// ranks, each in the order of clientIds. Without evidence, one group: the
// candidates themselves.
func clientScoreEvidenceGroups(
	clientIds []server.Id,
	clientScores map[server.Id]*ClientScore,
	evidence func(*ClientScore) clientScoreEvidenceRank,
) [][]server.Id {
	if evidence == nil {
		return [][]server.Id{clientIds}
	}
	groups := make([][]server.Id, clientScoreEvidenceRankCount)
	for _, clientId := range clientIds {
		rank := min(max(evidence(clientScores[clientId]), 0), clientScoreEvidenceRankCount-1)
		groups[rank] = append(groups[rank], clientId)
	}
	return slices.DeleteFunc(groups, func(group []server.Id) bool {
		return len(group) == 0
	})
}

// The candidates of one family facet.
func clientScoreFacetClientIds(candidateClientScores map[server.Id]*ClientScore, facet ipFamilyFacet) []server.Id {
	facetClientIds := []server.Id{}
	for clientId, clientScore := range candidateClientScores {
		if clientScore.ipFamilyFacet() == facet {
			facetClientIds = append(facetClientIds, clientId)
		}
	}
	return facetClientIds
}

// Draws up to n of the candidates by their weight in `mode` and bands the
// draw by their tier in `mode`. Weighted selection and tier banding run
// within each facet, and the preferred facet fills n first: a v4-capable
// request takes every dualstack provider it can before any v4-only one, so
// the single-family category only ever tops up a shortfall. With evidence,
// each evidence rank of a facet in turn, best first, is drawn this way before
// the next.
func selectClientScoresByWeight(
	candidateClientScores map[server.Id]*ClientScore,
	mode RankMode,
	facets []ipFamilyFacet,
	n int,
	evidence func(*ClientScore) clientScoreEvidenceRank,
) []server.Id {
	clientIds := []server.Id{}
	for _, facet := range facets {
		if n <= len(clientIds) {
			break
		}
		facetClientIds := clientScoreFacetClientIds(candidateClientScores, facet)
		for _, groupClientIds := range clientScoreEvidenceGroups(facetClientIds, candidateClientScores, evidence) {
			remainingCount := n - len(clientIds)
			if remainingCount <= 0 {
				break
			}
			mathrand.Shuffle(len(groupClientIds), func(i int, j int) {
				groupClientIds[i], groupClientIds[j] = groupClientIds[j], groupClientIds[i]
			})

			connect.WeightedSelectFunc(groupClientIds, remainingCount, func(clientId server.Id) float32 {
				clientScore := candidateClientScores[clientId]
				return clientScore.ScaledWeights[mode]
			})
			groupClientIds = groupClientIds[:min(remainingCount, len(groupClientIds))]

			// band by tier
			slices.SortStableFunc(groupClientIds, func(a server.Id, b server.Id) int {
				clientScoreA := candidateClientScores[a]
				clientScoreB := candidateClientScores[b]

				return clientScoreA.Tiers[mode] - clientScoreB.Tiers[mode]
			})
			clientIds = append(clientIds, groupClientIds...)
		}
	}
	return clientIds
}

// The URL weight the Online fallback reads: a record written before the
// weight existed has none, and reads as neutral.
func onlineFallbackUrlWeight(clientScore *ClientScore) float64 {
	if clientScore.UrlProbeSuccessWeight == 0 {
		return 1
	}
	return clientScore.UrlProbeSuccessWeight
}

// The Online fallback's order (connect/GEOMAP.md §10.3): reliability weight
// times URL weight, highest first, then the speed-mode performance
// adjustment -- the client-measured latency and throughput, past the speed
// cutoffs last. Missing measurements retain their ranking penalties without
// changing online admission.
func compareOnlineFallback(clientScoreA *ClientScore, clientScoreB *ClientScore) int {
	weightA := clientScoreA.ReliabilityWeight * onlineFallbackUrlWeight(clientScoreA)
	weightB := clientScoreB.ReliabilityWeight * onlineFallbackUrlWeight(clientScoreB)
	switch {
	case weightB < weightA:
		return -1
	case weightA < weightB:
		return 1
	}
	if d := clientScoreA.Tiers[RankModeSpeed] - clientScoreB.Tiers[RankModeSpeed]; d != 0 {
		return d
	}
	return clientScoreA.Scores[RankModeSpeed] - clientScoreB.Scores[RankModeSpeed]
}

// Draws up to n of the ids without replacement, each with a chance in
// proportion to its weight: the n largest log(u)/weight for u uniform in
// (0, 1] (Efraimidis and Spirakis). An id without a positive weight follows
// every one with, in random order, so the draw fills n whenever there are n
// ids. uniform returns values in [0, 1), as math/rand's Float64 does; the ids
// take its values in id order, so a given source draws the same ids whatever
// order they come in.
func weightedClientIdDraw(clientIds []server.Id, n int, weight func(server.Id) float64, uniform func() float64) []server.Id {
	type keyedClientId struct {
		clientId server.Id
		positive bool
		key      float64
	}
	orderedClientIds := slices.Clone(clientIds)
	slices.SortFunc(orderedClientIds, func(a server.Id, b server.Id) int {
		return a.Cmp(b)
	})
	keyed := make([]keyedClientId, 0, len(orderedClientIds))
	for _, clientId := range orderedClientIds {
		u := 1 - uniform()
		if w := weight(clientId); 0 < w {
			keyed = append(keyed, keyedClientId{clientId: clientId, positive: true, key: math.Log(u) / w})
		} else {
			keyed = append(keyed, keyedClientId{clientId: clientId, key: u})
		}
	}
	slices.SortStableFunc(keyed, func(a keyedClientId, b keyedClientId) int {
		if a.positive != b.positive {
			if a.positive {
				return -1
			}
			return 1
		}
		return cmp.Compare(b.key, a.key)
	})
	drawn := make([]server.Id, 0, max(0, min(n, len(keyed))))
	for _, k := range keyed[:cap(drawn)] {
		drawn = append(drawn, k.clientId)
	}
	return drawn
}

// One evidence rank's candidates by speed-mode tier, lowest first, so that
// those past the speed cutoffs (ClientScoreCutoffTier) come last, each tier
// in the order of clientIds.
func onlineFallbackSpeedTiers(clientIds []server.Id, clientScores map[server.Id]*ClientScore) [][]server.Id {
	tierClientIds := map[int][]server.Id{}
	for _, clientId := range clientIds {
		tier := clientScores[clientId].Tiers[RankModeSpeed]
		tierClientIds[tier] = append(tierClientIds[tier], clientId)
	}
	tiers := slices.Sorted(maps.Keys(tierClientIds))
	groups := make([][]server.Id, 0, len(tiers))
	for _, tier := range tiers {
		groups = append(groups, tierClientIds[tier])
	}
	return groups
}

// Takes up to n of the online bucket, each facet in the request's preference
// order. Without evidence, a facet is taken in the Online fallback's order
// (compareOnlineFallback). With evidence, each evidence rank of a facet in
// turn, best first, is taken speed tier by speed tier, the lowest first and
// those past the speed cutoffs last, as GEOMAP §10.3 orders them. Each tier is
// drawn weighted by the order's key -- reliability weight times URL weight,
// from uniform -- and the draw is listed in that order. A sort by the key
// would lend every answer the same providers, since measured providers' keys
// rarely tie; the draw spreads the answers over each tier in proportion to the
// key, and the tiers are few enough that the spread survives.
func selectOnlineClientScores(
	candidateClientScores map[server.Id]*ClientScore,
	facets []ipFamilyFacet,
	n int,
	evidence func(*ClientScore) clientScoreEvidenceRank,
	uniform func() float64,
) []server.Id {
	compare := func(a server.Id, b server.Id) int {
		return compareOnlineFallback(candidateClientScores[a], candidateClientScores[b])
	}
	clientIds := []server.Id{}
	for _, facet := range facets {
		if n <= len(clientIds) {
			break
		}
		facetClientIds := clientScoreFacetClientIds(candidateClientScores, facet)
		for _, groupClientIds := range clientScoreEvidenceGroups(facetClientIds, candidateClientScores, evidence) {
			remainingCount := n - len(clientIds)
			if remainingCount <= 0 {
				break
			}
			if evidence == nil {
				// full ties in no particular order
				mathrand.Shuffle(len(groupClientIds), func(i int, j int) {
					groupClientIds[i], groupClientIds[j] = groupClientIds[j], groupClientIds[i]
				})
				slices.SortStableFunc(groupClientIds, compare)
				clientIds = append(clientIds, groupClientIds[:min(remainingCount, len(groupClientIds))]...)
				continue
			}
			for _, tierClientIds := range onlineFallbackSpeedTiers(groupClientIds, candidateClientScores) {
				remainingCount := n - len(clientIds)
				if remainingCount <= 0 {
					break
				}
				drawn := weightedClientIdDraw(tierClientIds, remainingCount, func(clientId server.Id) float64 {
					clientScore := candidateClientScores[clientId]
					return clientScore.ReliabilityWeight * onlineFallbackUrlWeight(clientScore)
				}, uniform)
				slices.SortStableFunc(drawn, compare)
				clientIds = append(clientIds, drawn...)
			}
		}
	}
	return clientIds
}

// The providers the Online fallback lent Quality and Speed answers, by what
// the draw knew of them, whether or not rank_measured_first ordered the draw
// by it: the share without evidence is the unchecked supply those answers
// carry, before and after the setting.
var findProviders2OnlineFallbackEvidence = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "urnetwork_provider_backfill_online_evidence_total",
	Help: "Providers the FindProviders2 Online fallback lent, per requested rank mode and the evidence the draw had: url_reliability, url, reliability or none",
}, []string{"rank_mode", "evidence"})

// Each process's effective rank_measured_first, as its FindProviders2 draws
// read it (through the request settings, at most RequestSettingsMaxAge old).
// Configuration, not request outcomes, like the native reader's gauge: a
// process that answers no discovery reports its configuration all the same.
var findProviders2RankMeasuredFirstEnabled = prometheus.NewGaugeFunc(prometheus.GaugeOpts{
	Name: "urnetwork_findproviders2_rank_measured_first_enabled",
	Help: "Effective cached rank_measured_first configuration for non-forced Quality and Speed discovery draws; does not attest request outcomes",
}, func() float64 {
	if requestEgressIndexSettings().RankMeasuredFirst {
		return 1
	}
	return 0
})

// Every label pair is exported from the start, so a share is defined before
// the first answer.
func init() {
	for _, rankMode := range []RankMode{RankModeQuality, RankModeSpeed} {
		for rank := clientScoreEvidenceUrlAndReliability; rank < clientScoreEvidenceRankCount; rank += 1 {
			findProviders2OnlineFallbackEvidence.WithLabelValues(rankMode, rank.String())
		}
	}
	prometheus.MustRegister(findProviders2OnlineFallbackEvidence, findProviders2RankMeasuredFirstEnabled)
}
