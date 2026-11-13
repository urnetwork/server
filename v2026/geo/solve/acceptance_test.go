// The acceptance tests of connect/GEOMAP.md §5.6, the bar for turning the
// derive job on, and the no-pings property test beside them.

package solve

import (
	"math"
	mathrand "math/rand"
	"slices"
	"testing"
)

// The acceptance tests of connect/GEOMAP.md §5.6, the bar for turning the
// derive job on. Each runs the default settings on the synthetic continent of
// synthetic_test.go: 20 nodes, four of them tight-radius anchors near the
// corners, the rest with a 1 000 km genesis radius, and every source pinging
// every peer unless a test says otherwise. Each runs on several seeds, so a
// pass is not one lucky geometry.
var acceptanceSeeds = []int64{1, 2, 3}

const (
	// Zero-mean Gaussian noise on every sample: 0.2 ms of round trip, 20 km of
	// implied distance per sample.
	acceptanceNoiseMs = 0.2
	// the fixed sample count per term, and the larger one
	acceptanceFixedSamples  = 16
	acceptanceLargerSamples = 256
	// The tolerance on the RMS error over all nodes at the fixed count. The
	// median gives 2.6–3.4 km on these seeds, and no more than 6.0 km on 40
	// seeds; the minimum gives no less than 32 km on the same 40.
	acceptanceNoiseToleranceKm = 8.0
)

// A synthetic network pinged and solved.
func solveSynthetic(
	network *syntheticNetwork,
	samples int,
	sigmaMs float64,
	measures func(source int, target int) bool,
	rtt func(source int, target int) float64,
) *Result {
	aggregator := NewAggregator(network.settings)
	network.pings(aggregator, samples, sigmaMs, measures, rtt)
	return Solve(network.nodes, aggregator.Terms(), nil, nil, network.settings)
}

// The lowest weight among the nodes include selects.
func lowestQ(result *Result, include func(id string) bool) float64 {
	q := 1.0
	for _, node := range result.Nodes {
		if include(node.Id) {
			q = min(q, node.Q)
		}
	}
	return q
}

// Selects every node.
func everyNode(id string) bool {
	return true
}

// Test 1: perfect pings, rtt = d(p*_i, p*_j)/k + o for every pair. With genesis
// equal to the truth the solver moves nothing; with the wide-radius geneses
// perturbed by 20–60 km and the anchors left at the truth, it reproduces the
// truth to under a kilometre: the pings fix the geometry, the anchors the
// frame.
func TestAcceptancePerfectPings(t *testing.T) {
	for _, seed := range acceptanceSeeds {
		settings := DefaultSettings()
		result := solveSynthetic(newSyntheticNetwork(seed, settings, false), 100, 0, nil, nil)
		largestCorrectionKm := 0.0
		for _, node := range result.Nodes {
			largestCorrectionKm = max(largestCorrectionKm, node.Correction.LengthKm())
		}
		if 1e-6 < largestCorrectionKm {
			t.Fatalf("seed %d: genesis at the truth moved a node %g km", seed, largestCorrectionKm)
		}

		network := newSyntheticNetwork(seed, settings, true)
		result = solveSynthetic(network, 100, 0, nil, nil)
		rmsKm, largestKm := network.errorsKm(result, nil)
		genesisRmsKm, genesisLargestKm := 0.0, 0.0
		for i := range network.nodes {
			errorKm := DistanceKm(network.nodes[i].Genesis, network.truth[i])
			genesisRmsKm += errorKm * errorKm
			genesisLargestKm = max(genesisLargestKm, errorKm)
		}
		genesisRmsKm = math.Sqrt(genesisRmsKm / float64(len(network.nodes)))
		t.Logf(
			"seed %d: genesis at the truth moved at most %.1e km; perturbed genesis (RMS %.1f km, largest %.1f km) derived to RMS %.3f km, largest %.3f km; lowest q %.3f; sweeps %v",
			seed, largestCorrectionKm, genesisRmsKm, genesisLargestKm, rmsKm, largestKm, lowestQ(result, everyNode), result.Sweeps,
		)
		if !(largestKm < 1) {
			t.Fatalf("seed %d: largest error %.3f km, want under 1 km", seed, largestKm)
		}
	}
}

