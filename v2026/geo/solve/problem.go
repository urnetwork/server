// The problem a solve works on -- its nodes and compact terms, the index of
// the terms each node takes part in, the colors its sweeps run in -- and the
// fixed-order parallel machinery every sweep and sum runs on.

package solve

import (
	"cmp"
	"math"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
)

// One solve's state. Everything that grows with the fleet is a flat
// array: a term is 24 bytes, and the terms a node takes part in are found
// without a list per node -- its own terms are a contiguous range, since the
// terms are ordered by source, and the terms toward it are one range of a
// single index (incoming).
type problem struct {
	settings    *Settings
	containment Containment
	workers     int
	chunk       int

	// the valid nodes in id order, and the order they were given in
	nodes         []Node
	nodeIdIndexes map[string]uint32
	resultOrder   []uint32

	frames         []tangentFrame
	genesisWeights []float64
	// a node whose genesis is exact (a zero radius with a zero floor) and never
	// moves
	pinned         []bool
	regionEnabled  []bool
	countryEnabled []bool

	// ordered by source and then target; node i's own terms are
	// terms[sourceStarts[i]:sourceStarts[i+1]], and the terms toward it are
	// incoming[incomingStarts[i]:incomingStarts[i+1]], in term order
	terms          []problemTerm
	sourceStarts   []uint32
	incomingStarts []uint32
	incoming       []uint32

	corrections   []Offset
	positions     []vec3
	regionHinges  []float64
	countryHinges []float64
	damping       []float64

	q        []float64
	excluded []bool
	// the scoring behind q and excluded, or nil in the first round
	scores *scoreSet
	// coverage and the refusal rates, which do not depend on the geometry
	fixedStatistics fixedStatistics

	// The connected components of the nodes under the terms that weigh this
	// round: node lists componentNodes[componentStarts[c]:componentStarts[c+1]].
	// The objective is a sum over them, so each is its own problem to the
	// line search.
	componentStarts []uint32
	componentNodes  []uint32

	// The colors of the sweeps: nodes colorNodes[colorStarts[c]:colorStarts[c+1]]
	// share no term with each other, so they can step at once.
	colorStarts []uint32
	colorNodes  []uint32

	// how far each node moved in the last sweep of the last solve
	lastStepLengths []float64

	// scratch for the sweeps: each node's step length, the line search's
	// direction and its trial state
	stepLengths        []float64
	moves              []Offset
	trialCorrections   []Offset
	trialPositions     []vec3
	trialRegionHinges  []float64
	trialCountryHinges []float64

	droppedNodeCount int
	droppedTermCount int
}

// A term as the solve keeps it. The implied distance and the
// scale of its relative error are recomputed from the round trip where they
// are needed, a few flops against the bytes of storing them. The round trip
// stays a float64: rounded to a float32 it would be off by up to a fifth of a
// metre at continental range, enough to move a genesis that is exactly the
// truth.
type problemTerm struct {
	source uint32
	target uint32
	rttMs  float64
	// this round's weight v = q_s·n/max(d̂, floor)², 0 when the source is
	// excluded
	weight  float32
	samples uint16
}

// The term's end other than node i.
func (self *problemTerm) other(i int) int {
	if int(self.source) == i {
		return int(self.target)
	}
	return int(self.source)
}

// The implied distance d̂ = k·max(0, rtt − o).
func (self *problem) impliedKm(term *problemTerm) float64 {
	return self.settings.KmPerMs * max(0, term.rttMs-self.settings.OverheadMs)
}

// The scale of a term's relative error, max(d̂, DistanceFloorKm).
func (self *problem) scaleKm(impliedKm float64) float64 {
	return max(impliedKm, self.settings.DistanceFloorKm)
}

// A term's weight before its source's reputation: n/scale².
func (self *problem) baseWeight(term *problemTerm) float64 {
	scaleKm := self.scaleKm(self.impliedKm(term))
	return float64(term.samples) / (scaleKm * scaleKm)
}

// Runs work over the chunks [0, count) on the solve's workers. The
// chunks, not the workers, divide the work, and each chunk writes only its own
// outputs, so the result is the same on any number of workers.
func (self *problem) parallel(count int, work func(chunk int)) {
	workers := min(self.workers, count)
	if workers <= 1 {
		for c := 0; c < count; c += 1 {
			work(c)
		}
		return
	}
	var next atomic.Int64
	var group sync.WaitGroup
	for w := 0; w < workers; w += 1 {
		group.Add(1)
		go func() {
			defer group.Done()
			for {
				c := int(next.Add(1) - 1)
				if count <= c {
					return
				}
				work(c)
			}
		}()
	}
	group.Wait()
}

