// Package solve derives where providers and extenders are from the pings they
// co-sign (connect/GEOMAP.md §5). Each node starts from its genesis -- its
// GeoLite2 or egress-probe location, with an accuracy radius -- and moves by
// the least squares correction that trades the squared distance from genesis
// against the squared errors of its pings, with two hinges that bias it toward
// its genesis region and country. Every ping is weighed by the reputation of
// the source that measured it (§5.5), and a correction is published only when
// it improves on genesis (§5.4).
//
// The package is pure: it reads no database and imports nothing of the
// server. The derive job feeds it nodes, terms and refusal counts, and the
// acceptance tests of §5.6 run it on synthetic geometry.
package solve

import (
	"math"
	"slices"
)

// One provider or extender with at least one usable ping.
type Node struct {
	Id      string
	Genesis LatLon
	// The genesis accuracy radius, in km (Settings.GenesisRadiusKm). A wide
	// radius is a weak anchor the pings are free to move; a tight one barely
	// moves at all.
	RadiusKm float64
	// The genesis region (geo.ContainmentRegionKey) and country
	// (geo.ContainmentCountryKey) the containment terms bias the node toward.
	// Empty leaves that term out for the node.
	RegionKey  string
	CountryKey string
	// the correction a previous run derived, to start from; the zero offset
	// starts at genesis
	PreviousCorrection Offset
	// The distinct peers the node was expected to measure over the window:
	// the sample of the available peers its pinger draws (D26). Its coverage
	// is measured against this sample, so a source is never marked down for
	// subsampling by design. 0 measures it against every peer available, as
	// when each source is meant to measure all of them.
	ExpectedPeers int
}

// One source's co-signed pings toward one target over the window,
// aggregated (Aggregator). There is one per ordered pair: the two directions
// of a pair are two terms, each weighed by the reputation of the source that
// measured it, so one bad source cannot contaminate the honest measurement of
// the same pair from the other side.
type Term struct {
	Source string
	Target string
	RttMs  float64
	// the co-signed pings aggregated into RttMs
	Samples int
}

// One node's attestations over the window, counted as pinger and as
// target. A refused attestation is never a measurement (D8). Reporter-side
// counts remain reputation evidence; target-side totals carry no peer
// provenance and are diagnostic only, with no target weight or exclusion.
type Refusals struct {
	// the node's attestations, as pinger, that their target refused
	AsPinger int
	// the attestations reported refused by this target, not corroborated
	AsTarget int
	// every attestation the node made as pinger, refused or not
	PingsAsPinger int
	// every attestation the node received as target, refused or not
	PingsAsTarget int
}

// The hinge distances of the containment terms (§5.2), in km: how much nearer
// to p the nearest city outside the genesis region (country) is than the
// nearest city inside it, and 0 while the nearest city is inside. The place
// list implements it (geo.PlaceContainment). A solve looks hinges up from all
// its workers at once, so an implementation must be safe for concurrent use.
type Containment interface {
	// the region hinge at p for the region the key names
	RegionHinge(p LatLon, regionKey string) float64
	// the country hinge at p for the country the key names
	CountryHinge(p LatLon, countryKey string) float64
}

// What one node derived.
type NodeResult struct {
	Id      string
	Genesis LatLon
	// genesis ⊕ correction
	Position   LatLon
	Correction Offset

	// Over the terms touching the node that carried weight in the final solve
	// -- its own, and every other source's toward it, except those of an
	// excluded source: the co-signed pings aggregated into them, and the
	// distinct peers at their other ends.
	PingCount int
	PeerCount int
	// The RMS ping residual d(p_s, p_t) − d̂_st over the same terms, in km:
	// with every node at its derived position, and with this node back at its
	// genesis while every other node stays where it was derived. Holding the
	// peers fixed makes the comparison about this node's correction alone.
	ResidualKm        float64
	GenesisResidualKm float64
	// how far the node moved in the last sweep of the final solve: a node
	// still moving when the solve stopped has not arrived, and the publish
	// gate refuses it past PublishMaxLastStepKm
	LastStepKm float64

	// Reputation (§5.5): the weight of the node's own terms in the final
	// solve, whether they were left out of it instead, and the statistics and
	// z-scores of the scoring that decided both. A statistic the node has no
	// data for (no terms of its own, no attestations in that role) is 0, and
	// so is its z-score. With one ReputationRound nothing is scored: Q is 1
	// and the rest is 0. RefusalRateAsTarget and RefusalAsTargetZ are
	// diagnostic only; neither contributes to Q or Excluded.
	Q                   float64
	Excluded            bool
	SourceTermCount     int
	ScatterKm           float64
	BiasKm              float64
	Coverage            float64
	RefusalRateAsPinger float64
	RefusalRateAsTarget float64
	ScatterZ            float64
	BiasZ               float64
	CoverageZ           float64
	RefusalAsPingerZ    float64
	RefusalAsTargetZ    float64
}

