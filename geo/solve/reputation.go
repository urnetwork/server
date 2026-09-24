// Source reputation (§5.5): each node scored against the population on five
// statistics, and the weights and exclusions the scores give the next solve.

package solve

import (
	"math"
)

// Source reputation (§5.5): reversion to the mean. After a solve, each node is
// scored on five statistics, each a z-score against the population of nodes
// it is defined for:
//
//   - scatter, the RMS residual d(p_s, p_t) − d̂_st over the terms the node is
//     the source of;
//   - bias, the mean signed residual over the same terms: whether its round
//     trips run systematically long (negative) or short against the geometry
//     the rest of the graph agrees on;
//   - coverage, the share of the peers it was expected to measure that it has
//     terms with (D26): its distinct targets over Node.ExpectedPeers, capped
//     at 1, since every pinger samples its peers by design and must never be
//     marked down for that. With no expected sample (0) the share is of the
//     peers available to it, a peer being available when some source
//     measured it, so a provider (which can only ping extenders) and an
//     extender (which pings the other extenders) are each measured against
//     what they could have pinged;
//   - its refusal rate as pinger and its refusal rate as target.
//
// Four readings the section leaves open are settled here, each because the
// alternative defeats its purpose:
//
//   - Only the side of a statistic that marks a bad source counts. A scatter
//     below the mean is a source agreeing with the consensus better than most,
//     a coverage above it one measuring more broadly, a refusal rate below it
//     one refused (or refusing) less: none of that is unlike the mean in the
//     way that matters, and nothing is ever added to a source's standing by
//     good behaviour, only not subtracted. Bias counts both ways: round trips
//     that run short are as inconsistent as long ones, and a colluding target
//     can co-sign a claim the gate would have stopped.
//   - Coverage lowers the weight but never excludes (§5.6 test 3). It is
//     evidence about which pairs a source chose to measure, not about the
//     numbers it reported, and a truthful source that measured a subset
//     reported nothing wrong. It also cannot be a reliable trigger: against a
//     population that all measured everything, one source's coverage z-score
//     is −√(N−1) however many peers it did measure, which passes any fixed
//     threshold once N is large enough. Exclusion takes evidence against the
//     numbers themselves (scatter, bias) or against the protocol (refusals).
//   - A z-score divides by the population's standard deviation, but never by
//     less than the statistic's spread floor (Settings). A population tighter
//     than the floor has no spread worth standardizing: one refusal among
//     sources that were never refused, or rounding among residuals that are
//     all zero, would otherwise be many sigma out.
//   - The population always includes every node the statistic is defined
//     for, excluded ones too: a source is judged against everyone, and
//     dropping the outliers from the population would make every honest
//     source's ordinary spread look like an outlier in the next round.
//
// A source's weight is q = clamp(1/(1 + z_max²), MinQ, 1) over the five
// one-sided z-scores. It scales every term the source measured and nothing
// else: a target did not produce the number on a term, it only co-signed that
// the number was not undercut. A source past ExcludeZ on scatter, bias or
// either refusal rate is left out of the next solve entirely -- its terms
// carry no weight at all, not MinQ's -- and listed. It is still solved as a
// target from the terms others measured toward it, which is also what lets
// the next scoring judge its own claims against a geometry they had no part
// in. The genesis term is never reweighted: the anchor is the anchor.

// The five statistics, in a fixed order.
const (
	statisticScatter = iota
	statisticBias
	statisticCoverage
	statisticRefusalAsPinger
	statisticRefusalAsTarget
	statisticCount
)

// Coverage and the refusal rates, which do not depend on the geometry.
type fixedStatistics struct {
	values [statisticCount][]float64
	has    [statisticCount][]bool
}

// One scoring: each statistic's values, whether each node has one, their
// z-scores and the populations they were taken against, and the weights and
// exclusions they give.
type scoreSet struct {
	values      [statisticCount][]float64
	has         [statisticCount][]bool
	z           [statisticCount][]float64
	populations [statisticCount]PopulationStatistic
	q           []float64
	excluded    []bool
}