// Test 2 for one aggregate: whether the RMS error at the fixed sample count is
// under the tolerance and falls at the larger count. Production keeps
// PairSampleReservoir (16) samples of a pair; the test keeps every sample, up
// to the larger count, since what it measures is the aggregate of all of
// them. TestAcceptanceZeroMeanNoise holds the fixed count separately to the
// tolerance at the production reservoir.
func zeroMeanNoise(t *testing.T, seed int64, edgeAggregate EdgeAggregate) (bool, float64, float64) {
	aggregateName := "median"
	if edgeAggregate == EdgeAggregateMin {
		aggregateName = "min"
	}
	errorsKm := []float64{}
	for _, samples := range []int{acceptanceFixedSamples, acceptanceLargerSamples} {
		settings := DefaultSettings()
		settings.EdgeAggregate = edgeAggregate
		// the test measures the aggregate of every sample, so the reservoir
		// keeps them all
		settings.PairSampleReservoir = acceptanceLargerSamples
		network := newSyntheticNetwork(seed, settings, true)
		result := solveSynthetic(network, samples, acceptanceNoiseMs, nil, nil)
		rmsKm, largestKm := network.errorsKm(result, nil)
		t.Logf(
			"seed %d, %s, %d samples a term: RMS %.3f km, largest %.3f km; lowest q %.3f; sweeps %v",
			seed, aggregateName, samples, rmsKm, largestKm, lowestQ(result, everyNode), result.Sweeps,
		)
		errorsKm = append(errorsKm, rmsKm)
	}
	passes := errorsKm[0] < acceptanceNoiseToleranceKm && errorsKm[1] < errorsKm[0]
	return passes, errorsKm[0], errorsKm[1]
}

// Test 2: zero-mean noise on every sample, many samples a term. The median's
// error is under the tolerance at the fixed count and lower at the larger one.
// The same test fails with the minimum as the aggregate: the least of n
// samples of zero-mean noise sits well below the true round trip, so every
// implied distance runs short and the network contracts.
func TestAcceptanceZeroMeanNoise(t *testing.T) {
	for _, seed := range acceptanceSeeds {
		passes, fixedKm, largerKm := zeroMeanNoise(t, seed, EdgeAggregateMedian)
		if !passes {
			t.Fatalf("seed %d: median RMS %.3f km at %d samples, %.3f km at %d; want under %.1f km and falling", seed, fixedKm, acceptanceFixedSamples, largerKm, acceptanceLargerSamples, acceptanceNoiseToleranceKm)
		}
		// the fixed count at the production reservoir, which then keeps every
		// sample of a pair
		productionSettings := DefaultSettings()
		productionNetwork := newSyntheticNetwork(seed, productionSettings, true)
		productionResult := solveSynthetic(productionNetwork, acceptanceFixedSamples, acceptanceNoiseMs, nil, nil)
		productionKm, _ := productionNetwork.errorsKm(productionResult, nil)
		t.Logf("seed %d, median, %d samples a term at the production reservoir of %d: RMS %.3f km", seed, acceptanceFixedSamples, productionSettings.PairSampleReservoir, productionKm)
		if !(productionKm < acceptanceNoiseToleranceKm) {
			t.Fatalf("seed %d: median RMS %.3f km at the production reservoir, over %.1f km", seed, productionKm, acceptanceNoiseToleranceKm)
		}
		passes, fixedKm, largerKm = zeroMeanNoise(t, seed, EdgeAggregateMin)
		if passes {
			t.Fatalf("seed %d: the minimum passed: RMS %.3f km at %d samples, %.3f km at %d", seed, fixedKm, acceptanceFixedSamples, largerKm, acceptanceLargerSamples)
		}
		if fixedKm < acceptanceNoiseToleranceKm {
			t.Fatalf("seed %d: the minimum is within the tolerance at the fixed count (%.3f km)", seed, fixedKm)
		}
	}
}

