package model

import (
	"maps"
	"slices"
)

// The ranking UpdateClientScores gives each pool score, shared with the
// per-provider status (GetProviderStatuses) so a provider is told the numbers
// FindProviders2 actually draws and orders it by.

// One rank mode's performance targets (connect/GEOMAP.md §10.1): past a
// cutoff excludes the provider from the mode's tiers, between the threshold
// and the cutoff adds score points.
type clientScorePerformanceTarget struct {
	relativeLatencyMillisThreshold int
	relativeLatencyMillisCutoff    int
	relativeLatencyMillisPerScore  int
	bytesPerSecondThreshold        ByteCount
	bytesPerSecondCutoff           ByteCount
	bytesPerSecondPerScore         ByteCount
}

var clientScorePerformanceTargets = map[RankMode]clientScorePerformanceTarget{
	RankModeQuality: clientScorePerformanceTarget{
		relativeLatencyMillisThreshold: 50,
		relativeLatencyMillisCutoff:    200,
		relativeLatencyMillisPerScore:  20,
		bytesPerSecondThreshold:        8 * Mib,
		bytesPerSecondCutoff:           800 * Kib,
		bytesPerSecondPerScore:         200 * Kib,
	},
	RankModeSpeed: clientScorePerformanceTarget{
		relativeLatencyMillisThreshold: 20,
		relativeLatencyMillisCutoff:    50,
		relativeLatencyMillisPerScore:  5,
		bytesPerSecondThreshold:        40 * Mib,
		bytesPerSecondCutoff:           4 * Mib,
		bytesPerSecondPerScore:         1 * Mib,
	},
}

// A missing latency or throughput test costs two tiers.
const clientScoreMissingLatencyScore = 2 * ClientScorePerTier
const clientScoreMissingSpeedScore = 2 * ClientScorePerTier

// The score the selection weight treats as the worst: a minimum score at or
// past it scales the weight down to clientScoreMinScoreScale.
const clientScoreMinimumMaxScore = 2 * ClientScorePerTier

// The range each factor scales a native provider's selection weight over.
const (
	clientScoreMinReliabilityWeightScale = 0.1
	clientScoreMaxReliabilityWeightScale = 1.0
	clientScoreMinScoreScale             = 0.1
	clientScoreMaxScoreScale             = 1.0
)

// The old fields stay authoritative wherever the new one is empty
// (connect/GEOMAP.md §10.4): a row the new rollup has not written ranks
// exactly as it did, on its net-type score in both modes. With the index, the
// base is the index in quality and zero in speed (§10.3), and the minimum
// reads a zero base.
func clientScoreRankModeBases(netTypeScore int, netTypeScoreSpeed int, egressIndex *int) (rankModeBases map[RankMode]int, rankModeMinimumBases map[RankMode]int) {
	rankModeBases = map[RankMode]int{
		RankModeQuality: netTypeScore,
		RankModeSpeed:   netTypeScoreSpeed,
	}
	rankModeMinimumBases = rankModeBases
	if egressIndex != nil {
		rankModeBases = map[RankMode]int{
			RankModeQuality: *egressIndex,
			RankModeSpeed:   0,
		}
		rankModeMinimumBases = map[RankMode]int{
			RankModeQuality: 0,
			RankModeSpeed:   0,
		}
	}
	return
}

// Per mode, score = min(20·base + adjust, MaxClientScore) and the tier its
// twentieths, where the performance tests make the adjust and a cutoff
// excludes (connect/GEOMAP.md §10.1). rankModeMinimumBases is the base of the
// score the minimum and the weight read (see ClientScore.minimumScores).
func setClientScoreRanks(
	clientScore *ClientScore,
	rankModeBases map[RankMode]int,
	rankModeMinimumBases map[RankMode]int,
	minRelativeLatencyMillis int,
	maxBytesPerSecond ByteCount,
	hasLatencyTest bool,
	hasSpeedTest bool,
) {
	clientScore.minimumScores = map[string]int{}
	for rankMode, target := range clientScorePerformanceTargets {
		exclude := false
		scoreAdjust := 0

		if hasLatencyTest {
			if target.relativeLatencyMillisCutoff < minRelativeLatencyMillis {
				exclude = true
			} else if d := minRelativeLatencyMillis - target.relativeLatencyMillisThreshold; 0 < d {
				scoreAdjust += (d + target.relativeLatencyMillisPerScore/2) / target.relativeLatencyMillisPerScore
			}
		} else {
			scoreAdjust += clientScoreMissingLatencyScore
		}

		if hasSpeedTest {
			if maxBytesPerSecond < target.bytesPerSecondCutoff {
				exclude = true
			} else if d := target.bytesPerSecondThreshold - maxBytesPerSecond; 0 < d {
				scoreAdjust += int((d + target.bytesPerSecondPerScore/2) / target.bytesPerSecondPerScore)
			}
		} else {
			scoreAdjust += clientScoreMissingSpeedScore
		}

		if !exclude {
			score := min(
				ClientScorePerTier*rankModeBases[rankMode]+scoreAdjust,
				MaxClientScore,
			)
			clientScore.Scores[rankMode] = score
			clientScore.Tiers[rankMode] = score / ClientScorePerTier
			clientScore.minimumScores[rankMode] = min(
				ClientScorePerTier*rankModeMinimumBases[rankMode]+scoreAdjust,
				MaxClientScore,
			)
		} else {
			clientScore.Scores[rankMode] = MaxClientScore
			clientScore.Tiers[rankMode] = ClientScoreCutoffTier
			clientScore.minimumScores[rankMode] = MaxClientScore
		}
	}
}

