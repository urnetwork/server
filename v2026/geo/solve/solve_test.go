// Tests of the solver on problems with closed forms -- two nodes, a node
// against a border, the robust losses -- and of its handling of input, warm
// starts, order, components and the publish gate.

package solve

import (
	"math"
	mathrand "math/rand"
	"reflect"
	"slices"
	"testing"
)

// km per degree of longitude on the equator, where every test below lays its
// nodes: moving along the equator is moving east, and distances along it are
// differences of km
const equatorKmPerDegree = math.Pi / 180 * EarthRadiusKm

// The point km east of the origin along the equator.
func onEquator(km float64) LatLon {
	return LatLon{Latitude: 0, Longitude: km / equatorKmPerDegree}
}

// The round trip whose implied distance is distanceKm.
func rttFor(settings *Settings, distanceKm float64) float64 {
	return distanceKm/settings.KmPerMs + settings.OverheadMs
}

// Settings that solve to a tight tolerance and leave reputation out, for the
// tests with a closed-form answer.
func preciseSettings() *Settings {
	settings := DefaultSettings()
	settings.ReputationRounds = 1
	settings.MaxIterations = 2000
	settings.MinStepKm = 1e-10
	return settings
}

// Fails the test unless got is within tolerance of want.
func assertNear(t *testing.T, name string, got float64, want float64, tolerance float64) {
	t.Helper()
	if !(math.Abs(got-want) <= tolerance) {
		t.Fatalf("%s = %.9f, want %.9f (± %g)", name, got, want, tolerance)
	}
}

// The block solver on the smallest problem with a closed form. A and B lie on the equator D0 apart, with genesis weights w_A and w_B,
// and two terms, A→B and B→A, imply d1 and d2 with weights v1 and v2. The two
// terms act as one of weight v = v1 + v2 implying the weighted mean d̄, and
// setting the gradient of
//
//	w_A·a² + w_B·(b − D0)² + v·(b − a − d̄)²
//
// to zero gives, with e = D − d̄ the residual at the solution,
//
//	e = (D0 − d̄) / (1 + v·(1/w_A + 1/w_B)),  a = v·e/w_A,  b = D0 − v·e/w_B.
func TestSolveTwoNodes(t *testing.T) {
	settings := preciseSettings()
	const d0Km = 500.0
	const d1Km = 400.0
	const d2Km = 420.0
	const aRadiusKm = 50.0
	const bRadiusKm = 100.0
	nodes := []Node{
		{Id: "a", Genesis: onEquator(0), RadiusKm: aRadiusKm},
		{Id: "b", Genesis: onEquator(d0Km), RadiusKm: bRadiusKm},
	}
	terms := []Term{
		{Source: "a", Target: "b", RttMs: rttFor(settings, d1Km), Samples: 4},
		{Source: "b", Target: "a", RttMs: rttFor(settings, d2Km), Samples: 2},
	}
	result := Solve(nodes, terms, nil, nil, settings)

	wA := 1 / (aRadiusKm * aRadiusKm)
	wB := 1 / (bRadiusKm * bRadiusKm)
	// a term's weight as the solve keeps it, in a float32
	v1 := float64(float32(4 / (d1Km * d1Km)))
	v2 := float64(float32(2 / (d2Km * d2Km)))
	v := v1 + v2
	meanKm := (v1*d1Km + v2*d2Km) / v
	e := (d0Km - meanKm) / (1 + v*(1/wA+1/wB))
	aWantKm := v * e / wA
	bWantKm := d0Km - v*e/wB
	t.Logf("closed form: a moves %.6f km, b moves %.6f km, distance %.6f km", aWantKm, bWantKm-d0Km, bWantKm-aWantKm)

	a := result.Node("a")
	b := result.Node("b")
	assertNear(t, "a east", a.Correction.EastKm, aWantKm, 1e-6)
	assertNear(t, "b east", b.Correction.EastKm, bWantKm-d0Km, 1e-6)
	assertNear(t, "a north", a.Correction.NorthKm, 0, 1e-6)
	assertNear(t, "b north", b.Correction.NorthKm, 0, 1e-6)
	assertNear(t, "distance", DistanceKm(a.Position, b.Position), meanKm+e, 1e-6)
	if !result.Converged {
		t.Fatalf("not converged after %v sweeps", result.Sweeps)
	}

	// what the result says about the terms
	if a.PingCount != 6 || a.PeerCount != 1 || b.PingCount != 6 || b.PeerCount != 1 || a.SourceTermCount != 1 {
		t.Fatalf("counts: a %d pings %d peers, b %d pings %d peers", a.PingCount, a.PeerCount, b.PingCount, b.PeerCount)
	}
	// the correction explains the pings better than genesis did, but one peer
	// is short of the default gate
	if !(a.ResidualKm < a.GenesisResidualKm) || Publishable(a, DefaultSettings()) {
		t.Fatalf("a: residual %v at genesis %v, publishable %v", a.ResidualKm, a.GenesisResidualKm, Publishable(a, DefaultSettings()))
	}
}

// A region border and a country border, each a meridian
// east of the origin: a hinge is how far east of its line the point is, in km
// along the equator. Near the equator that is exactly the margin the place
// list would report for two cities placed symmetrically about the line.
type fakeContainment struct {
	regionLineKm  float64
	countryLineKm float64
}

// Implements Containment.
func (self *fakeContainment) RegionHinge(p LatLon, regionKey string) float64 {
	if regionKey != "west" {
		return 0
	}
	return max(0, p.Longitude*equatorKmPerDegree-self.regionLineKm)
}