// Test 3: a source that pings only its four nearest peers, with the noise of
// test 2, while every other source pings every peer truthfully. When its round
// trips are those of a point 800 km east of where it is, its weight comes out
// under 0.2 while every honest source keeps 0.8 or more, the honest nodes stay
// within the noise-only tolerance, and the result lists it as excluded. When
// its round trips are truthful, its coverage alone lowers its weight -- by a
// z-score past ExcludeZ, since one source against 19 that measured everything
// is −√19 out -- but never excludes it.
func TestAcceptanceMaliciousSource(t *testing.T) {
	const subsetSize = 4
	const fakeEastKm = 800.0
	for _, seed := range acceptanceSeeds {
		for _, truthful := range []bool{false, true} {
			settings := DefaultSettings()
			network := newSyntheticNetwork(seed, settings, true)
			// a wide-radius node, not an anchor
			bad := len(syntheticAnchorCorners) + 3
			badId := syntheticNodeId(bad)
			subsetIndexes := map[int]bool{}
			for len(subsetIndexes) < subsetSize {
				nearest := -1
				for j := range network.truth {
					if j == bad || subsetIndexes[j] {
						continue
					}
					if nearest < 0 || DistanceKm(network.truth[bad], network.truth[j]) < DistanceKm(network.truth[bad], network.truth[nearest]) {
						nearest = j
					}
				}
				subsetIndexes[nearest] = true
			}
			fake := Move(network.truth[bad], Offset{EastKm: fakeEastKm})
			result := solveSynthetic(
				network,
				acceptanceFixedSamples,
				acceptanceNoiseMs,
				func(source int, target int) bool {
					return source != bad || subsetIndexes[target]
				},
				func(source int, target int) float64 {
					if source == bad && !truthful {
						return network.trueRttMs(fake, network.truth[target])
					}
					return network.trueRttMs(network.truth[source], network.truth[target])
				},
			)

			honest := func(id string) bool {
				return id != badId
			}
			honestRmsKm, honestLargestKm := network.errorsKm(result, func(i int) bool {
				return i != bad
			})
			badNode := result.Node(badId)
			honestQ := lowestQ(result, honest)
			t.Logf(
				"seed %d, truthful %v: subset source q %.4f (z scatter %.2f, bias %.2f, coverage %.2f), excluded %v; lowest honest q %.4f; honest RMS %.3f km, largest %.3f km; listed %v",
				seed, truthful, badNode.Q, badNode.ScatterZ, badNode.BiasZ, badNode.CoverageZ, badNode.Excluded, honestQ, honestRmsKm, honestLargestKm, result.Excluded,
			)
			if !(honestRmsKm < acceptanceNoiseToleranceKm) {
				t.Fatalf("seed %d, truthful %v: honest RMS %.3f km, over the noise-only tolerance %.1f km", seed, truthful, honestRmsKm, acceptanceNoiseToleranceKm)
			}

			if !truthful {
				if !(badNode.Q < 0.2) {
					t.Fatalf("seed %d: malicious source q %.4f, want under 0.2", seed, badNode.Q)
				}
				if !(0.8 <= honestQ) {
					t.Fatalf("seed %d: lowest honest q %.4f, want 0.8 or more", seed, honestQ)
				}
				if !badNode.Excluded || !slices.Equal(result.Excluded, []string{badId}) {
					t.Fatalf("seed %d: listed %v, want [%s]", seed, result.Excluded, badId)
				}
				continue
			}

			if badNode.Excluded || 0 < len(result.Excluded) {
				t.Fatalf("seed %d: the truthful subset source was excluded (listed %v)", seed, result.Excluded)
			}
			if !(settings.ExcludeZ < -badNode.CoverageZ) {
				t.Fatalf("seed %d: coverage z %.2f is not past ExcludeZ, so the test shows nothing", seed, badNode.CoverageZ)
			}
			if !(badNode.Q < honestQ) {
				t.Fatalf("seed %d: the subset source's q %.4f is not below every honest q (%.4f)", seed, badNode.Q, honestQ)
			}
			// its weight is its coverage's
			wantQ := min(1, max(settings.MinQ, 1/(1+badNode.CoverageZ*badNode.CoverageZ)))
			assertNear(t, "coverage q", badNode.Q, wantQ, 1e-12)
		}
	}
}