// Coverage and the refusal rates of every node, taken once per solve since
// the geometry does not move them.
func (self *problem) newFixedStatistics(nodeIdRefusals map[string]Refusals) fixedStatistics {
	n := len(self.nodes)
	statistics := fixedStatistics{}
	for s := 0; s < statisticCount; s += 1 {
		statistics.values[s] = make([]float64, n)
		statistics.has[s] = make([]bool, n)
	}

	targetCount := 0
	for i := 0; i < n; i += 1 {
		if self.incomingStarts[i] < self.incomingStarts[i+1] {
			targetCount += 1
		}
	}
	self.parallelNodes(func(i int) {
		// one term per ordered pair, so a source's terms are its distinct
		// targets
		measured := int(self.sourceStarts[i+1] - self.sourceStarts[i])
		if measured == 0 {
			return
		}
		expected := self.nodes[i].ExpectedPeers
		if expected <= 0 {
			expected = targetCount
			if self.incomingStarts[i] < self.incomingStarts[i+1] {
				expected -= 1
			}
		}
		if 0 < expected {
			statistics.values[statisticCoverage][i] = min(1, float64(measured)/float64(expected))
			statistics.has[statisticCoverage][i] = true
		}
	})

	if 0 < len(nodeIdRefusals) {
		self.parallelNodes(func(i int) {
			counts, ok := nodeIdRefusals[self.nodes[i].Id]
			if !ok {
				return
			}
			if 0 < counts.PingsAsPinger {
				statistics.values[statisticRefusalAsPinger][i] = refusalRate(counts.AsPinger, counts.PingsAsPinger)
				statistics.has[statisticRefusalAsPinger][i] = true
			}
			if 0 < counts.PingsAsTarget {
				statistics.values[statisticRefusalAsTarget][i] = refusalRate(counts.AsTarget, counts.PingsAsTarget)
				statistics.has[statisticRefusalAsTarget][i] = true
			}
		})
	}
	return statistics
}

// The share of attestations refused, at most 1.
func refusalRate(refused int, attestations int) float64 {
	return min(1, float64(max(0, refused))/float64(attestations))
}

// Every node scored against the current geometry: each node's own
// statistics in parallel, and the population's in fixed-order sums.
func (self *problem) score() *scoreSet {
	settings := self.settings
	n := len(self.nodes)
	scores := &scoreSet{
		q:        make([]float64, n),
		excluded: make([]bool, n),
	}
	for s := 0; s < statisticCount; s += 1 {
		scores.values[s] = make([]float64, n)
		scores.has[s] = make([]bool, n)
		scores.z[s] = make([]float64, n)
	}

	self.parallelNodes(func(i int) {
		start, end := self.sourceStarts[i], self.sourceStarts[i+1]
		if start == end {
			return
		}
		// every term the node measured, whether or not the last solve
		// weighed it
		var sum float64
		var sumSquares float64
		for k := start; k < end; k += 1 {
			term := &self.terms[k]
			residual := surfaceKm(self.positions[i], self.positions[term.target]) - self.impliedKm(term)
			sum += residual
			sumSquares += residual * residual
		}
		count := float64(end - start)
		scores.values[statisticScatter][i] = math.Sqrt(sumSquares / count)
		scores.has[statisticScatter][i] = true
		scores.values[statisticBias][i] = sum / count
		scores.has[statisticBias][i] = true
	})
	for _, s := range []int{statisticCoverage, statisticRefusalAsPinger, statisticRefusalAsTarget} {
		copy(scores.values[s], self.fixedStatistics.values[s])
		copy(scores.has[s], self.fixedStatistics.has[s])
	}

	spreadFloors := [statisticCount]float64{
		statisticScatter:         settings.ScatterFloorKm,
		statisticBias:            settings.BiasFloorKm,
		statisticCoverage:        settings.CoverageFloor,
		statisticRefusalAsPinger: settings.RefusalFloor,
		statisticRefusalAsTarget: settings.RefusalFloor,
	}
	for s := 0; s < statisticCount; s += 1 {
		population := self.populationStatistic(scores.values[s], scores.has[s])
		scores.populations[s] = population
		spread := max(population.StdDev, spreadFloors[s])
		if !(0 < spread) {
			continue
		}
		self.parallelNodes(func(i int) {
			if scores.has[s][i] {
				scores.z[s][i] = (scores.values[s][i] - population.Mean) / spread
			}
		})
	}

	self.parallelNodes(func(i int) {
		scatter := max(0, scores.z[statisticScatter][i])
		bias := math.Abs(scores.z[statisticBias][i])
		coverage := max(0, -scores.z[statisticCoverage][i])
		refusalAsPinger := max(0, scores.z[statisticRefusalAsPinger][i])
		refusalAsTarget := max(0, scores.z[statisticRefusalAsTarget][i])
		// the evidence against the numbers and the protocol, which alone can
		// exclude
		evidence := max(scatter, bias, refusalAsPinger, refusalAsTarget)
		zMax := max(evidence, coverage)
		scores.q[i] = min(1, max(settings.MinQ, 1/(1+zMax*zMax)))
		scores.excluded[i] = settings.ExcludeZ < evidence
	})
	return scores
}