// Implements Containment.
func (self *fakeContainment) CountryHinge(p LatLon, countryKey string) float64 {
	if countryKey != "west" {
		return 0
	}
	return max(0, p.Longitude*equatorKmPerDegree-self.countryLineKm)
}

// A crossing priced by the hinges. One node with genesis
// at the origin is pulled east by its pings toward a pinned anchor; with s the
// shift its pings alone ask for, the solution x minimizes
//
//	w·x² + v·(x − s)² + λ_region·(x − b_region)₊² + λ_country·(x − b_country)₊²
//
// so on each side of each line it is a ratio of linear terms.
func TestSolveContainment(t *testing.T) {
	settings := preciseSettings()
	// an exact genesis, which never moves
	settings.GenesisMinRadiusKm = 0
	const anchorKm = 500.0
	const impliedKm = 200.0
	const shiftKm = anchorKm - impliedKm
	const radiusKm = 100.0
	const samples = 10
	w := 1 / (radiusKm * radiusKm)
	// the term's weight as the solve keeps it, in a float32
	v := float64(float32(samples / (impliedKm * impliedKm)))

	for _, test := range []struct {
		name          string
		regionLineKm  float64
		countryLineKm float64
		wantKm        float64
	}{
		{
			name:          "no line in the way",
			regionLineKm:  1000,
			countryLineKm: 1000,
			wantKm:        v * shiftKm / (w + v),
		},
		{
			name:          "over the region line, short of the country line",
			regionLineKm:  100,
			countryLineKm: 150,
			wantKm:        (v*shiftKm + settings.LambdaRegion*100) / (w + v + settings.LambdaRegion),
		},
		{
			name:          "over both",
			regionLineKm:  100,
			countryLineKm: 102,
			wantKm:        (v*shiftKm + settings.LambdaRegion*100 + settings.LambdaCountry*102) / (w + v + settings.LambdaRegion + settings.LambdaCountry),
		},
	} {
		nodes := []Node{
			{Id: "node", Genesis: onEquator(0), RadiusKm: radiusKm, RegionKey: "west", CountryKey: "west"},
			{Id: "anchor", Genesis: onEquator(anchorKm), RadiusKm: 0},
		}
		terms := []Term{
			{Source: "node", Target: "anchor", RttMs: rttFor(settings, impliedKm), Samples: samples},
		}
		containment := &fakeContainment{regionLineKm: test.regionLineKm, countryLineKm: test.countryLineKm}
		result := Solve(nodes, terms, nil, containment, settings)
		node := result.Node("node")
		t.Logf("%s: moved %.6f km east in %v sweeps (closed form %.6f km)", test.name, node.Correction.EastKm, result.Sweeps, test.wantKm)
		assertNear(t, test.name, node.Correction.EastKm, test.wantKm, 1e-6)
		assertNear(t, test.name+" north", node.Correction.NorthKm, 0, 1e-6)
		if anchor := result.Node("anchor"); anchor.Correction.LengthKm() != 0 {
			t.Fatalf("%s: the pinned anchor moved %+v", test.name, anchor.Correction)
		}
	}

	// A nil containment, or a zero weight, leaves the terms out.
	nodes := []Node{
		{Id: "node", Genesis: onEquator(0), RadiusKm: radiusKm, RegionKey: "west", CountryKey: "west"},
		{Id: "anchor", Genesis: onEquator(anchorKm), RadiusKm: 0},
	}
	terms := []Term{{Source: "node", Target: "anchor", RttMs: rttFor(settings, impliedKm), Samples: samples}}
	unbounded := v * shiftKm / (w + v)
	result := Solve(nodes, terms, nil, nil, settings)
	assertNear(t, "no containment", result.Node("node").Correction.EastKm, unbounded, 1e-6)
	zeroSettings := *settings
	zeroSettings.LambdaRegion = 0
	zeroSettings.LambdaCountry = 0
	result = Solve(nodes, terms, nil, &fakeContainment{regionLineKm: 0, countryLineKm: 0}, &zeroSettings)
	assertNear(t, "zero weights", result.Node("node").Correction.EastKm, unbounded, 1e-6)
}

