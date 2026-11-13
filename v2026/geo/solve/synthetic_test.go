// The synthetic continent the acceptance tests of §5.6 share: true
// coordinates, geneses, and pings derived from the truth.

package solve

import (
	"fmt"
	"math"
	mathrand "math/rand"
)

// The geometry the acceptance tests of §5.6 share: true coordinates spread
// over a continent, a genesis for every node, and pings derived from the truth
// with k and o as configured.
type syntheticNetwork struct {
	settings *Settings
	random   *mathrand.Rand
	truth    []LatLon
	anchor   []bool
	nodes    []Node
}

// The continent: 20 nodes over a box the size of Europe, about 1 700 km north
// to south and 2 300 km east to west, so the pings run from tens of km to
// nearly 3 000 km.
const (
	syntheticNodeCount    = 20
	syntheticSouth        = 40.0
	syntheticNorth        = 55.0
	syntheticWest         = -5.0
	syntheticEast         = 25.0
	syntheticWideRadiusKm = 1000.0
	// GeoLite2's radius for a well placed residential prefix
	syntheticAnchorRadiusKm = 5.0
)

// The anchors sit near the four corners, so between them they fix the frame --
// the translation and the rotation the pings alone cannot see.
var syntheticAnchorCorners = []LatLon{
	{Latitude: 41, Longitude: -4},
	{Latitude: 41, Longitude: 24},
	{Latitude: 54, Longitude: -4},
	{Latitude: 54, Longitude: 24},
}

// The id of synthetic node i.
func syntheticNodeId(i int) string {
	return fmt.Sprintf("node-%02d", i)
}

// The nodes placed. With perturbed, every node but the
// anchors gets a wide-radius genesis moved tens of km from its truth, in a
// random direction; otherwise every genesis is the truth.
func newSyntheticNetwork(seed int64, settings *Settings, perturbed bool) *syntheticNetwork {
	network := &syntheticNetwork{
		settings: settings,
		random:   mathrand.New(mathrand.NewSource(seed)),
	}
	for i := 0; i < syntheticNodeCount; i += 1 {
		var truth LatLon
		anchor := i < len(syntheticAnchorCorners)
		if anchor {
			corner := syntheticAnchorCorners[i]
			truth = LatLon{
				Latitude:  corner.Latitude + network.random.Float64() - 0.5,
				Longitude: corner.Longitude + network.random.Float64() - 0.5,
			}
		} else {
			truth = LatLon{
				Latitude:  syntheticSouth + network.random.Float64()*(syntheticNorth-syntheticSouth),
				Longitude: syntheticWest + network.random.Float64()*(syntheticEast-syntheticWest),
			}
		}
		genesis := truth
		radiusKm := syntheticAnchorRadiusKm
		if !anchor {
			radiusKm = syntheticWideRadiusKm
			if perturbed {
				bearing := network.random.Float64() * 2 * math.Pi
				shiftKm := 20 + network.random.Float64()*40
				genesis = Move(truth, Offset{NorthKm: shiftKm * math.Cos(bearing), EastKm: shiftKm * math.Sin(bearing)})
			}
		}
		network.truth = append(network.truth, truth)
		network.anchor = append(network.anchor, anchor)
		network.nodes = append(network.nodes, Node{
			Id:       syntheticNodeId(i),
			Genesis:  genesis,
			RadiusKm: radiusKm,
		})
	}
	return network
}

// The round trip the truth implies: d/k + o, exactly the inverse of the
// implied distance.
func (self *syntheticNetwork) trueRttMs(from LatLon, to LatLon) float64 {
	return DistanceKm(from, to)/self.settings.KmPerMs + self.settings.OverheadMs
}

// Adds `samples` pings for every ordered pair the source measures, each
// with independent zero-mean Gaussian noise of sigmaMs. rtt, when not nil,
// replaces the true round trip of a pair; measures, when not nil, says which
// pairs a source measures.
func (self *syntheticNetwork) pings(
	aggregator *Aggregator,
	samples int,
	sigmaMs float64,
	measures func(source int, target int) bool,
	rtt func(source int, target int) float64,
) {
	for source := range self.truth {
		for target := range self.truth {
			if source == target {
				continue
			}
			if measures != nil && !measures(source, target) {
				continue
			}
			rttMs := self.trueRttMs(self.truth[source], self.truth[target])
			if rtt != nil {
				rttMs = rtt(source, target)
			}
			for j := 0; j < samples; j += 1 {
				// a sample cannot come back before it was sent
				aggregator.Add(syntheticNodeId(source), syntheticNodeId(target), max(0, rttMs+sigmaMs*self.random.NormFloat64()))
			}
		}
	}
}

// The RMS and the largest distance from the truth of the derived
// positions, over the nodes that `include` selects (all when nil).
func (self *syntheticNetwork) errorsKm(result *Result, include func(i int) bool) (float64, float64) {
	var sumSquares float64
	var largestKm float64
	count := 0
	for i := range self.truth {
		if include != nil && !include(i) {
			continue
		}
		errorKm := DistanceKm(result.Node(syntheticNodeId(i)).Position, self.truth[i])
		sumSquares += errorKm * errorKm
		largestKm = max(largestKm, errorKm)
		count += 1
	}
	return math.Sqrt(sumSquares / float64(count)), largestKm
}