// Test 4: no pings. With only its genesis term a node's objective is a
// quadratic whose minimum is its genesis, so a correct solve returns it there
// from any start: cold, or warm started from a correction hundreds of km
// stale. A node with pings too few (MinDerivePings) or to too few peers
// (MinDerivePeers) is solved like any other, since its pings are
// measurements, and the publish gate refuses it. Each case runs on the
// acceptance network, noisy as in test 2, with one extra wide-radius node.
func TestAcceptanceNoPings(t *testing.T) {
	const noPingsToleranceKm = 1e-9
	extraId := syntheticNodeId(syntheticNodeCount)
	for _, seed := range acceptanceSeeds {
		// the extra node's genesis, somewhere on the continent, and a stale
		// correction 200–500 km long in a random direction
		setup := func(settings *Settings) (*syntheticNetwork, *Aggregator, Node, Offset) {
			network := newSyntheticNetwork(seed, settings, true)
			aggregator := NewAggregator(settings)
			network.pings(aggregator, acceptanceFixedSamples, acceptanceNoiseMs, nil, nil)
			extra := Node{
				Id: extraId,
				Genesis: LatLon{
					Latitude:  syntheticSouth + network.random.Float64()*(syntheticNorth-syntheticSouth),
					Longitude: syntheticWest + network.random.Float64()*(syntheticEast-syntheticWest),
				},
				RadiusKm: syntheticWideRadiusKm,
			}
			bearing := network.random.Float64() * 2 * math.Pi
			staleKm := 200 + network.random.Float64()*300
			stale := Offset{NorthKm: staleKm * math.Cos(bearing), EastKm: staleKm * math.Sin(bearing)}
			return network, aggregator, extra, stale
		}

		for _, warm := range []bool{false, true} {
			// (a) cold and (b) warm started from a stale correction
			settings := DefaultSettings()
			network, aggregator, extra, stale := setup(settings)
			if warm {
				extra.PreviousCorrection = stale
			}
			result := Solve(append(slices.Clone(network.nodes), extra), aggregator.Terms(), nil, nil, settings)
			node := result.Node(extraId)
			t.Logf("seed %d, no terms, warm %v (stale %.1f km): correction %.3g km, publishable %v; sweeps %v", seed, warm, extra.PreviousCorrection.LengthKm(), node.Correction.LengthKm(), Publishable(node, settings), result.Sweeps)
			if !(node.Correction.LengthKm() < noPingsToleranceKm) {
				t.Fatalf("seed %d, warm %v: a node with no terms ended %g km from its genesis", seed, warm, node.Correction.LengthKm())
			}
			if Publishable(node, settings) || node.PingCount != 0 || node.PeerCount != 0 {
				t.Fatalf("seed %d, warm %v: %+v", seed, warm, node)
			}
			if rmsKm, _ := network.errorsKm(result, nil); !(rmsKm < acceptanceNoiseToleranceKm) {
				t.Fatalf("seed %d, warm %v: the network's RMS error is %.3f km with the extra node", seed, warm, rmsKm)
			}
		}

		// (c) fewer than MinDerivePings samples, and (d) MinDerivePings samples
		// all from one peer. The samples are honest sources' truthful pings
		// toward the extra node, from a truth 80 km from its genesis, so the
		// node is solved like any other and moves -- its pings are
		// measurements -- and the publish gate is what refuses it.
		for _, test := range []struct {
			name      string
			samples   []int
			wantPeers int
		}{
			{name: "fewer pings than MinDerivePings", samples: []int{1, 1}, wantPeers: 2},
			{name: "MinDerivePings pings from one peer", samples: []int{DefaultSettings().MinDerivePings}, wantPeers: 1},
		} {
			settings := DefaultSettings()
			network, aggregator, extra, _ := setup(settings)
			truth := Move(extra.Genesis, Offset{NorthKm: 48, EastKm: 64})
			terms := aggregator.Terms()
			for j, samples := range test.samples {
				peer := len(syntheticAnchorCorners) + j
				terms = append(terms, Term{
					Source:  syntheticNodeId(peer),
					Target:  extraId,
					RttMs:   network.trueRttMs(network.truth[peer], truth),
					Samples: samples,
				})
			}
			result := Solve(append(slices.Clone(network.nodes), extra), terms, nil, nil, settings)
			node := result.Node(extraId)
			t.Logf("seed %d, %s: %d pings from %d peers, correction %.3f km, publishable %v", seed, test.name, node.PingCount, node.PeerCount, node.Correction.LengthKm(), Publishable(node, settings))
			if node.PeerCount != test.wantPeers || !(node.PingCount < settings.MinDerivePings || node.PeerCount < settings.MinDerivePeers) {
				t.Fatalf("seed %d, %s: %d pings from %d peers", seed, test.name, node.PingCount, node.PeerCount)
			}
			if !(0 < node.Correction.LengthKm()) {
				t.Fatalf("seed %d, %s: the pings did not move the node", seed, test.name)
			}
			if Publishable(node, settings) {
				t.Fatalf("seed %d, %s: publishable", seed, test.name)
			}
		}

		// (e) the whole network with no terms at all, cold and warm started
		for _, warm := range []bool{false, true} {
			settings := DefaultSettings()
			network := newSyntheticNetwork(seed, settings, true)
			nodes := slices.Clone(network.nodes)
			if warm {
				for i := range nodes {
					bearing := network.random.Float64() * 2 * math.Pi
					staleKm := 200 + network.random.Float64()*300
					nodes[i].PreviousCorrection = Offset{NorthKm: staleKm * math.Cos(bearing), EastKm: staleKm * math.Sin(bearing)}
				}
			}
			result := Solve(nodes, nil, nil, nil, settings)
			largestKm := 0.0
			for _, node := range result.Nodes {
				largestKm = max(largestKm, node.Correction.LengthKm())
				if node.Q != 1 || node.Excluded || Publishable(&node, settings) {
					t.Fatalf("seed %d, warm %v: %+v", seed, warm, node)
				}
			}
			t.Logf("seed %d, no terms anywhere, warm %v: largest correction %.3g km; sweeps %v, converged %v", seed, warm, largestKm, result.Sweeps, result.Converged)
			if !(largestKm < noPingsToleranceKm) || 0 < len(result.Excluded) || !result.Converged {
				t.Fatalf("seed %d, warm %v: largest correction %g km, excluded %v, converged %v", seed, warm, largestKm, result.Excluded, result.Converged)
			}
			for _, sweeps := range result.Sweeps {
				// one sweep takes every node home and the next finds nothing to do
				if 3 < sweeps {
					t.Fatalf("seed %d, warm %v: %v sweeps", seed, warm, result.Sweeps)
				}
			}
		}
	}
}

