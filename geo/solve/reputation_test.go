// Tests of source reputation (§5.5): the five statistics and their z-scores,
// the one-sided weights, exclusion, the spread floors, two-sided refusals,
// and coverage against the expected sample.

package solve

import (
	"fmt"
	"math"
	mathrand "math/rand"
	"testing"
)

// A problem with its nodes at their genesis, scored against that geometry
// without solving, so every residual is exactly what the round trips were
// built to leave.
func scoredProblem(nodes []Node, terms []Term, nodeIdRefusals map[string]Refusals, settings *Settings) (*problem, *scoreSet) {
	problem := newProblem(nodes, terms, nodeIdRefusals, nil, settings)
	return problem, problem.score()
}

// A term whose residual d(p_s, p_t) − d̂ is residualKm with both nodes at
// their genesis.
func termWithResidual(settings *Settings, nodeIdNodes map[string]Node, source string, target string, residualKm float64) Term {
	distanceKm := DistanceKm(nodeIdNodes[source].Genesis, nodeIdNodes[target].Genesis)
	return Term{Source: source, Target: target, RttMs: rttFor(settings, distanceKm-residualKm), Samples: 1}
}

// Four nodes whose statistics were worked by hand, scored. On the equator a degree apart:
//
//	a: a→b residual −10, a→c +20    scatter √250, bias 5, coverage 2/3
//	b: b→a, b→c, b→d all 0          scatter 0, bias 0, coverage 1
//	c: c→d residual −30             scatter 30, bias −30, coverage 1/3
//	d: no terms of its own
//
// with pinger refusal rates a 0.1, b 0, c 0.5 and target refusal rates a 0,
// b 0, d 0.5. Every node is a target, so each source had 3 peers available.
func TestReputationStatistics(t *testing.T) {
	settings := DefaultSettings()
	nodes := []Node{
		{Id: "a", Genesis: onEquator(0 * equatorKmPerDegree), RadiusKm: 5},
		{Id: "b", Genesis: onEquator(1 * equatorKmPerDegree), RadiusKm: 5},
		{Id: "c", Genesis: onEquator(2 * equatorKmPerDegree), RadiusKm: 5},
		{Id: "d", Genesis: onEquator(3 * equatorKmPerDegree), RadiusKm: 5},
	}
	nodeIdNodes := map[string]Node{}
	for _, node := range nodes {
		nodeIdNodes[node.Id] = node
	}
	terms := []Term{
		termWithResidual(settings, nodeIdNodes, "a", "b", -10),
		termWithResidual(settings, nodeIdNodes, "a", "c", 20),
		termWithResidual(settings, nodeIdNodes, "b", "a", 0),
		termWithResidual(settings, nodeIdNodes, "b", "c", 0),
		termWithResidual(settings, nodeIdNodes, "b", "d", 0),
		termWithResidual(settings, nodeIdNodes, "c", "d", -30),
	}
	nodeIdRefusals := map[string]Refusals{
		"a": {AsPinger: 1, PingsAsPinger: 10, AsTarget: 0, PingsAsTarget: 5},
		"b": {AsPinger: 0, PingsAsPinger: 10, AsTarget: 0, PingsAsTarget: 10},
		"c": {AsPinger: 5, PingsAsPinger: 10},
		"d": {AsTarget: 2, PingsAsTarget: 4},
	}
	problem, scores := scoredProblem(nodes, terms, nodeIdRefusals, settings)
	nodeIdIndexes := problem.nodeIdIndexes

	for _, want := range []struct {
		statistic int
		node      string
		value     float64
		z         float64
	}{
		{statistic: statisticScatter, node: "a", value: math.Sqrt(250), z: 0.044144862},
		{statistic: statisticScatter, node: "b", value: 0, z: -1.246220471},
		{statistic: statisticScatter, node: "c", value: 30, z: 1.202075609},
		{statistic: statisticBias, node: "a", value: 5, z: 0.862662186},
		{statistic: statisticBias, node: "b", value: 0, z: 0.539163866},
		{statistic: statisticBias, node: "c", value: -30, z: -1.401826052},
		{statistic: statisticCoverage, node: "a", value: 2.0 / 3, z: 0},
		{statistic: statisticCoverage, node: "b", value: 1, z: 1.224744871},
		{statistic: statisticCoverage, node: "c", value: 1.0 / 3, z: -1.224744871},
		{statistic: statisticRefusalAsPinger, node: "a", value: 0.1, z: -0.462910050},
		{statistic: statisticRefusalAsPinger, node: "b", value: 0, z: -0.925820100},
		{statistic: statisticRefusalAsPinger, node: "c", value: 0.5, z: 1.388730150},
		{statistic: statisticRefusalAsTarget, node: "a", value: 0, z: -0.707106781},
		{statistic: statisticRefusalAsTarget, node: "b", value: 0, z: -0.707106781},
		{statistic: statisticRefusalAsTarget, node: "d", value: 0.5, z: 1.414213562},
	} {
		i := nodeIdIndexes[want.node]
		if !scores.has[want.statistic][i] {
			t.Fatalf("statistic %d of %s is undefined", want.statistic, want.node)
		}
		assertNear(t, "value", scores.values[want.statistic][i], want.value, 1e-6)
		assertNear(t, "z", scores.z[want.statistic][i], want.z, 1e-6)
	}
	// d has no terms of its own and c was never a target of a refusal count
	for _, undefined := range []struct {
		statistic int
		node      string
	}{
		{statistic: statisticScatter, node: "d"},
		{statistic: statisticBias, node: "d"},
		{statistic: statisticCoverage, node: "d"},
		{statistic: statisticRefusalAsPinger, node: "d"},
		{statistic: statisticRefusalAsTarget, node: "c"},
	} {
		if scores.has[undefined.statistic][nodeIdIndexes[undefined.node]] || scores.z[undefined.statistic][nodeIdIndexes[undefined.node]] != 0 {
			t.Fatalf("statistic %d of %s is defined", undefined.statistic, undefined.node)
		}
	}
	population := scores.population()
	if population.ScatterKm.Count != 3 || population.RefusalRateAsTarget.Count != 3 {
		t.Fatalf("population counts %+v", population)
	}
	assertNear(t, "scatter mean", population.ScatterKm.Mean, 15.270462767, 1e-6)
	assertNear(t, "scatter spread", population.ScatterKm.StdDev, 12.253419940, 1e-6)
	assertNear(t, "bias spread", population.BiasKm.StdDev, 15.456030826, 1e-6)

	// Only the side that marks a bad source counts: b's scatter below the
	// mean and its coverage above it are not held against it, so its weight
	// comes from its bias alone; c's comes from its worst, its bias.
	for _, want := range []struct {
		node string
		q    float64
	}{
		{node: "a", q: 0.573333333},
		{node: "b", q: 0.774774775},
		{node: "c", q: 0.337254902},
		{node: "d", q: 0.333333333},
	} {
		assertNear(t, "q of "+want.node, scores.q[nodeIdIndexes[want.node]], want.q, 1e-6)
		if scores.excluded[nodeIdIndexes[want.node]] {
			t.Fatalf("%s excluded at ExcludeZ %v", want.node, settings.ExcludeZ)
		}
	}

	// Exclusion is strictly past ExcludeZ: at 1.4 c (bias 1.402) and d
	// (target refusals 1.414) are out; at 1.41 only d.
	for _, test := range []struct {
		excludeZ float64
		excluded []string
	}{
		{excludeZ: 1.4, excluded: []string{"c", "d"}},
		{excludeZ: 1.41, excluded: []string{"d"}},
		{excludeZ: 1.415, excluded: []string{}},
	} {
		excludeSettings := DefaultSettings()
		excludeSettings.ExcludeZ = test.excludeZ
		problem, scores := scoredProblem(nodes, terms, nodeIdRefusals, excludeSettings)
		got := []string{}
		for i, node := range problem.nodes {
			if scores.excluded[i] {
				got = append(got, node.Id)
			}
		}
		if len(got) != len(test.excluded) || (0 < len(got) && got[0] != test.excluded[0]) || (1 < len(got) && got[1] != test.excluded[1]) {
			t.Fatalf("ExcludeZ %v: excluded %v, want %v", test.excludeZ, got, test.excluded)
		}
		// an excluded source's terms carry no weight in the next solve
		problem.reweight(scores)
		for k := range problem.terms {
			term := &problem.terms[k]
			if problem.excluded[term.source] != (term.weight == 0) {
				t.Fatalf("term %d from %s: weight %v", k, problem.nodes[term.source].Id, term.weight)
			}
			if !problem.excluded[term.source] && term.weight != float32(problem.q[term.source]*problem.baseWeight(term)) {
				t.Fatalf("term %d reweighted to %v, want %v", k, term.weight, float32(problem.q[term.source]*problem.baseWeight(term)))
			}
		}
	}
}