// The refinements of D9 against their closed forms
// on one free node and a pinned anchor, with a round trip that is slack: longer
// than the genesis distance D0 allows, so the node moves away by x < d̂ − D0.
func TestSolveRobustTerms(t *testing.T) {
	const d0Km = 300.0
	const impliedKm = 500.0
	const radiusKm = 100.0
	const samples = 8
	w := 1 / (radiusKm * radiusKm)
	// the term's weight as the solve keeps it, in a float32
	v := float64(float32(samples / (impliedKm * impliedKm)))
	solveAway := func(settings *Settings) float64 {
		settings.GenesisMinRadiusKm = 0
		nodes := []Node{
			{Id: "anchor", Genesis: onEquator(0), RadiusKm: 0},
			{Id: "node", Genesis: onEquator(d0Km), RadiusKm: radiusKm},
		}
		terms := []Term{{Source: "node", Target: "anchor", RttMs: rttFor(settings, impliedKm), Samples: samples}}
		return Solve(nodes, terms, nil, nil, settings).Node("node").Correction.EastKm
	}

	// plain squares: w·x² + v·(D0 + x − d̂)²
	plainKm := v * (impliedKm - d0Km) / (w + v)
	assertNear(t, "plain", solveAway(preciseSettings()), plainKm, 1e-6)

	// Asymmetric: a slack residual weighs α·v.
	asymmetric := preciseSettings()
	asymmetric.Asymmetric = true
	alpha := asymmetric.AsymmetricSlackWeight
	asymmetricKm := alpha * v * (impliedKm - d0Km) / (w + alpha*v)
	assertNear(t, "asymmetric", solveAway(asymmetric), asymmetricKm, 1e-6)

	// Huber: past the threshold c the cost is v·d̂²·(2c|u| − c²) with
	// u = (D0 + x − d̂)/d̂, whose slope is constant, so the node stops where the
	// genesis slope 2·w·x meets it: x = v·d̂·c/w, while |u| is still past c.
	huber := preciseSettings()
	huber.Huber = true
	huber.HuberRelative = 0.1
	huberKm := v * impliedKm * huber.HuberRelative / w
	if relative := (impliedKm - d0Km - huberKm) / impliedKm; !(huber.HuberRelative < relative) {
		t.Fatalf("test geometry: the Huber solution is inside the threshold (%v)", relative)
	}
	assertNear(t, "huber", solveAway(huber), huberKm, 1e-6)
	t.Logf("moved away: plain %.4f km, asymmetric %.4f km, huber %.4f km", plainKm, asymmetricKm, huberKm)

	// the Huber cost is continuous at the threshold, and linear past it: a
	// 100 km implied distance is its own scale
	problem := newProblem(nil, nil, nil, nil, huber)
	below := problem.pingCost(100, 1, 10-1e-9)
	above := problem.pingCost(100, 1, 10+1e-9)
	assertNear(t, "huber continuity", above, below, 1e-6)
	assertNear(t, "huber slope", problem.pingCost(100, 1, 30)-problem.pingCost(100, 1, 20), 2*huber.HuberRelative*100*10, 1e-9)
}

// Invalid nodes and terms are dropped and counted, and a repeated pair keeps
// the same term whatever the order.
func TestSolveDropsInvalidInput(t *testing.T) {
	settings := DefaultSettings()
	nodes := []Node{
		{Id: "a", Genesis: onEquator(0), RadiusKm: 10},
		{Id: "b", Genesis: onEquator(100), RadiusKm: 10},
		// a repeated id keeps the first
		{Id: "a", Genesis: onEquator(9000), RadiusKm: 10},
		{Id: "nan", Genesis: LatLon{Latitude: math.NaN(), Longitude: 0}, RadiusKm: 10},
		{Id: "off", Genesis: LatLon{Latitude: 91, Longitude: 0}, RadiusKm: 10},
		{Id: "inf", Genesis: LatLon{Latitude: 0, Longitude: math.Inf(1)}, RadiusKm: 10},
	}
	terms := []Term{
		{Source: "a", Target: "b", RttMs: rttFor(settings, 90), Samples: 2},
		// a repeated pair keeps the most samples, whatever the order
		{Source: "a", Target: "b", RttMs: rttFor(settings, 80), Samples: 5},
		{Source: "a", Target: "b", RttMs: rttFor(settings, 70), Samples: 1},
		{Source: "b", Target: "a", RttMs: rttFor(settings, 95), Samples: 3},
		{Source: "a", Target: "unknown", RttMs: 5, Samples: 1},
		{Source: "a", Target: "a", RttMs: 5, Samples: 1},
		{Source: "a", Target: "nan", RttMs: 5, Samples: 1},
		{Source: "b", Target: "a", RttMs: -1, Samples: 1},
		{Source: "b", Target: "a", RttMs: math.NaN(), Samples: 1},
		{Source: "b", Target: "a", RttMs: math.Inf(1), Samples: 1},
		{Source: "b", Target: "a", RttMs: 5, Samples: 0},
	}
	result := Solve(nodes, terms, nil, nil, settings)
	if len(result.Nodes) != 2 || result.Nodes[0].Id != "a" || result.Nodes[1].Id != "b" {
		t.Fatalf("nodes %+v", result.Nodes)
	}
	if result.Node("a").Genesis != onEquator(0) {
		t.Fatalf("the repeated id replaced the first: %+v", result.Node("a").Genesis)
	}
	if result.DroppedNodeCount != 4 || result.TermCount != 2 || result.DroppedTermCount != 9 {
		t.Fatalf("dropped %d nodes, kept %d terms, dropped %d", result.DroppedNodeCount, result.TermCount, result.DroppedTermCount)
	}
	if result.Node("nan") != nil || result.Node("unknown") != nil {
		t.Fatal("a dropped node has a result")
	}
	// a's term toward b is the five-sample one
	if a := result.Node("a"); a.PingCount != 8 || a.PeerCount != 1 {
		t.Fatalf("a: %d pings %d peers", a.PingCount, a.PeerCount)
	}
	shuffled := slices.Clone(terms)
	slices.Reverse(shuffled)
	if reversed := Solve(nodes, shuffled, nil, nil, settings); !reflect.DeepEqual(reversed.Nodes, result.Nodes) {
		t.Fatal("the choice between repeated terms depends on their order")
	}
}

