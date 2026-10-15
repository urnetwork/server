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
// product, therefore lent the least known providers first. While rank_measured_first is on, the
// evidence is the first key of every draw instead: the draws take the
// candidates rank by rank, best first, and each rank in the order the draw
// gave it before, the neutral weights included. Supply is unchanged; the
// providers without evidence fill whatever the measured leave.
package model

import (
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
// sets EgressValidUntil, and the first of them has not left the window since.
// filterPool gives a score without it the neutral URL weight and takes its
// native membership away.
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

// The candidates by evidence rank, best first, without the empty ranks.
// Without evidence, one group: the candidates themselves.
func clientScoreEvidenceGroups(
	candidateClientScores map[server.Id]*ClientScore,
	evidence func(*ClientScore) clientScoreEvidenceRank,
) []map[server.Id]*ClientScore {
	if evidence == nil {
		return []map[server.Id]*ClientScore{candidateClientScores}
	}
	groups := make([]map[server.Id]*ClientScore, clientScoreEvidenceRankCount)
	for clientId, clientScore := range candidateClientScores {
		rank := min(max(evidence(clientScore), 0), clientScoreEvidenceRankCount-1)
		if groups[rank] == nil {
			groups[rank] = map[server.Id]*ClientScore{}
		}
		groups[rank][clientId] = clientScore
	}
	return slices.DeleteFunc(groups, func(group map[server.Id]*ClientScore) bool {
		return len(group) == 0
	})
}

// Draws up to n of the candidates by their weight in `mode` and bands the
// draw by their tier in `mode`. Weighted selection and tier banding run
// within each facet, and the preferred facet fills n first: a v4-capable
// request takes every dualstack provider it can before any v4-only one, so
// the single-family category only ever tops up a shortfall. With evidence,
// each evidence rank in turn, best first, is drawn this way before the next.
func selectClientScoresByWeight(
	candidateClientScores map[server.Id]*ClientScore,
	mode RankMode,
	facets []ipFamilyFacet,
	n int,
	evidence func(*ClientScore) clientScoreEvidenceRank,
) []server.Id {
	clientIds := []server.Id{}
	for _, group := range clientScoreEvidenceGroups(candidateClientScores, evidence) {
		for _, facet := range facets {
			remainingCount := n - len(clientIds)
			if remainingCount <= 0 {
				break
			}
			facetClientIds := []server.Id{}
			for clientId, clientScore := range group {
				if clientScore.ipFamilyFacet() == facet {
					facetClientIds = append(facetClientIds, clientId)
				}
			}
			mathrand.Shuffle(len(facetClientIds), func(i int, j int) {
				facetClientIds[i], facetClientIds[j] = facetClientIds[j], facetClientIds[i]
			})

			connect.WeightedSelectFunc(facetClientIds, remainingCount, func(clientId server.Id) float32 {
				clientScore := group[clientId]
				return clientScore.ScaledWeights[mode]
			})
			facetClientIds = facetClientIds[:min(remainingCount, len(facetClientIds))]

			// band by tier
			slices.SortStableFunc(facetClientIds, func(a server.Id, b server.Id) int {
				clientScoreA := group[a]
				clientScoreB := group[b]

				return clientScoreA.Tiers[mode] - clientScoreB.Tiers[mode]
			})
			clientIds = append(clientIds, facetClientIds...)
		}
	}
	return clientIds
}

// Takes up to n of the online bucket in its order (connect/GEOMAP.md §10.3):
// reliability weight times URL weight, highest first, then the speed-mode
// performance adjustment -- the client-measured latency and throughput, past
// the speed cutoffs last. Missing measurements retain their ranking penalties
// without changing online admission. Each facet keeps the request's existing
// preference order. With evidence, each evidence rank in turn, best first, is
// taken this way before the next.
func selectOnlineClientScores(
	candidateClientScores map[server.Id]*ClientScore,
	facets []ipFamilyFacet,
	n int,
	evidence func(*ClientScore) clientScoreEvidenceRank,
) []server.Id {
	clientIds := []server.Id{}
	for _, group := range clientScoreEvidenceGroups(candidateClientScores, evidence) {
		for _, facet := range facets {
			remainingCount := n - len(clientIds)
			if remainingCount <= 0 {
				break
			}
			facetClientIds := []server.Id{}
			for clientId, clientScore := range group {
				if clientScore.ipFamilyFacet() == facet {
					facetClientIds = append(facetClientIds, clientId)
				}
			}
			// full ties in no particular order
			mathrand.Shuffle(len(facetClientIds), func(i int, j int) {
				facetClientIds[i], facetClientIds[j] = facetClientIds[j], facetClientIds[i]
			})
			slices.SortStableFunc(facetClientIds, func(a server.Id, b server.Id) int {
				clientScoreA := group[a]
				clientScoreB := group[b]
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

// Every label pair is exported from the start, so a share is defined before
// the first answer.
func init() {
	for _, rankMode := range []RankMode{RankModeQuality, RankModeSpeed} {
		for rank := clientScoreEvidenceUrlAndReliability; rank < clientScoreEvidenceRankCount; rank += 1 {
			findProviders2OnlineFallbackEvidence.WithLabelValues(rankMode, rank.String())
		}
	}
	prometheus.MustRegister(findProviders2OnlineFallbackEvidence)
}