// Coverage lowers a source's weight but never excludes it, however far out.
func TestReputationCoverageNeverExcludes(t *testing.T) {
	settings := DefaultSettings()
	settings.ExcludeZ = 2
	nodes := []Node{}
	for i := 0; i < 6; i += 1 {
		nodes = append(nodes, Node{Id: syntheticNodeId(i), Genesis: onEquator(float64(i) * 150), RadiusKm: 5})
	}
	terms := []Term{}
	for i := range nodes {
		for j := range nodes {
			// node-05 measures only node-00; everyone else measures everyone
			if i == j || (i == 5 && j != 0) {
				continue
			}
			terms = append(terms, Term{Source: nodes[i].Id, Target: nodes[j].Id, RttMs: rttFor(settings, DistanceKm(nodes[i].Genesis, nodes[j].Genesis)), Samples: 1})
		}
	}
	problem, scores := scoredProblem(nodes, terms, nil, settings)
	subset := int(problem.nodeIdIndexes["node-05"])
	// one source against five that measured everything: −√5 whatever it measured
	assertNear(t, "coverage z", scores.z[statisticCoverage][subset], -math.Sqrt(5), 1e-9)
	assertNear(t, "q", scores.q[subset], 1.0/6, 1e-9)
	if scores.excluded[subset] {
		t.Fatalf("coverage z %v past ExcludeZ %v excluded the source", scores.z[statisticCoverage][subset], settings.ExcludeZ)
	}
	for i := range problem.nodes {
		if i != subset && scores.q[i] != 1 {
			t.Fatalf("%s: q %v", problem.nodes[i].Id, scores.q[i])
		}
	}
}