// A node with no terms is anchored by its genesis alone and does not move, and
// has nothing to publish.
func TestSolveNodeWithoutTerms(t *testing.T) {
	settings := DefaultSettings()
	nodes := []Node{
		{Id: "a", Genesis: LatLon{Latitude: 47.5, Longitude: 7.5}, RadiusKm: 1000},
		{Id: "b", Genesis: LatLon{Latitude: -12, Longitude: 130}, RadiusKm: 5},
	}
	result := Solve(nodes, nil, nil, nil, settings)
	for _, node := range result.Nodes {
		if node.Correction != (Offset{}) || node.PingCount != 0 || node.PeerCount != 0 || Publishable(&node, settings) {
			t.Fatalf("%+v", node)
		}
		if 1e-9 < DistanceKm(node.Position, node.Genesis) {
			t.Fatalf("%s moved to %+v", node.Id, node.Position)
		}
	}
	// a warm start with no terms behind it decays back to genesis
	nodes[0].PreviousCorrection = Offset{NorthKm: 30, EastKm: -40}
	result = Solve(nodes, nil, nil, nil, settings)
	if 0.01 < result.Node("a").Correction.LengthKm() {
		t.Fatalf("an unsupported warm start stayed at %+v", result.Node("a").Correction)
	}
}

// A solve started from a previous one's corrections gets the same answer in a
// sweep or two, and the identical answer from the nodes and terms in any
// order.
func TestSolveWarmStartAndOrder(t *testing.T) {
	settings := DefaultSettings()
	network := newSyntheticNetwork(11, settings, true)
	aggregator := NewAggregator(settings)
	network.pings(aggregator, 16, 0.2, nil, nil)
	terms := aggregator.Terms()
	cold := Solve(network.nodes, terms, nil, nil, settings)

	warmNodes := slices.Clone(network.nodes)
	for i := range warmNodes {
		previous := cold.Node(warmNodes[i].Id)
		// the derive job recovers the correction from the stored position
		warmNodes[i].PreviousCorrection = OffsetBetween(previous.Genesis, previous.Position)
		assertNear(t, "recovered correction", warmNodes[i].PreviousCorrection.NorthKm, previous.Correction.NorthKm, 1e-6)
		assertNear(t, "recovered correction", warmNodes[i].PreviousCorrection.EastKm, previous.Correction.EastKm, 1e-6)
	}
	warm := Solve(warmNodes, terms, nil, nil, settings)
	for i := range warm.Nodes {
		if 0.05 < DistanceKm(warm.Nodes[i].Position, cold.Nodes[i].Position) {
			t.Fatalf("%s: warm start ended %.3f km from the cold solve", warm.Nodes[i].Id, DistanceKm(warm.Nodes[i].Position, cold.Nodes[i].Position))
		}
	}
	t.Logf("sweeps per round: cold %v, warm %v", cold.Sweeps, warm.Sweeps)
	if !(warm.Sweeps[0] < cold.Sweeps[0]) {
		t.Fatalf("a warm start took %v sweeps, a cold one %v", warm.Sweeps, cold.Sweeps)
	}

	random := mathrand.New(mathrand.NewSource(1))
	shuffledNodes := slices.Clone(network.nodes)
	random.Shuffle(len(shuffledNodes), func(i int, j int) { shuffledNodes[i], shuffledNodes[j] = shuffledNodes[j], shuffledNodes[i] })
	shuffledTerms := slices.Clone(terms)
	random.Shuffle(len(shuffledTerms), func(i int, j int) { shuffledTerms[i], shuffledTerms[j] = shuffledTerms[j], shuffledTerms[i] })
	shuffled := Solve(shuffledNodes, shuffledTerms, nil, nil, settings)
	for i := range shuffledNodes {
		if !reflect.DeepEqual(*shuffled.Node(shuffledNodes[i].Id), *cold.Node(shuffledNodes[i].Id)) {
			t.Fatalf("%s differs with the input shuffled", shuffledNodes[i].Id)
		}
	}
	if !reflect.DeepEqual(shuffled.Population, cold.Population) || !reflect.DeepEqual(shuffled.Excluded, cold.Excluded) {
		t.Fatal("the population differs with the input shuffled")
	}
}

// A solve only reads its settings, and nil settings are the defaults.
func TestSolveLeavesSettingsAlone(t *testing.T) {
	settings := DefaultSettings()
	before := *settings
	network := newSyntheticNetwork(1, settings, true)
	aggregator := NewAggregator(settings)
	network.pings(aggregator, 4, 0.2, nil, nil)
	Solve(network.nodes, aggregator.Terms(), map[string]Refusals{"node-01": {AsPinger: 1, PingsAsPinger: 4}}, nil, settings)
	if *settings != before {
		t.Fatalf("Solve changed its settings: %+v, was %+v", *settings, before)
	}
	// nil settings are the defaults
	if result := Solve(network.nodes, aggregator.Terms(), nil, nil, nil); len(result.Sweeps) != DefaultSettings().ReputationRounds {
		t.Fatalf("nil settings ran %d rounds", len(result.Sweeps))
	}
}