// How many chunks of the solve's chunk size cover n items.
func (self *problem) chunks(n int) int {
	return (n + self.chunk - 1) / self.chunk
}

// The items [start, end) of chunk c of n items.
func (self *problem) chunkRange(c int, n int) (int, int) {
	return c * self.chunk, min(n, (c+1)*self.chunk)
}

// Runs work on every node, in parallel by chunk.
func (self *problem) parallelNodes(work func(i int)) {
	n := len(self.nodes)
	self.parallel(self.chunks(n), func(c int) {
		start, end := self.chunkRange(c, n)
		for i := start; i < end; i += 1 {
			work(i)
		}
	})
}

// The partial sums of chunks [0, count) added up in chunk order, which is what
// makes a parallel sum the same number on any number of workers: floating
// addition is not associative, but a fixed order of it is a fixed result.
func (self *problem) sum(count int, partial func(chunk int) float64) float64 {
	partials := make([]float64, count)
	self.parallel(count, func(c int) {
		partials[c] = partial(c)
	})
	var total float64
	for _, p := range partials {
		total += p
	}
	return total
}

// A quantity added up over the nodes in fixed chunks.
func (self *problem) sumNodes(value func(i int) float64) float64 {
	n := len(self.nodes)
	return self.sum(self.chunks(n), func(c int) float64 {
		start, end := self.chunkRange(c, n)
		var partial float64
		for i := start; i < end; i += 1 {
			partial += value(i)
		}
		return partial
	})
}

// The largest of a quantity over the nodes. A maximum does not depend on the
// order it is taken in, so it needs no fixed combine.
func (self *problem) maxNodes(value func(i int) float64) float64 {
	n := len(self.nodes)
	count := self.chunks(n)
	partials := make([]float64, count)
	self.parallel(count, func(c int) {
		start, end := self.chunkRange(c, n)
		for i := start; i < end; i += 1 {
			partials[c] = max(partials[c], value(i))
		}
	})
	largest := 0.0
	for _, p := range partials {
		largest = max(largest, p)
	}
	return largest
}