// A spread floor keeps a population with no real spread from making a tiny
// difference many sigma: one refusal in a hundred, among pingers never
// refused, is not four sigma of evidence.
func TestReputationSpreadFloor(t *testing.T) {
	nodes := []Node{
		{Id: "x", Genesis: onEquator(0), RadiusKm: 5},
		{Id: "y", Genesis: onEquator(100), RadiusKm: 5},
		{Id: "z", Genesis: onEquator(200), RadiusKm: 5},
	}
	nodeIdRefusals := map[string]Refusals{
		"x": {PingsAsPinger: 100},
		"y": {PingsAsPinger: 100},
		"z": {AsPinger: 1, PingsAsPinger: 100},
	}
	settings := DefaultSettings()
	problem, scores := scoredProblem(nodes, nil, nodeIdRefusals, settings)
	z := int(problem.nodeIdIndexes["z"])
	assertNear(t, "z with the floor", scores.z[statisticRefusalAsPinger][z], (0.01-0.01/3)/settings.RefusalFloor, 1e-9)
	if scores.q[z] < 0.98 {
		t.Fatalf("q with the floor %v", scores.q[z])
	}

	settings.RefusalFloor = 0
	problem, scores = scoredProblem(nodes, nil, nodeIdRefusals, settings)
	assertNear(t, "z without the floor", scores.z[statisticRefusalAsPinger][z], math.Sqrt(2), 1e-9)
	assertNear(t, "q without the floor", scores.q[z], 1.0/3, 1e-9)

	// with no spread at all and no floor, nobody is out
	settings.ScatterFloorKm = 0
	settings.BiasFloorKm = 0
	problem, scores = scoredProblem(nodes, nil, map[string]Refusals{"x": {PingsAsPinger: 10}, "y": {PingsAsPinger: 10}}, settings)
	for i := range problem.nodes {
		if scores.q[i] != 1 {
			t.Fatalf("%s: q %v", problem.nodes[i].Id, scores.q[i])
		}
	}
}

// Refusals are read two-sided and against the population (§5.5): a pinger the
// targets refuse, while they co-sign everyone else, loses its weight, and
// the targets that refused it, each refusing once like all the others, do not.
func TestReputationRefusalsTwoSided(t *testing.T) {
	settings := DefaultSettings()
	nodes := []Node{}
	nodeIdRefusals := map[string]Refusals{}
	for i := 0; i < 10; i += 1 {
		id := syntheticNodeId(i)
		nodes = append(nodes, Node{Id: id, Genesis: onEquator(float64(i) * 100), RadiusKm: 5})
		if i == 0 {
			// refused once by each of the other nine
			nodeIdRefusals[id] = Refusals{AsPinger: 9, PingsAsPinger: 20, AsTarget: 0, PingsAsTarget: 20}
		} else {
			nodeIdRefusals[id] = Refusals{AsPinger: 0, PingsAsPinger: 20, AsTarget: 1, PingsAsTarget: 20}
		}
	}
	problem, scores := scoredProblem(nodes, nil, nodeIdRefusals, settings)
	bad := int(problem.nodeIdIndexes[syntheticNodeId(0)])
	assertNear(t, "bad pinger z", scores.z[statisticRefusalAsPinger][bad], 3, 1e-9)
	assertNear(t, "bad pinger q", scores.q[bad], 0.1, 1e-9)
	for i := range problem.nodes {
		if i == bad {
			continue
		}
		if scores.q[i] < 0.99 {
			t.Fatalf("%s refused the bad pinger once and has q %v", problem.nodes[i].Id, scores.q[i])
		}
	}
}