// One reputation statistic over the nodes it is
// defined for.
type PopulationStatistic struct {
	Count  int
	Mean   float64
	StdDev float64
}

// The population each node's z-scores were taken against.
type Population struct {
	ScatterKm           PopulationStatistic
	BiasKm              PopulationStatistic
	Coverage            PopulationStatistic
	RefusalRateAsPinger PopulationStatistic
	RefusalRateAsTarget PopulationStatistic
}

// One derivation.
type Result struct {
	// one per valid node, in the order Solve was given them
	Nodes []NodeResult
	// the ids of the sources the final solve left out, ordered, for the
	// dashboard
	Excluded   []string
	Population Population
	// The sweeps each reputation round's solve took; whether the last solve
	// stopped before MaxIterations, on MinStepKm or on the objective's
	// stagnation; and whether that stop was the stagnation.
	Sweeps    []int
	Converged bool
	Stagnated bool
	// why the nodes that are not publishable were refused, by reason
	PublishRefusals PublishRefusals
	// The RMS ping residual over the terms of the final solve, with every node
	// at its derived position and with every node at its genesis. The job logs
	// both, so drift in k, o or the population is visible.
	ResidualKm        float64
	GenesisResidualKm float64
	// The terms the solve used, and the inputs it dropped: a node with an
	// invalid genesis or a repeated id; a term naming an unknown node or one
	// node twice, with a round trip that is negative or not finite, with no
	// samples, or repeating an ordered pair.
	TermCount        int
	DroppedNodeCount int
	DroppedTermCount int

	nodeIdIndexes map[string]int
}

// The result for a node id, or nil.
func (self *Result) Node(id string) *NodeResult {
	i, ok := self.nodeIdIndexes[id]
	if !ok {
		return nil
	}
	return &self.Nodes[i]
}

// Why a node's correction is not published (§5.4), the first gate it failed.
type PublishRefusal int

const (
	// published
	PublishRefusalNone PublishRefusal = 0
	// fewer than MinDerivePings co-signed pings
	PublishRefusalFewPings PublishRefusal = 1
	// pings to fewer than MinDerivePeers distinct peers
	PublishRefusalFewPeers PublishRefusal = 2
	// still moving by more than PublishMaxLastStepKm when the solve stopped
	PublishRefusalStillMoving PublishRefusal = 3
	// no lower RMS ping residual than at genesis
	PublishRefusalNoImprovement PublishRefusal = 4
)

// The count of nodes refused for each reason: the counts the derive job
// records with its run and the monitor reads, so a rising reason shows.
type PublishRefusals struct {
	FewPings      int `json:"few_pings"`
	FewPeers      int `json:"few_peers"`
	StillMoving   int `json:"still_moving"`
	NoImprovement int `json:"no_improvement"`
}

// Why the node's correction is refused publication, or PublishRefusalNone.
// The gates of §5.4 (D11), in order: at least MinDerivePings co-signed pings,
// to at least MinDerivePeers distinct peers; still by the end of the solve,
// having moved no more than PublishMaxLastStepKm in its last sweep, since a
// node still sliding when a stagnation stop or the sweep cap ended the solve
// is at no answer yet; and an RMS ping residual below its residual at
// genesis. There is deliberately no cap on the distance moved: the genesis
// term already makes every kilometre of shift compete with the measurements,
// and the containment terms price a region or country crossing on top.
func PublishRefusalOf(node *NodeResult, settings *Settings) PublishRefusal {
	switch {
	case node.PingCount < settings.MinDerivePings:
		return PublishRefusalFewPings
	case node.PeerCount < settings.MinDerivePeers:
		return PublishRefusalFewPeers
	case settings.PublishMaxLastStepKm < node.LastStepKm:
		return PublishRefusalStillMoving
	case !(node.ResidualKm < node.GenesisResidualKm):
		return PublishRefusalNoImprovement
	}
	return PublishRefusalNone
}