// The publish gate, and the first gate a node fails: enough pings, enough
// peers -- three, so a node with two is refused however many pings it has --
// still when the solve stopped, a residual below genesis, and no cap on the
// distance moved.
func TestPublishable(t *testing.T) {
	settings := DefaultSettings()
	improved := NodeResult{PingCount: 3, PeerCount: 3, ResidualKm: 4, GenesisResidualKm: 9}
	for _, test := range []struct {
		name string
		edit func(node *NodeResult)
		want PublishRefusal
	}{
		{name: "improves with enough pings and peers", edit: func(node *NodeResult) {}, want: PublishRefusalNone},
		{name: "too few pings", edit: func(node *NodeResult) { node.PingCount = 2 }, want: PublishRefusalFewPings},
		{name: "one peer", edit: func(node *NodeResult) { node.PeerCount = 1 }, want: PublishRefusalFewPeers},
		{name: "two peers", edit: func(node *NodeResult) { node.PeerCount = 2 }, want: PublishRefusalFewPeers},
		{name: "two peers, many pings", edit: func(node *NodeResult) { node.PeerCount = 2; node.PingCount = 400 }, want: PublishRefusalFewPeers},
		{name: "many peers", edit: func(node *NodeResult) { node.PeerCount = 64; node.PingCount = 400 }, want: PublishRefusalNone},
		{name: "no better than genesis", edit: func(node *NodeResult) { node.ResidualKm = node.GenesisResidualKm }, want: PublishRefusalNoImprovement},
		{name: "still moving", edit: func(node *NodeResult) { node.LastStepKm = 2 * settings.PublishMaxLastStepKm }, want: PublishRefusalStillMoving},
		{name: "just still enough", edit: func(node *NodeResult) { node.LastStepKm = settings.PublishMaxLastStepKm }, want: PublishRefusalNone},
		{name: "worse than genesis", edit: func(node *NodeResult) { node.ResidualKm = 12 }, want: PublishRefusalNoImprovement},
		// no distance cap (D11): the genesis term bounds the shift
		{name: "moved a long way", edit: func(node *NodeResult) { node.Correction = Offset{NorthKm: 900, EastKm: -700} }, want: PublishRefusalNone},
	} {
		node := improved
		test.edit(&node)
		if got := PublishRefusalOf(&node, settings); got != test.want {
			t.Errorf("%s: PublishRefusalOf = %d, want %d", test.name, got, test.want)
		}
		if got := Publishable(&node, settings); got != (test.want == PublishRefusalNone) {
			t.Errorf("%s: Publishable = %v", test.name, got)
		}
	}

	// From a solve: of four nodes pinging each other, the one whose wide
	// genesis was wrong is published near the truth, fixed by three peers
	// that are not on one line. Its genesis still pulls it by w/(w + Σv) of
	// the error along its least measured direction, which 50 samples a term
	// keep under a km.
	solveSettings := DefaultSettings()
	solveSettings.ReputationRounds = 1
	truth := []LatLon{
		onEquator(0),
		onEquator(400),
		{Latitude: 3, Longitude: 200 / equatorKmPerDegree},
		{Latitude: -2, Longitude: 300 / equatorKmPerDegree},
	}
	nodes := []Node{
		{Id: "a", Genesis: truth[0], RadiusKm: 5},
		{Id: "b", Genesis: truth[1], RadiusKm: 5},
		{Id: "c", Genesis: Move(truth[2], Offset{NorthKm: 60, EastKm: 40}), RadiusKm: 500},
		{Id: "d", Genesis: truth[3], RadiusKm: 5},
	}
	terms := []Term{}
	for i := range nodes {
		for j := range nodes {
			if i != j {
				terms = append(terms, Term{Source: nodes[i].Id, Target: nodes[j].Id, RttMs: rttFor(solveSettings, DistanceKm(truth[i], truth[j])), Samples: 50})
			}
		}
	}
	result := Solve(nodes, terms, nil, nil, solveSettings)
	c := result.Node("c")
	if c.PeerCount != 3 || !Publishable(c, solveSettings) {
		t.Fatalf("c: %+v", c)
	}
	if 1 < DistanceKm(c.Position, truth[2]) {
		t.Fatalf("c derived %.3f km from the truth", DistanceKm(c.Position, truth[2]))
	}
	t.Logf("c: genesis %.1f km off, derived %.3f km off; residual %.3f km, at genesis %.3f km", DistanceKm(c.Genesis, truth[2]), DistanceKm(c.Position, truth[2]), c.ResidualKm, c.GenesisResidualKm)
}

// Two peers fix a node only up to the line through them: its mirror image
// across that line is exactly as far from both, so a node whose wide genesis
// lies on the mirror's side settles at the mirror, hundreds of km from where
// it is, explaining its pings as well as the truth would. Every other gate
// passes it there -- the old gate of two peers published it -- so the peer
// gate is what refuses it, as few peers. A third peer off the line breaks the
// tie: from the same genesis the node settles where it is, and publishes.
func TestPublishRefusesTwoPeers(t *testing.T) {
	settings := DefaultSettings()
	// exact anchors and no reputation: only the pings move the node
	settings.GenesisMinRadiusKm = 0
	settings.ReputationRounds = 1
	anchors := []Node{
		{Id: "anchor-west", Genesis: onEquator(0), RadiusKm: 0},
		{Id: "anchor-east", Genesis: onEquator(400), RadiusKm: 0},
		{Id: "anchor-north", Genesis: LatLon{Latitude: 3, Longitude: 300 / equatorKmPerDegree}, RadiusKm: 0},
	}
	truth := LatLon{Latitude: 1.8, Longitude: 200 / equatorKmPerDegree}
	// across the equator, the line through the first two anchors
	mirror := LatLon{Latitude: -truth.Latitude, Longitude: truth.Longitude}
	node := Node{Id: "node", Genesis: Move(mirror, Offset{NorthKm: 50, EastKm: -30}), RadiusKm: 1000}
	solveWith := func(peers []Node) *NodeResult {
		terms := []Term{}
		for _, peer := range peers {
			rttMs := rttFor(settings, DistanceKm(peer.Genesis, truth))
			terms = append(terms,
				Term{Source: peer.Id, Target: node.Id, RttMs: rttMs, Samples: 50},
				Term{Source: node.Id, Target: peer.Id, RttMs: rttMs, Samples: 50},
			)
		}
		result := Solve(append(slices.Clone(peers), node), terms, nil, nil, settings)
		return result.Node(node.Id)
	}

	two := solveWith(anchors[:2])
	t.Logf("two peers: %d pings, %.3f km from the mirror, %.1f km from the truth; residual %.4f km, at genesis %.1f km; last step %.6f km",
		two.PingCount, DistanceKm(two.Position, mirror), DistanceKm(two.Position, truth), two.ResidualKm, two.GenesisResidualKm, two.LastStepKm)
	if two.PeerCount != 2 || !(DistanceKm(two.Position, mirror) < 1) {
		t.Fatalf("with two peers the node did not settle at the mirror: %+v", two)
	}
	if refusal := PublishRefusalOf(two, settings); refusal != PublishRefusalFewPeers {
		t.Fatalf("with two peers the node is refused for %d, want few peers", refusal)
	}
	gateOfTwo := *settings
	gateOfTwo.MinDerivePeers = 2
	if !Publishable(two, &gateOfTwo) {
		t.Fatalf("a gate of two peers refuses the mirror for %d, so the peer gate is not what keeps it", PublishRefusalOf(two, &gateOfTwo))
	}

	three := solveWith(anchors)
	t.Logf("three peers: %d pings, %.3f km from the truth; residual %.4f km, at genesis %.1f km; last step %.6f km",
		three.PingCount, DistanceKm(three.Position, truth), three.ResidualKm, three.GenesisResidualKm, three.LastStepKm)
	if three.PeerCount != 3 || !(DistanceKm(three.Position, truth) < 1) {
		t.Fatalf("with three peers the node did not settle at the truth: %+v", three)
	}
	if refusal := PublishRefusalOf(three, settings); refusal != PublishRefusalNone {
		t.Fatalf("with three peers the node is refused for %d", refusal)
	}
}