// The problem of a solve: the valid nodes in id order, their terms indexed,
// their colors, each node's state at its warm start, and every term at its
// base weight.
func newProblem(nodes []Node, terms []Term, nodeIdRefusals map[string]Refusals, containment Containment, settings *Settings) *problem {
	workers := settings.Workers
	if workers <= 0 {
		workers = runtime.GOMAXPROCS(0)
	}
	self := &problem{
		settings:    settings,
		containment: containment,
		workers:     workers,
		chunk:       max(1, settings.ParallelChunk),
	}

	// The valid nodes in the order given, the first of each id. The solve
	// works in id order instead, so neither its answer nor the choice between
	// repeated terms depends on the order the job read them in.
	validLatLon := func(p LatLon) bool {
		return -90 <= p.Latitude && p.Latitude <= 90 && !math.IsNaN(p.Longitude) && !math.IsInf(p.Longitude, 0)
	}
	seenNodeIds := make(map[string]bool, len(nodes))
	given := make([]Node, 0, len(nodes))
	for _, node := range nodes {
		if !validLatLon(node.Genesis) || seenNodeIds[node.Id] {
			self.droppedNodeCount += 1
			continue
		}
		seenNodeIds[node.Id] = true
		given = append(given, node)
	}
	seenNodeIds = nil
	self.nodes = slices.Clone(given)
	slices.SortFunc(self.nodes, func(a Node, b Node) int {
		return cmp.Compare(a.Id, b.Id)
	})
	n := len(self.nodes)
	self.nodeIdIndexes = make(map[string]uint32, n)
	for i := range self.nodes {
		self.nodeIdIndexes[self.nodes[i].Id] = uint32(i)
	}
	self.resultOrder = make([]uint32, len(given))
	for i := range given {
		self.resultOrder[i] = self.nodeIdIndexes[given[i].Id]
	}
	given = nil

	self.indexTerms(terms)

	// Color the nodes so that no two of a color share a term: greedily, in
	// node order, each node taking the least color none of its neighbours
	// colored before it has. A node's neighbours are its own terms' targets
	// and the sources of the terms toward it.
	colors := make([]uint32, n)
	colorCount := uint32(0)
	// usedStamps[color] is the (1-based) node that last found color taken
	usedStamps := []uint32{}
	for i := 0; i < n; i += 1 {
		stamp := uint32(i + 1)
		take := func(neighbour uint32) {
			if neighbour < uint32(i) {
				usedStamps[colors[neighbour]] = stamp
			}
		}
		for k := self.sourceStarts[i]; k < self.sourceStarts[i+1]; k += 1 {
			take(self.terms[k].target)
		}
		for _, k := range self.incoming[self.incomingStarts[i]:self.incomingStarts[i+1]] {
			take(self.terms[k].source)
		}
		color := uint32(0)
		for color < colorCount && usedStamps[color] == stamp {
			color += 1
		}
		if color == colorCount {
			colorCount += 1
			usedStamps = append(usedStamps, 0)
		}
		colors[i] = color
	}
	self.colorStarts = make([]uint32, colorCount+1)
	for i := 0; i < n; i += 1 {
		self.colorStarts[colors[i]+1] += 1
	}
	for c := uint32(0); c < colorCount; c += 1 {
		self.colorStarts[c+1] += self.colorStarts[c]
	}
	self.colorNodes = make([]uint32, n)
	colorFill := slices.Clone(self.colorStarts[:colorCount])
	for i := 0; i < n; i += 1 {
		self.colorNodes[colorFill[colors[i]]] = uint32(i)
		colorFill[colors[i]] += 1
	}

	self.frames = make([]tangentFrame, n)
	self.genesisWeights = make([]float64, n)
	self.pinned = make([]bool, n)
	self.regionEnabled = make([]bool, n)
	self.countryEnabled = make([]bool, n)
	self.corrections = make([]Offset, n)
	self.positions = make([]vec3, n)
	self.regionHinges = make([]float64, n)
	self.countryHinges = make([]float64, n)
	self.damping = make([]float64, n)
	self.q = make([]float64, n)
	self.excluded = make([]bool, n)
	self.parallelNodes(func(i int) {
		node := &self.nodes[i]
		self.frames[i] = newTangentFrame(node.Genesis)
		// A radius under the floor, zero or NaN anchors at the floor; one
		// wider than half the earth anchors as half the earth. Every genesis
		// weight is then positive, which is what keeps every block of the
		// solve well posed.
		radiusKm := settings.GenesisMinRadiusKm
		if radiusKm < node.RadiusKm {
			radiusKm = min(node.RadiusKm, halfCircumferenceKm)
		}
		if 0 < radiusKm {
			self.genesisWeights[i] = 1 / (radiusKm * radiusKm)
		} else {
			self.pinned[i] = true
		}
		correction := node.PreviousCorrection
		finite := !math.IsNaN(correction.NorthKm) && !math.IsInf(correction.NorthKm, 0) &&
			!math.IsNaN(correction.EastKm) && !math.IsInf(correction.EastKm, 0)
		if self.pinned[i] || !finite {
			correction = Offset{}
		}
		self.corrections[i] = correction
		self.positions[i] = self.frames[i].at(correction)
		self.regionEnabled[i] = containment != nil && 0 < settings.LambdaRegion && node.RegionKey != ""
		self.countryEnabled[i] = containment != nil && 0 < settings.LambdaCountry && node.CountryKey != ""
		self.regionHinges[i] = self.regionHingeAt(i, self.positions[i])
		self.countryHinges[i] = self.countryHingeAt(i, self.positions[i])
		self.q[i] = 1
	})
	self.parallel(self.chunks(len(self.terms)), func(c int) {
		start, end := self.chunkRange(c, len(self.terms))
		for k := start; k < end; k += 1 {
			self.terms[k].weight = float32(self.baseWeight(&self.terms[k]))
		}
	})
	self.fixedStatistics = self.newFixedStatistics(nodeIdRefusals)

	self.stepLengths = make([]float64, n)
	self.lastStepLengths = make([]float64, n)
	self.moves = make([]Offset, n)
	self.trialCorrections = make([]Offset, n)
	self.trialPositions = make([]vec3, n)
	self.trialRegionHinges = make([]float64, n)
	self.trialCountryHinges = make([]float64, n)
	return self
}