// Whether the node's correction passes every gate of §5.4 (PublishRefusalOf);
// otherwise the node keeps genesis.
func Publishable(node *NodeResult, settings *Settings) bool {
	return PublishRefusalOf(node, settings) == PublishRefusalNone
}

// A correction for every node, derived from its genesis and the terms
// (§5.2–§5.5). nodeIdRefusals may be nil; a nil containment
// leaves the containment terms out; nil settings are the defaults, and the
// settings are only read.
//
// The solve runs ReputationRounds times: iteratively reweighted least squares.
// The first weighs every source fully; each later one weighs every source by
// its scores against the geometry before it, starting from the corrections
// before it. Nothing about a source carries over from a previous run but its
// warm start, so a source that stops misbehaving returns to full weight as its
// bad pings age out of the window.
//
// The answer does not depend on the order of the nodes or the terms -- the
// order the derive job read them in, or which of its cursors read them: the
// nodes are taken in id order and the terms in (source id, target id) order,
// the repeats of a pair resolved by content, before anything is summed, and
// every sum after that is in that fixed order.
func Solve(nodes []Node, terms []Term, nodeIdRefusals map[string]Refusals, containment Containment, settings *Settings) *Result {
	if settings == nil {
		settings = DefaultSettings()
	}
	problem := newProblem(nodes, terms, nodeIdRefusals, containment, settings)
	rounds := max(1, settings.ReputationRounds)
	sweeps := make([]int, 0, rounds)
	stop := solveStopSweepCap
	for round := 0; round < rounds; round += 1 {
		if 0 < round {
			problem.reweight(problem.score())
		}
		roundSweeps, roundStop := problem.solveBlocks()
		sweeps = append(sweeps, roundSweeps)
		stop = roundStop
	}
	return problem.result(sweeps, stop)
}

// The result of the final solve: every node's own result and publish
// refusal, the network's residuals, and the scoring behind the final weights.
func (self *problem) result(sweeps []int, stop solveStop) *Result {
	result := &Result{
		Nodes:            make([]NodeResult, len(self.resultOrder)),
		Excluded:         []string{},
		Sweeps:           sweeps,
		Converged:        stop != solveStopSweepCap,
		Stagnated:        stop == solveStopStagnation,
		TermCount:        len(self.terms),
		DroppedNodeCount: self.droppedNodeCount,
		DroppedTermCount: self.droppedTermCount,
		nodeIdIndexes:    make(map[string]int, len(self.resultOrder)),
	}

	// the network's residuals over the terms of the final solve, each term
	// counted once, from its source
	used := func(term *problemTerm) bool {
		return !self.excluded[term.source]
	}
	usedCount := self.sumNodes(func(i int) float64 {
		var count float64
		for k := self.sourceStarts[i]; k < self.sourceStarts[i+1]; k += 1 {
			if used(&self.terms[k]) {
				count += 1
			}
		}
		return count
	})
	if 0 < usedCount {
		sumSquares := self.sumNodes(func(i int) float64 {
			var partial float64
			for k := self.sourceStarts[i]; k < self.sourceStarts[i+1]; k += 1 {
				term := &self.terms[k]
				if used(term) {
					residual := surfaceKm(self.positions[i], self.positions[term.target]) - self.impliedKm(term)
					partial += residual * residual
				}
			}
			return partial
		})
		genesisSumSquares := self.sumNodes(func(i int) float64 {
			var partial float64
			for k := self.sourceStarts[i]; k < self.sourceStarts[i+1]; k += 1 {
				term := &self.terms[k]
				if used(term) {
					residual := surfaceKm(self.frames[i].origin, self.frames[term.target].origin) - self.impliedKm(term)
					partial += residual * residual
				}
			}
			return partial
		})
		result.ResidualKm = math.Sqrt(sumSquares / usedCount)
		result.GenesisResidualKm = math.Sqrt(genesisSumSquares / usedCount)
	}

	// Each node's own result, in parallel: it reads the shared state and
	// writes only its own entry.
	self.parallel(self.chunks(len(self.resultOrder)), func(c int) {
		start, end := self.chunkRange(c, len(self.resultOrder))
		for r := start; r < end; r += 1 {
			i := int(self.resultOrder[r])
			result.Nodes[r] = self.nodeResult(i, used)
		}
	})
	for r, i := range self.resultOrder {
		result.nodeIdIndexes[self.nodes[i].Id] = r
		if self.excluded[i] {
			result.Excluded = append(result.Excluded, self.nodes[i].Id)
		}
		switch PublishRefusalOf(&result.Nodes[r], self.settings) {
		case PublishRefusalFewPings:
			result.PublishRefusals.FewPings += 1
		case PublishRefusalFewPeers:
			result.PublishRefusals.FewPeers += 1
		case PublishRefusalStillMoving:
			result.PublishRefusals.StillMoving += 1
		case PublishRefusalNoImprovement:
			result.PublishRefusals.NoImprovement += 1
		}
	}
	slices.Sort(result.Excluded)
	if self.scores != nil {
		result.Population = self.scores.population()
	}
	return result
}