// The result reports the scoring behind the final solve: q follows from the
// one-sided z-scores, the population is the nodes' own statistics, and the
// excluded list is the excluded flags.
func TestReputationResultIsConsistent(t *testing.T) {
	settings := DefaultSettings()
	network := newSyntheticNetwork(5, settings, true)
	aggregator := NewAggregator(settings)
	network.pings(aggregator, 16, 0.5, nil, nil)
	nodeIdRefusals := map[string]Refusals{
		syntheticNodeId(3): {AsPinger: 4, PingsAsPinger: 40, AsTarget: 1, PingsAsTarget: 40},
		syntheticNodeId(9): {AsPinger: 0, PingsAsPinger: 40, AsTarget: 6, PingsAsTarget: 40},
	}
	result := Solve(network.nodes, aggregator.Terms(), nodeIdRefusals, nil, settings)

	var scatterSum float64
	scatterCount := 0
	excluded := []string{}
	for _, node := range result.Nodes {
		zMax := max(max(0, node.ScatterZ), math.Abs(node.BiasZ), max(0, -node.CoverageZ), max(0, node.RefusalAsPingerZ), max(0, node.RefusalAsTargetZ))
		assertNear(t, node.Id+" q", node.Q, min(1, max(settings.MinQ, 1/(1+zMax*zMax))), 1e-12)
		if 0 < node.SourceTermCount {
			scatterSum += node.ScatterKm
			scatterCount += 1
		}
		if node.Excluded {
			excluded = append(excluded, node.Id)
		}
	}
	if result.Population.ScatterKm.Count != scatterCount {
		t.Fatalf("population count %d, %d nodes with terms", result.Population.ScatterKm.Count, scatterCount)
	}
	assertNear(t, "population scatter mean", result.Population.ScatterKm.Mean, scatterSum/float64(scatterCount), 1e-9)
	if len(excluded) != len(result.Excluded) {
		t.Fatalf("excluded %v, listed %v", excluded, result.Excluded)
	}
	assertNear(t, "refusal rate", result.Node(syntheticNodeId(9)).RefusalRateAsTarget, 6.0/40, 1e-12)
	if result.Population.RefusalRateAsPinger.Count != 2 {
		t.Fatalf("refusal population %+v", result.Population.RefusalRateAsPinger)
	}

	// with one round there is no scoring, and every source weighs fully
	oneRound := DefaultSettings()
	oneRound.ReputationRounds = 1
	result = Solve(network.nodes, aggregator.Terms(), nodeIdRefusals, nil, oneRound)
	for _, node := range result.Nodes {
		if node.Q != 1 || node.Excluded || node.ScatterZ != 0 || node.RefusalAsTargetZ != 0 {
			t.Fatalf("%s scored with one round: %+v", node.Id, node)
		}
	}
	if len(result.Sweeps) != 1 || result.Population.ScatterKm.Count != 0 {
		t.Fatalf("one round: sweeps %v population %+v", result.Sweeps, result.Population)
	}
}