// For any nodes and any stale warm starts, with no terms every node returns to
// its genesis: random points anywhere on the sphere, radii from a metre to half
// the earth, stale corrections from a micrometre to 10 000 km, one reputation
// round or the default.
func TestNoPingsReturnToGenesis(t *testing.T) {
	random := mathrand.New(mathrand.NewSource(7))
	largestKm := 0.0
	for trial := 0; trial < 200; trial += 1 {
		settings := DefaultSettings()
		if trial%2 == 0 {
			settings.ReputationRounds = 1
		}
		nodes := []Node{}
		for i := 0; i < 1+random.Intn(30); i += 1 {
			staleKm := math.Pow(10, -9+13*random.Float64())
			bearing := random.Float64() * 2 * math.Pi
			nodes = append(nodes, Node{
				Id: syntheticNodeId(i),
				Genesis: LatLon{
					Latitude:  math.Asin(2*random.Float64()-1) * 180 / math.Pi,
					Longitude: random.Float64()*360 - 180,
				},
				RadiusKm:           math.Pow(10, -3+7.3*random.Float64()),
				PreviousCorrection: Offset{NorthKm: staleKm * math.Cos(bearing), EastKm: staleKm * math.Sin(bearing)},
			})
		}
		result := Solve(nodes, nil, nil, nil, settings)
		for _, node := range result.Nodes {
			largestKm = max(largestKm, node.Correction.LengthKm())
			if !(node.Correction.LengthKm() < 1e-9) {
				t.Fatalf("trial %d: %s (radius %.3g km, stale %.3g km) ended %g km from its genesis after %v sweeps",
					trial, node.Id, nodes[result.nodeIdIndexes[node.Id]].RadiusKm, nodes[result.nodeIdIndexes[node.Id]].PreviousCorrection.LengthKm(), node.Correction.LengthKm(), result.Sweeps)
			}
		}
	}
	t.Logf("largest correction over 200 problems: %.3g km", largestKm)
}