// One node's result, over the terms that carried weight in the final solve.
func (self *problem) nodeResult(i int, used func(term *problemTerm) bool) NodeResult {
	node := &self.nodes[i]
	nodeResult := NodeResult{
		Id:              node.Id,
		Genesis:         node.Genesis,
		Position:        self.positions[i].latLon(),
		Correction:      self.corrections[i],
		LastStepKm:      self.lastStepLengths[i],
		Q:               self.q[i],
		Excluded:        self.excluded[i],
		SourceTermCount: int(self.sourceStarts[i+1] - self.sourceStarts[i]),
	}
	var sumSquares float64
	var genesisSumSquares float64
	termCount := 0
	measure := func(term *problemTerm) {
		other := term.other(i)
		impliedKm := self.impliedKm(term)
		residual := surfaceKm(self.positions[i], self.positions[other]) - impliedKm
		genesisResidual := surfaceKm(self.frames[i].origin, self.positions[other]) - impliedKm
		sumSquares += residual * residual
		genesisSumSquares += genesisResidual * genesisResidual
		termCount += 1
		nodeResult.PingCount += int(term.samples)
	}
	// Its own terms are ordered by target and the terms toward it by source,
	// so the distinct peers are the union of two sorted lists, counted as they
	// are merged.
	own := self.terms[self.sourceStarts[i]:self.sourceStarts[i+1]]
	toward := self.incoming[self.incomingStarts[i]:self.incomingStarts[i+1]]
	a, b := 0, 0
	for a < len(own) || b < len(toward) {
		var ownPeer, towardPeer uint32 = math.MaxUint32, math.MaxUint32
		for a < len(own) && !used(&own[a]) {
			a += 1
		}
		for b < len(toward) && !used(&self.terms[toward[b]]) {
			b += 1
		}
		if a < len(own) {
			ownPeer = own[a].target
		}
		if b < len(toward) {
			towardPeer = self.terms[toward[b]].source
		}
		if ownPeer == math.MaxUint32 && towardPeer == math.MaxUint32 {
			break
		}
		nodeResult.PeerCount += 1
		if ownPeer <= towardPeer {
			measure(&own[a])
			a += 1
		}
		if towardPeer <= ownPeer {
			measure(&self.terms[toward[b]])
			b += 1
		}
	}
	if 0 < termCount {
		nodeResult.ResidualKm = math.Sqrt(sumSquares / float64(termCount))
		nodeResult.GenesisResidualKm = math.Sqrt(genesisSumSquares / float64(termCount))
	}
	if self.scores != nil {
		self.scores.fill(i, &nodeResult)
	}
	return nodeResult
}