// Validates the terms, orders them by source and then target, keeps
// one per ordered pair, and builds the index of the terms toward each node.
// The ordering is a counting sort by source followed by a sort of each
// source's few terms by target, so it is linear in the terms however many
// there are.
func (self *problem) indexTerms(terms []Term) {
	settings := self.settings
	n := len(self.nodes)
	const invalid = math.MaxUint32

	// resolve the ends of every term, in parallel
	sources := make([]uint32, len(terms))
	targets := make([]uint32, len(terms))
	dropped := make([]int, self.chunks(len(terms)))
	self.parallel(len(dropped), func(c int) {
		start, end := self.chunkRange(c, len(terms))
		for k := start; k < end; k += 1 {
			term := &terms[k]
			sources[k] = invalid
			source, sourceOk := self.nodeIdIndexes[term.Source]
			target, targetOk := self.nodeIdIndexes[term.Target]
			if !sourceOk || !targetOk || source == target || !(0 <= term.RttMs) || math.IsInf(term.RttMs, 1) || term.Samples < 1 {
				dropped[c] += 1
				continue
			}
			scaleKm := max(settings.KmPerMs*max(0, term.RttMs-settings.OverheadMs), settings.DistanceFloorKm)
			if !(0 < scaleKm) || math.IsInf(scaleKm, 1) {
				// a zero distance under a zero floor would weigh infinitely
				dropped[c] += 1
				continue
			}
			sources[k] = source
			targets[k] = target
		}
	})
	for _, count := range dropped {
		self.droppedTermCount += count
	}

	// counting sort by source, keeping the input order within a source
	starts := make([]uint32, n+1)
	for k := range terms {
		if sources[k] != invalid {
			starts[sources[k]+1] += 1
		}
	}
	for i := 0; i < n; i += 1 {
		starts[i+1] += starts[i]
	}
	sorted := make([]problemTerm, starts[n])
	next := slices.Clone(starts[:n])
	for k := range terms {
		source := sources[k]
		if source == invalid {
			continue
		}
		sorted[next[source]] = problemTerm{
			source:  source,
			target:  targets[k],
			rttMs:   terms[k].RttMs,
			samples: uint16(min(terms[k].Samples, math.MaxUint16)),
		}
		next[source] += 1
	}
	sources = nil
	targets = nil
	next = nil

	// Within each source, by target; of repeats of an ordered pair the one
	// with the most samples comes first, then the shorter round trip, and is
	// the one kept, so the choice does not depend on the input order.
	repeats := make([]int, self.chunks(n))
	self.parallel(len(repeats), func(c int) {
		start, end := self.chunkRange(c, n)
		for i := start; i < end; i += 1 {
			own := sorted[starts[i]:starts[i+1]]
			slices.SortFunc(own, func(a problemTerm, b problemTerm) int {
				if c := cmp.Compare(a.target, b.target); c != 0 {
					return c
				}
				if c := cmp.Compare(b.samples, a.samples); c != 0 {
					return c
				}
				return cmp.Compare(a.rttMs, b.rttMs)
			})
			for j := 1; j < len(own); j += 1 {
				if own[j].target == own[j-1].target {
					repeats[c] += 1
				}
			}
		}
	})
	repeatCount := 0
	for _, count := range repeats {
		repeatCount += count
	}
	self.droppedTermCount += repeatCount

	// drop the repeats
	self.terms = sorted
	self.sourceStarts = make([]uint32, n+1)
	if 0 < repeatCount {
		self.terms = make([]problemTerm, 0, len(sorted)-repeatCount)
		for i := 0; i < n; i += 1 {
			for j := starts[i]; j < starts[i+1]; j += 1 {
				if starts[i] < j && sorted[j].target == sorted[j-1].target {
					continue
				}
				self.terms = append(self.terms, sorted[j])
			}
			self.sourceStarts[i+1] = uint32(len(self.terms))
		}
	} else {
		copy(self.sourceStarts, starts)
	}

	// the terms toward each node, in term order
	self.incomingStarts = make([]uint32, n+1)
	for k := range self.terms {
		self.incomingStarts[self.terms[k].target+1] += 1
	}
	for i := 0; i < n; i += 1 {
		self.incomingStarts[i+1] += self.incomingStarts[i]
	}
	self.incoming = make([]uint32, len(self.terms))
	fill := slices.Clone(self.incomingStarts[:n])
	for k := range self.terms {
		target := self.terms[k].target
		self.incoming[fill[target]] = uint32(k)
		fill[target] += 1
	}
}