// The genesis radius of a looked-up row, with and without a recorded radius,
// and of an egress probe.
func TestGenesisRadius(t *testing.T) {
	settings := DefaultSettings()
	for _, test := range []struct {
		name       string
		accuracyKm float64
		level      GenesisLevel
		want       float64
	}{
		{name: "recorded", accuracyKm: 7, level: GenesisLevelCountry, want: 7},
		{name: "none, city", accuracyKm: 0, level: GenesisLevelCity, want: 25},
		{name: "none, region", accuracyKm: 0, level: GenesisLevelRegion, want: 100},
		{name: "none, country", accuracyKm: 0, level: GenesisLevelCountry, want: 500},
		{name: "NaN, city", accuracyKm: math.NaN(), level: GenesisLevelCity, want: 25},
	} {
		if got := settings.GenesisRadiusKm(test.accuracyKm, test.level); got != test.want {
			t.Errorf("%s: %v, want %v", test.name, got, test.want)
		}
	}
	if settings.ProbedGenesisRadiusKm(true) != 25 || settings.ProbedGenesisRadiusKm(false) != 100 {
		t.Fatalf("probed radii %v %v", settings.ProbedGenesisRadiusKm(true), settings.ProbedGenesisRadiusKm(false))
	}
}

// The objective is a sum over the connected components of the terms, so one
// component's line search must never move another. The network's move is
// straight toward its truth, which on perfect pings lowers its objective by
// nearly all of it; the isolated node has just gone home from a stale
// correction, so its move repeats that whole correction, which on a 20 000 km
// radius the genesis charges almost nothing for. A single multiple for both
// would throw it hundreds of km back off its genesis.
func TestSolveComponentsAreIndependent(t *testing.T) {
	for _, seed := range []int64{1, 2, 3} {
		settings := DefaultSettings()
		network := newSyntheticNetwork(seed, settings, true)
		aggregator := NewAggregator(settings)
		network.pings(aggregator, 100, 0, nil, nil)
		isolated := Node{Id: "isolated", Genesis: LatLon{Latitude: 47, Longitude: 10}, RadiusKm: 20000}
		problem := newProblem(append(slices.Clone(network.nodes), isolated), aggregator.Terms(), nil, nil, settings)
		problem.findComponents()
		for j := range network.nodes {
			i := problem.nodeIdIndexes[network.nodes[j].Id]
			problem.moves[i] = OffsetBetween(network.nodes[j].Genesis, network.truth[j])
		}
		x := problem.nodeIdIndexes["isolated"]
		problem.moves[x] = Offset{NorthKm: -300, EastKm: 200}

		movedKm := problem.lineSearch()
		if !(1 < movedKm) {
			t.Fatalf("seed %d: test geometry: the line search moved the network %.3f km", seed, movedKm)
		}
		if problem.corrections[x] != (Offset{}) {
			t.Fatalf("seed %d: the network's line search moved the isolated node to %+v", seed, problem.corrections[x])
		}
		t.Logf("seed %d: the line search moved the network up to %.3f km and the isolated node not at all", seed, movedKm)
	}
}