// Sets the source weights of the next solve from a scoring.
func (self *problem) reweight(scores *scoreSet) {
	self.scores = scores
	copy(self.q, scores.q)
	copy(self.excluded, scores.excluded)
	self.parallel(self.chunks(len(self.terms)), func(c int) {
		start, end := self.chunkRange(c, len(self.terms))
		for k := start; k < end; k += 1 {
			term := &self.terms[k]
			if self.excluded[term.source] {
				term.weight = 0
			} else {
				term.weight = float32(self.q[term.source] * self.baseWeight(term))
			}
		}
	})
}

// The mean and population standard deviation of the
// values that are defined, by two passes -- the second sums squared
// deviations from the mean rather than subtracting two large sums -- each a
// fixed-order sum.
func (self *problem) populationStatistic(values []float64, has []bool) PopulationStatistic {
	count := self.sumNodes(func(i int) float64 {
		if has[i] {
			return 1
		}
		return 0
	})
	if count == 0 {
		return PopulationStatistic{}
	}
	mean := self.sumNodes(func(i int) float64 {
		if has[i] {
			return values[i]
		}
		return 0
	}) / count
	sumSquares := self.sumNodes(func(i int) float64 {
		if has[i] {
			deviation := values[i] - mean
			return deviation * deviation
		}
		return 0
	})
	return PopulationStatistic{
		Count:  int(count),
		Mean:   mean,
		StdDev: math.Sqrt(sumSquares / count),
	}
}

// Copies node i's statistics and z-scores into its result.
func (self *scoreSet) fill(i int, nodeResult *NodeResult) {
	nodeResult.ScatterKm = self.values[statisticScatter][i]
	nodeResult.BiasKm = self.values[statisticBias][i]
	nodeResult.Coverage = self.values[statisticCoverage][i]
	nodeResult.RefusalRateAsPinger = self.values[statisticRefusalAsPinger][i]
	nodeResult.RefusalRateAsTarget = self.values[statisticRefusalAsTarget][i]
	nodeResult.ScatterZ = self.z[statisticScatter][i]
	nodeResult.BiasZ = self.z[statisticBias][i]
	nodeResult.CoverageZ = self.z[statisticCoverage][i]
	nodeResult.RefusalAsPingerZ = self.z[statisticRefusalAsPinger][i]
	nodeResult.RefusalAsTargetZ = self.z[statisticRefusalAsTarget][i]
}

// The populations of the scoring, by statistic.
func (self *scoreSet) population() Population {
	return Population{
		ScatterKm:           self.populations[statisticScatter],
		BiasKm:              self.populations[statisticBias],
		Coverage:            self.populations[statisticCoverage],
		RefusalRateAsPinger: self.populations[statisticRefusalAsPinger],
		RefusalRateAsTarget: self.populations[statisticRefusalAsTarget],
	}
}