// Sets a pool score to its lowest lookback and decides it with the shared
// egress decision: online, each mode's native membership and the weight the
// mode draws it by. Every location and group map a provider sits in, and its
// per-provider status, rank it this way.
func rankClientScore(
	clientScore *ClientScore,
	decision providerEgressDecision,
	countFilter providerCountFilter,
	egressSettings *EgressIndexSettings,
	minIndependentReliabilityWeights map[int]float64,
) {
	lookbackIndexes := slices.Collect(maps.Keys(clientScore.LookbackClientScores))
	slices.Sort(lookbackIndexes)
	minLookbackIndex := lookbackIndexes[0]

	minClientScore := clientScore.LookbackClientScores[minLookbackIndex]

	clientScore.Scores = minClientScore.Scores
	clientScore.ReliabilityWeight = minClientScore.ReliabilityWeight
	clientScore.IndependentReliabilityWeight = minClientScore.IndependentReliabilityWeight
	clientScore.Tiers = minClientScore.Tiers
	clientScore.MinRelativeLatencyMillis = minClientScore.MinRelativeLatencyMillis
	clientScore.MaxBytesPerSecond = minClientScore.MaxBytesPerSecond
	clientScore.HasLatencyTest = minClientScore.HasLatencyTest
	clientScore.HasSpeedTest = minClientScore.HasSpeedTest
	clientScore.minimumScores = minClientScore.minimumScores

	clientScore.ScaledWeights = map[string]float32{}
	clientScore.PassesMinimums = map[string]bool{}

	// Both native modes require passing accepted URL evidence; only quality
	// additionally excludes the explicit ARIN non-quality class.
	rankModePassesBucket := map[RankMode]bool{
		RankModeQuality: decision.quality,
		RankModeSpeed:   decision.speed,
	}

	// Online is all common-gate passes. Missing latency/throughput tests
	// and low performance change ordering without reducing membership.
	weights := map[int]float64{}
	for lookbackIndex, lookbackClientScore := range clientScore.LookbackClientScores {
		weights[lookbackIndex] = lookbackClientScore.IndependentReliabilityWeight
	}
	passesReliability := providerReliabilityPasses(weights, minIndependentReliabilityWeights)
	clientScore.Online = decision.online && passesReliability
	clientScore.UrlProbeSuccessWeight = providerUrlProbeSuccessWeight(countFilter.healthCounts[clientScore.ClientId])
	if counts, ok := countFilter.healthCounts[clientScore.ClientId]; ok && counts.Total > 0 {
		validUntil := counts.FirstMeasuredAt.Add(min(egressSettings.EvidenceMaxAge, ProviderEgressHealthMaxAge))
		clientScore.EgressValidUntil = &validUntil
	}

	for _, rankMode := range slices.Collect(maps.Keys(clientScore.Scores)) {
		passesMinimum := rankModePassesBucket[rankMode] && passesReliability

		if passesMinimum {
			u := max(0.0, min(1.0, float64(minClientScore.IndependentReliabilityWeight-minIndependentReliabilityWeights[minLookbackIndex])/(1.0-minIndependentReliabilityWeights[minLookbackIndex])))
			reliabilityWeightScale := (1-u)*clientScoreMinReliabilityWeightScale + u*clientScoreMaxReliabilityWeightScale
			v := max(0.0, min(1.0, float64(clientScoreMinimumMaxScore-clientScore.minimumScores[rankMode])/float64(clientScoreMinimumMaxScore)))
			scoreScale := (1-v)*clientScoreMinScoreScale + v*clientScoreMaxScoreScale
			clientScore.ScaledWeights[rankMode] = float32(reliabilityWeightScale * clientScore.ReliabilityWeight * scoreScale * clientScore.UrlProbeSuccessWeight)
			clientScore.PassesMinimums[rankMode] = true
		}
	}
}