// The solve is the same bit for bit whatever order its terms come in, as the
// derive job's cursors deliver them: the whole result, every node and every
// population statistic, for the terms reversed and shuffled.
func TestSolveIgnoresTermOrder(t *testing.T) {
	settings := DefaultSettings()
	network := newSyntheticNetwork(4, settings, true)
	aggregator := NewAggregator(settings)
	network.pings(aggregator, 16, 0.2, nil, nil)
	terms := aggregator.Terms()
	want := Solve(network.nodes, terms, nil, nil, settings)
	random := mathrand.New(mathrand.NewSource(9))
	reversed := slices.Clone(terms)
	slices.Reverse(reversed)
	shuffled := slices.Clone(terms)
	random.Shuffle(len(shuffled), func(i int, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
	for _, permuted := range [][]Term{reversed, shuffled} {
		if got := Solve(network.nodes, permuted, nil, nil, settings); !reflect.DeepEqual(got, want) {
			t.Fatal("the result depends on the order of the terms")
		}
	}
}

// Seed 13 of the acceptance continent has a stiff pair, 93 km apart at the
// truth and 167 km apart at genesis. Swept one node at a time, the pair and
// everything else come to within a kilometre of the truth; swept all at once
// (Jacobi), both ends closed the whole gap in the same sweep and the pair
// settled in a wrong arrangement 72 km off, which no later sweep undid.
func TestSolveKeepsStiffPairsApart(t *testing.T) {
	settings := DefaultSettings()
	network := newSyntheticNetwork(13, settings, true)
	pair := [2]int{11, 14}
	if gapKm := DistanceKm(network.truth[pair[0]], network.truth[pair[1]]); !(gapKm < 100) {
		t.Fatalf("test geometry: the pair is %.1f km apart", gapKm)
	}
	result := solveSynthetic(network, 100, 0, nil, nil)
	_, largestKm := network.errorsKm(result, nil)
	t.Logf("largest error %.3f km; sweeps %v", largestKm, result.Sweeps)
	if !(largestKm < 1) {
		t.Fatalf("largest error %.3f km, want under 1 km", largestKm)
	}
}

// The anchors, the determined node and the weak node of the publish gate's
// tests, and their terms: the three pinned anchors ping each other; the
// determined node is pinged from all three, around it, with fifty samples a
// term and starts 2 km from its truth; the weak node is pinged from the same
// three with two samples a term and starts 200 km from its truth, so it is
// still sliding toward it a few sweeps in. Every node has at least the three
// peers the default peer gate asks for, so each is refused, or not, by the
// gate its test is about.
func stillMovingProblem(settings *Settings) ([]Node, []Term, LatLon, LatLon) {
	anchors := []Node{
		{Id: "anchor-west", Genesis: LatLon{Latitude: 0, Longitude: 0}, RadiusKm: 0},
		{Id: "anchor-east", Genesis: LatLon{Latitude: 0, Longitude: 2}, RadiusKm: 0},
		{Id: "anchor-north", Genesis: LatLon{Latitude: 1.5, Longitude: 1}, RadiusKm: 0},
	}
	determinedTruth := LatLon{Latitude: 0.5, Longitude: 1}
	weakTruth := LatLon{Latitude: -2, Longitude: 3}
	nodes := append(slices.Clone(anchors),
		Node{Id: "determined", Genesis: Move(determinedTruth, Offset{NorthKm: 1.2, EastKm: -1.6}), RadiusKm: 1000},
		Node{Id: "weak", Genesis: Move(weakTruth, Offset{NorthKm: 120, EastKm: 160}), RadiusKm: 1000},
	)
	terms := []Term{}
	ping := func(a LatLon, aId string, b LatLon, bId string, samples int) {
		rttMs := rttFor(settings, DistanceKm(a, b))
		terms = append(terms,
			Term{Source: aId, Target: bId, RttMs: rttMs, Samples: samples},
			Term{Source: bId, Target: aId, RttMs: rttMs, Samples: samples},
		)
	}
	for i, anchor := range anchors {
		for _, other := range anchors[i+1:] {
			ping(anchor.Genesis, anchor.Id, other.Genesis, other.Id, 50)
		}
	}
	for _, anchor := range anchors {
		ping(anchor.Genesis, anchor.Id, determinedTruth, "determined", 50)
		ping(anchor.Genesis, anchor.Id, weakTruth, "weak", 2)
	}
	return nodes, terms, determinedTruth, weakTruth
}

// Settings for the publish gate's tests: exact anchors, no reputation, and a
// solve cut off after three sweeps, when the weak node is still sliding.
func stillMovingSettings() *Settings {
	settings := DefaultSettings()
	settings.GenesisMinRadiusKm = 0
	settings.ReputationRounds = 1
	settings.MaxIterations = 3
	return settings
}

// A node that is still sliding when the solve stops is refused as still
// moving, while a determined one publishes.
func TestPublishRefusesStillMoving(t *testing.T) {
	settings := stillMovingSettings()
	nodes, terms, determinedTruth, weakTruth := stillMovingProblem(settings)
	result := Solve(nodes, terms, nil, nil, settings)
	weak := result.Node("weak")
	determined := result.Node("determined")
	t.Logf("weak: last step %.4f km, %d pings from %d peers, %.1f km off; determined: last step %.6f km, %.4f km off; sweeps %v",
		weak.LastStepKm, weak.PingCount, weak.PeerCount, DistanceKm(weak.Position, weakTruth), determined.LastStepKm, DistanceKm(determined.Position, determinedTruth), result.Sweeps)
	if refusal := PublishRefusalOf(weak, settings); refusal != PublishRefusalStillMoving {
		t.Fatalf("the weak node is refused for %d, want still moving (last step %.4f km)", refusal, weak.LastStepKm)
	}
	if !Publishable(determined, settings) {
		t.Fatalf("the determined node is refused for %d", PublishRefusalOf(determined, settings))
	}
	// with the gate opened past the weak node's last step, only the pings
	// and the residual judge it
	opened := *settings
	opened.PublishMaxLastStepKm = 2 * weak.LastStepKm
	if refusal := PublishRefusalOf(weak, &opened); refusal == PublishRefusalStillMoving {
		t.Fatal("the weak node is refused as still moving past its last step")
	}
}

// The refusal reasons are counted in the result -- the counts the derive job
// records with its run and the monitor reads -- one for every node that is
// not publishable, by the first gate it failed: a node with one ping as few
// pings; one with three pings from one peer, and one with six from two, as
// few peers; the weak node as still moving; and the pinned anchors, which
// cannot improve on an exact genesis, as no improvement.
func TestPublishRefusalsAreCounted(t *testing.T) {
	settings := stillMovingSettings()
	nodes, terms, _, _ := stillMovingProblem(settings)
	onePing := LatLon{Latitude: 3, Longitude: 3}
	onePeer := LatLon{Latitude: 3, Longitude: -1}
	twoPeers := LatLon{Latitude: -1, Longitude: -1}
	nodes = append(nodes,
		Node{Id: "one-ping", Genesis: onePing, RadiusKm: 100},
		Node{Id: "one-peer", Genesis: onePeer, RadiusKm: 100},
		Node{Id: "two-peers", Genesis: twoPeers, RadiusKm: 100},
	)
	anchorWest := LatLon{Latitude: 0, Longitude: 0}
	anchorNorth := LatLon{Latitude: 1.5, Longitude: 1}
	terms = append(terms,
		Term{Source: "one-ping", Target: "anchor-north", RttMs: rttFor(settings, DistanceKm(onePing, anchorNorth)), Samples: 1},
		Term{Source: "one-peer", Target: "anchor-north", RttMs: rttFor(settings, DistanceKm(onePeer, anchorNorth)), Samples: 3},
		Term{Source: "two-peers", Target: "anchor-west", RttMs: rttFor(settings, DistanceKm(twoPeers, anchorWest)), Samples: 3},
		Term{Source: "two-peers", Target: "anchor-north", RttMs: rttFor(settings, DistanceKm(twoPeers, anchorNorth)), Samples: 3},
	)
	result := Solve(nodes, terms, nil, nil, settings)
	want := PublishRefusals{FewPings: 1, FewPeers: 2, StillMoving: 1, NoImprovement: 3}
	if result.PublishRefusals != want {
		for _, node := range result.Nodes {
			t.Logf("%s: refusal %d, last step %.4f km, %d pings from %d peers, residual %.4f against %.4f", node.Id, PublishRefusalOf(&node, settings), node.LastStepKm, node.PingCount, node.PeerCount, node.ResidualKm, node.GenesisResidualKm)
		}
		t.Fatalf("refusals %+v, want %+v", result.PublishRefusals, want)
	}
	counted := result.PublishRefusals.FewPings + result.PublishRefusals.FewPeers + result.PublishRefusals.StillMoving + result.PublishRefusals.NoImprovement
	published := 0
	for i := range result.Nodes {
		if Publishable(&result.Nodes[i], settings) {
			published += 1
		}
	}
	if counted+published != len(result.Nodes) || published != 1 {
		t.Fatalf("%d refused and %d published of %d nodes", counted, published, len(result.Nodes))
	}
}

// The objective's stagnation stops a solve before the cap, on the same sweep
// every time, and a solve without it runs on: on the acceptance continent with
// a step tolerance nothing meets, only the stagnation or the cap can stop it.
func TestSolveStopsOnStagnation(t *testing.T) {
	settings := DefaultSettings()
	settings.ReputationRounds = 1
	settings.MinStepKm = 1e-12
	settings.MaxIterations = 300
	network := newSyntheticNetwork(2, settings, true)
	aggregator := NewAggregator(settings)
	network.pings(aggregator, 16, 0.2, nil, nil)
	terms := aggregator.Terms()

	stopped := Solve(network.nodes, terms, nil, nil, settings)
	again := Solve(network.nodes, terms, nil, nil, settings)
	noStop := DefaultSettings()
	noStop.ReputationRounds = 1
	noStop.MinStepKm = 1e-12
	noStop.MaxIterations = 300
	noStop.StagnationSweeps = 0
	capped := Solve(network.nodes, terms, nil, nil, noStop)
	stoppedRmsKm, _ := network.errorsKm(stopped, nil)
	cappedRmsKm, _ := network.errorsKm(capped, nil)
	t.Logf("stopped after %v sweeps (RMS %.4f km), the cap after %v (RMS %.4f km)", stopped.Sweeps, stoppedRmsKm, capped.Sweeps, cappedRmsKm)
	if !stopped.Stagnated || !stopped.Converged || !(stopped.Sweeps[0] < settings.MaxIterations) {
		t.Fatalf("stopped after %v sweeps, stagnated %v", stopped.Sweeps, stopped.Stagnated)
	}
	if !reflect.DeepEqual(again, stopped) {
		t.Fatal("the stagnation stop fell differently on the same solve")
	}
	if capped.Stagnated || capped.Converged || capped.Sweeps[0] != noStop.MaxIterations {
		t.Fatalf("without the stop: %v sweeps, stagnated %v, converged %v", capped.Sweeps, capped.Stagnated, capped.Converged)
	}
	// Under noise the objective's minimum is not the truth, so sweeping on can
	// move the error either way; the stop must leave it where the cap does,
	// to within a few percent.
	if 0.05*cappedRmsKm < math.Abs(stoppedRmsKm-cappedRmsKm) {
		t.Fatalf("the stop left an RMS error of %.4f km against %.4f km at the cap", stoppedRmsKm, cappedRmsKm)
	}
}