// A population that samples its peers by design (D26):
// extenders each measure `extenderPeers` of the other extenders and providers
// each measure `providerPeers` extenders, from more available than either.
// Every genesis is the truth and every round trip is exact, so the residuals
// are zero and only coverage can set a weight. sampled, when not nil, says how
// many peers a source actually measured instead of its expected sample.
func expectedPeersNetwork(
	extenders int,
	providers int,
	extenderPeers int,
	providerPeers int,
	expected bool,
	sampled func(i int) int,
	settings *Settings,
) ([]Node, []Term) {
	random := mathrand.New(mathrand.NewSource(int64(extenders + providers)))
	nodes := make([]Node, 0, extenders+providers)
	for i := 0; i < extenders+providers; i += 1 {
		expectedPeers := extenderPeers
		if extenders <= i {
			expectedPeers = providerPeers
		}
		node := Node{
			Id: fmt.Sprintf("n%07d", i),
			Genesis: LatLon{
				Latitude:  syntheticSouth + random.Float64()*(syntheticNorth-syntheticSouth),
				Longitude: syntheticWest + random.Float64()*(syntheticEast-syntheticWest),
			},
			RadiusKm: 100,
		}
		if expected {
			node.ExpectedPeers = expectedPeers
		}
		nodes = append(nodes, node)
	}
	terms := []Term{}
	for i := range nodes {
		peers := extenderPeers
		if extenders <= i {
			peers = providerPeers
		}
		if sampled != nil {
			peers = sampled(i)
		}
		// a sample of distinct extenders other than itself
		for _, j := range random.Perm(extenders)[:peers+1] {
			if j == i || peers <= 0 {
				continue
			}
			peers -= 1
			terms = append(terms, Term{
				Source:  nodes[i].Id,
				Target:  nodes[j].Id,
				RttMs:   rttFor(settings, DistanceKm(nodes[i].Genesis, nodes[j].Genesis)),
				Samples: 1,
			})
		}
	}
	return nodes, terms
}

// Coverage measured against the sample a source was expected to take (D26). Extenders expect 64 of 199
// other extenders and providers 16 of 200.
func TestReputationCoverageAgainstExpectedPeers(t *testing.T) {
	const extenders = 200
	const providers = 400
	settings := DefaultSettings()

	// every source measured exactly its expected sample: no coverage penalty
	nodes, terms := expectedPeersNetwork(extenders, providers, 64, 16, true, nil, settings)
	problem, scores := scoredProblem(nodes, terms, nil, settings)
	for i := range problem.nodes {
		if scores.values[statisticCoverage][i] != 1 || scores.z[statisticCoverage][i] != 0 || scores.q[i] != 1 {
			t.Fatalf("%s: coverage %v, z %v, q %v", problem.nodes[i].Id, scores.values[statisticCoverage][i], scores.z[statisticCoverage][i], scores.q[i])
		}
	}

	// A source that measured a quarter of its sample is the one marked down:
	// an extender measuring 16 of its 64, and a provider 4 of its 16.
	shortExtender := 17
	shortProvider := extenders + 23
	short := func(i int) int {
		switch i {
		case shortExtender:
			return 16
		case shortProvider:
			return 4
		case extenders:
			// the more peers, the full coverage, never above it
			return 32
		}
		if i < extenders {
			return 64
		}
		return 16
	}
	nodes, terms = expectedPeersNetwork(extenders, providers, 64, 16, true, short, settings)
	problem, scores = scoredProblem(nodes, terms, nil, settings)
	for i := range problem.nodes {
		coverage := scores.values[statisticCoverage][i]
		switch problem.nodes[i].Id {
		case nodes[shortExtender].Id, nodes[shortProvider].Id:
			assertNear(t, "short coverage", coverage, 0.25, 1e-12)
			if !(scores.q[i] < 0.2) {
				t.Fatalf("%s measured a quarter of its sample and has q %v", problem.nodes[i].Id, scores.q[i])
			}
			t.Logf("%s: coverage %.2f, z %.2f, q %.4f", problem.nodes[i].Id, coverage, scores.z[statisticCoverage][i], scores.q[i])
		default:
			if coverage != 1 || scores.q[i] != 1 {
				t.Fatalf("%s: coverage %v, q %v", problem.nodes[i].Id, coverage, scores.q[i])
			}
		}
	}

	// With no expected sample, coverage is today's share of the peers
	// available, exactly, and it marks every provider down for measuring
	// the 16 its design asks of it.
	nodes, terms = expectedPeersNetwork(extenders, providers, 64, 16, false, nil, settings)
	problem, scores = scoredProblem(nodes, terms, nil, settings)
	lowestProviderQ := 1.0
	for i := range problem.nodes {
		want := 64.0 / (extenders - 1)
		if extenders <= i {
			want = 16.0 / extenders
			lowestProviderQ = min(lowestProviderQ, scores.q[i])
		}
		if scores.values[statisticCoverage][i] != want {
			t.Fatalf("%s: coverage %v, want %v", problem.nodes[i].Id, scores.values[statisticCoverage][i], want)
		}
	}
	t.Logf("without expected samples every provider's q is %.4f", lowestProviderQ)
	if !(lowestProviderQ < 1) {
		t.Fatal("without expected samples the providers were not marked down, so the test shows nothing")
	}
}
