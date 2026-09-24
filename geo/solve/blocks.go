// The sweeps of a solve: each node's block Gauss–Newton step, the colors the
// nodes step in, the connected components, and the parallel-tangents line
// search that follows every sweep.

package solve

import (
	"math"
)

// How a solve stopped.
type solveStop int

const (
	// no node's step, nor the line search's move, was as long as MinStepKm
	solveStopStep solveStop = 1
	// the objective fell by no more than StagnationRelativeImprovement of
	// itself over the last StagnationSweeps sweeps
	solveStopStagnation solveStop = 2
	// MaxIterations sweeps ran
	solveStopSweepCap solveStop = 3
)

// Minimizes the objective by block Gauss–Newton with Levenberg–Marquardt
// damping, from the current corrections, and returns the sweeps taken and how
// the solve stopped. Every node's move in the last sweep is left in
// lastStepLengths.
//
// Why blocks suffice: the objective couples two nodes only through the pings
// between them, so with every other node held where it is, the part of the
// objective one node can change is its genesis term, its containment terms
// and its own pings -- a problem in two unknowns, its north and east offsets,
// solved in closed form with no global system, factorization or linear
// algebra library. The genesis term adds w·I to every block's normal matrix,
// so every block is positive definite however few or degenerate its pings: a
// node with one peer, or none, still has a unique step. A step is taken only
// when it lowers the node's part of the objective, which is the only part it
// changes, so the objective falls monotonically.
//
// The sweeps are Gauss–Seidel, run in parallel by color (colorNodes): the
// nodes of one color share no term, so each steps from its neighbours' latest
// positions at the same time as the rest of its color, and the colors run in
// order. That is exactly the sweep one node at a time would make, on any
// number of workers. A Jacobi sweep -- every node stepping at once from the
// positions of the sweep before -- was measured and refused: both ends of a
// stiff pair close the whole gap between them in the same sweep, and on seed
// 13 of the §5.6 continent (TestSolveKeepsStiffPairsApart) a pair 93 km apart
// at the truth and 167 km apart at genesis was carried past each other into a
// wrong arrangement 72 km off, where one node at a time came to within 0.11
// km.
//
// Each sweep after the first is followed by a line search along the
// direction from two iterates back, the parallel-tangents step (lineSearch),
// without which a cluster bound tightly to itself and loosely to its anchors
// drifts home by a fraction of a percent a sweep.
//
// Three things stop a solve. A sweep's largest step under MinStepKm: the
// longest step a node took, or proposed and had to refuse -- its raised
// damping makes its next proposal shorter, which is not convergence -- or
// the longest move of the line search. The objective's stagnation: over the
// last StagnationSweeps sweeps it fell by no more than
// StagnationRelativeImprovement of itself, taken as a fixed-order sum so the
// stop falls on the same sweep on any number of workers. And MaxIterations,
// the backstop. Warm starting from the previous run's corrections carries
// convergence across runs.
func (self *problem) solveBlocks() (int, solveStop) {
	settings := self.settings
	self.parallelNodes(func(i int) {
		self.damping[i] = settings.InitialDamping
		self.lastStepLengths[i] = 0
	})
	self.findComponents()
	n := len(self.nodes)
	// the objective, containment included, as a fixed-order sum
	objective := func() float64 {
		return self.sumNodes(func(i int) float64 {
			correction := self.corrections[i]
			cost := self.genesisWeights[i]*(correction.NorthKm*correction.NorthKm+correction.EastKm*correction.EastKm) +
				self.containmentCost(i, self.regionHinges[i], self.countryHinges[i])
			for k := self.sourceStarts[i]; k < self.sourceStarts[i+1]; k += 1 {
				term := &self.terms[k]
				if term.weight == 0 {
					continue
				}
				impliedKm := self.impliedKm(term)
				residual := surfaceKm(self.positions[i], self.positions[term.target]) - impliedKm
				cost += self.pingCost(impliedKm, float64(term.weight), residual)
			}
			return cost
		})
	}
	stagnationSweeps := max(0, settings.StagnationSweeps)
	// the objective at the start and after each sweep
	objectives := []float64{}
	if 0 < stagnationSweeps {
		objectives = append(objectives, objective())
	}
	// the corrections at the start of this sweep and of the one before it
	sweepStart := make([]Offset, n)
	previousSweepStart := make([]Offset, n)
	for sweep := 0; sweep < settings.MaxIterations; sweep += 1 {
		copy(sweepStart, self.corrections)
		for c := 0; c+1 < len(self.colorStarts); c += 1 {
			// a color's nodes are independent, so any split of them among
			// the workers gives the same result
			colorNodes := self.colorNodes[self.colorStarts[c]:self.colorStarts[c+1]]
			pieces := min(len(colorNodes), 4*self.workers)
			self.parallel(pieces, func(piece int) {
				for _, i := range colorNodes[piece*len(colorNodes)/pieces : (piece+1)*len(colorNodes)/pieces] {
					self.step(int(i))
				}
			})
		}
		largestStepKm := self.maxNodes(func(i int) float64 {
			return self.stepLengths[i]
		})
		if 0 < sweep {
			self.parallelNodes(func(i int) {
				self.moves[i] = Offset{
					NorthKm: self.corrections[i].NorthKm - previousSweepStart[i].NorthKm,
					EastKm:  self.corrections[i].EastKm - previousSweepStart[i].EastKm,
				}
			})
			largestStepKm = max(largestStepKm, self.lineSearch())
		}
		copy(previousSweepStart, sweepStart)
		self.parallelNodes(func(i int) {
			self.lastStepLengths[i] = math.Hypot(self.corrections[i].NorthKm-sweepStart[i].NorthKm, self.corrections[i].EastKm-sweepStart[i].EastKm)
		})
		if largestStepKm < settings.MinStepKm {
			return sweep + 1, solveStopStep
		}
		if 0 < stagnationSweeps {
			objectives = append(objectives, objective())
			if stagnationSweeps <= sweep+1 {
				// the objective only ever falls, so this is the fall over
				// the window, never negative
				before := objectives[sweep+1-stagnationSweeps]
				if before-objectives[sweep+1] <= settings.StagnationRelativeImprovement*before {
					return sweep + 1, solveStopStagnation
				}
			}
		}
	}
	return max(0, settings.MaxIterations), solveStopSweepCap
}

// One Gauss–Newton step for one node, with every other node where it is: the
// full step, or the damped one when the full one fails. It leaves
// in stepLengths[i] the length of the step taken, or of a damped step it had
// to refuse and raised its damping for, else 0. Only the node's own entries
// are written, and only its neighbours' positions read, so nodes that share
// no term can step at once.
func (self *problem) step(i int) {
	self.stepLengths[i] = 0
	if self.pinned[i] {
		return
	}
	settings := self.settings
	frame := &self.frames[i]
	correction := self.corrections[i]
	position := self.positions[i]

	// Each residual's partial derivatives with respect to the node's north and
	// east offsets, by central differences at these four points.
	stepKm := settings.DerivativeStepKm
	northPlus := frame.at(Offset{NorthKm: correction.NorthKm + stepKm, EastKm: correction.EastKm})
	northMinus := frame.at(Offset{NorthKm: correction.NorthKm - stepKm, EastKm: correction.EastKm})
	eastPlus := frame.at(Offset{NorthKm: correction.NorthKm, EastKm: correction.EastKm + stepKm})
	eastMinus := frame.at(Offset{NorthKm: correction.NorthKm, EastKm: correction.EastKm - stepKm})

	// The block's normal equations H·δ = −g. The genesis term's residual is
	// the offset itself, with the identity as its derivative: w·|Δ|² exactly,
	// since Move keeps distance from genesis.
	genesisWeight := self.genesisWeights[i]
	// A ping residual's weight in the linearized system: the term's weight
	// times the ratio of the loss's slope to the square's at this residual.
	// With that reweighting each Gauss–Newton step is a descent step on the
	// robust objective, and it is exactly the term's weight without D9.
	linearWeight := func(impliedKm float64, weight float64, residualKm float64) float64 {
		if settings.Asymmetric && residualKm < 0 {
			weight *= settings.AsymmetricSlackWeight
		}
		if settings.Huber {
			relative := math.Abs(residualKm) / self.scaleKm(impliedKm)
			if settings.HuberRelative < relative {
				weight *= settings.HuberRelative / relative
			}
		}
		return weight
	}
	h11, h12, h22 := genesisWeight, 0.0, genesisWeight
	g1, g2 := genesisWeight*correction.NorthKm, genesisWeight*correction.EastKm
	// the node's part of the objective where it is, from the same residuals
	currentCost := genesisWeight*(correction.NorthKm*correction.NorthKm+correction.EastKm*correction.EastKm) +
		self.containmentCost(i, self.regionHinges[i], self.countryHinges[i])
	linearize := func(k uint32) {
		term := &self.terms[k]
		if term.weight == 0 {
			return
		}
		weight := float64(term.weight)
		impliedKm := self.impliedKm(term)
		other := self.positions[term.other(i)]
		residual := surfaceKm(position, other) - impliedKm
		currentCost += self.pingCost(impliedKm, weight, residual)
		j1 := (surfaceKm(northPlus, other) - surfaceKm(northMinus, other)) / (2 * stepKm)
		j2 := (surfaceKm(eastPlus, other) - surfaceKm(eastMinus, other)) / (2 * stepKm)
		termWeight := linearWeight(impliedKm, weight, residual)
		h11 += termWeight * j1 * j1
		h12 += termWeight * j1 * j2
		h22 += termWeight * j2 * j2
		g1 += termWeight * j1 * residual
		g2 += termWeight * j2 * residual
	}
	for k := self.sourceStarts[i]; k < self.sourceStarts[i+1]; k += 1 {
		linearize(k)
	}
	for _, k := range self.incoming[self.incomingStarts[i]:self.incomingStarts[i+1]] {
		linearize(k)
	}

	// A hinge at zero is flat on the side the node is on: it enters the
	// linearization only once it is positive, and a step that crosses into
	// it is priced by the cost comparison below. This is also what keeps the
	// place lookups off the path of every node that is inside its region.
	if self.regionEnabled[i] && 0 < self.regionHinges[i] {
		j1 := (self.regionHingeAt(i, northPlus) - self.regionHingeAt(i, northMinus)) / (2 * stepKm)
		j2 := (self.regionHingeAt(i, eastPlus) - self.regionHingeAt(i, eastMinus)) / (2 * stepKm)
		h11 += settings.LambdaRegion * j1 * j1
		h12 += settings.LambdaRegion * j1 * j2
		h22 += settings.LambdaRegion * j2 * j2
		g1 += settings.LambdaRegion * j1 * self.regionHinges[i]
		g2 += settings.LambdaRegion * j2 * self.regionHinges[i]
	}
	if self.countryEnabled[i] && 0 < self.countryHinges[i] {
		j1 := (self.countryHingeAt(i, northPlus) - self.countryHingeAt(i, northMinus)) / (2 * stepKm)
		j2 := (self.countryHingeAt(i, eastPlus) - self.countryHingeAt(i, eastMinus)) / (2 * stepKm)
		h11 += settings.LambdaCountry * j1 * j1
		h12 += settings.LambdaCountry * j1 * j2
		h22 += settings.LambdaCountry * j2 * j2
		g1 += settings.LambdaCountry * j1 * self.countryHinges[i]
		g2 += settings.LambdaCountry * j2 * self.countryHinges[i]
	}

	// The undamped Gauss–Newton step goes first. Where the block's model is
	// exact -- a genesis term alone is exactly quadratic -- it lands on the
	// minimum, where a damped step only ever closes a fraction 1/(1 + μ) of
	// the way and leaves a remainder no convergence test can tell from done.
	// Only when it fails to lower the cost is the damped step tried, with the
	// node's damping, raised when that fails too and lowered when it does not.
	// Marquardt's damping scales each diagonal entry by 1 + μ, so μ is
	// unitless and the damped step bends from Gauss–Newton toward gradient
	// descent with each axis in its own units.
	// the part of the objective the node's correction changes, with the node
	// at a candidate and every other node where it is
	candidateCost := func(candidate Offset, candidatePosition vec3, regionHinge float64, countryHinge float64) float64 {
		cost := genesisWeight * (candidate.NorthKm*candidate.NorthKm + candidate.EastKm*candidate.EastKm)
		price := func(k uint32) {
			term := &self.terms[k]
			if term.weight == 0 {
				return
			}
			impliedKm := self.impliedKm(term)
			residual := surfaceKm(candidatePosition, self.positions[term.other(i)]) - impliedKm
			cost += self.pingCost(impliedKm, float64(term.weight), residual)
		}
		for k := self.sourceStarts[i]; k < self.sourceStarts[i+1]; k += 1 {
			price(k)
		}
		for _, k := range self.incoming[self.incomingStarts[i]:self.incomingStarts[i+1]] {
			price(k)
		}
		return cost + self.containmentCost(i, regionHinge, countryHinge)
	}
	damping := self.damping[i]
	for _, stepDamping := range []float64{0, damping} {
		// the 2×2 system in closed form
		a11 := h11 * (1 + stepDamping)
		a22 := h22 * (1 + stepDamping)
		determinant := a11*a22 - h12*h12
		if !(0 < determinant) {
			// w·I keeps the block positive definite, so only a non-finite
			// input reaches here
			return
		}
		deltaNorth := -(a22*g1 - h12*g2) / determinant
		deltaEast := -(a11*g2 - h12*g1) / determinant
		stepLengthKm := math.Hypot(deltaNorth, deltaEast)
		if !(0 < stepLengthKm) || math.IsInf(stepLengthKm, 1) {
			return
		}
		candidate := Offset{NorthKm: correction.NorthKm + deltaNorth, EastKm: correction.EastKm + deltaEast}
		candidatePosition := frame.at(candidate)
		candidateRegionHinge := self.regionHingeAt(i, candidatePosition)
		candidateCountryHinge := self.countryHingeAt(i, candidatePosition)
		if candidateCost(candidate, candidatePosition, candidateRegionHinge, candidateCountryHinge) < currentCost {
			self.corrections[i] = candidate
			self.positions[i] = candidatePosition
			self.regionHinges[i] = candidateRegionHinge
			self.countryHinges[i] = candidateCountryHinge
			self.damping[i] = damping * settings.DampingDecrease
			self.stepLengths[i] = stepLengthKm
			return
		}
		if stepDamping != 0 && settings.MinStepKm <= stepLengthKm {
			// A step too short to matter that fails to lower the cost is a
			// node at its minimum, not a model gone nonlinear, so it leaves
			// the damping alone. The floor at the initial damping restarts a
			// damping that many good steps drove to nothing.
			self.damping[i] = max(damping*settings.DampingIncrease, settings.InitialDamping)
			self.stepLengths[i] = stepLengthKm
		}
	}
}

// One ping term's share of the objective at a residual: v·r², with
// the refinements of D9 when they are on. The Huber branch is continuous with
// the square at the threshold and grows linearly past it.
func (self *problem) pingCost(impliedKm float64, weight float64, residualKm float64) float64 {
	settings := self.settings
	if settings.Asymmetric && residualKm < 0 {
		weight *= settings.AsymmetricSlackWeight
	}
	if settings.Huber {
		scaleKm := self.scaleKm(impliedKm)
		relative := math.Abs(residualKm) / scaleKm
		if settings.HuberRelative < relative {
			threshold := settings.HuberRelative
			return weight * scaleKm * scaleKm * (2*threshold*relative - threshold*threshold)
		}
	}
	return weight * residualKm * residualKm
}

// The containment terms of node i at the given hinges, those it has enabled.
func (self *problem) containmentCost(i int, regionHinge float64, countryHinge float64) float64 {
	var cost float64
	if self.regionEnabled[i] {
		cost += self.settings.LambdaRegion * regionHinge * regionHinge
	}
	if self.countryEnabled[i] {
		cost += self.settings.LambdaCountry * countryHinge * countryHinge
	}
	return cost
}

// Node i's region hinge at a position, 0 when it has no region term.
func (self *problem) regionHingeAt(i int, position vec3) float64 {
	if !self.regionEnabled[i] {
		return 0
	}
	return hingeKm(self.containment.RegionHinge(position.latLon(), self.nodes[i].RegionKey))
}

// Node i's country hinge at a position, 0 when it has no country term.
func (self *problem) countryHingeAt(i int, position vec3) float64 {
	if !self.countryEnabled[i] {
		return 0
	}
	return hingeKm(self.containment.CountryHinge(position.latLon(), self.nodes[i].CountryKey))
}

// A containment answer as a hinge. A hinge is a distance, never negative;
// an answer that is not one (negative, NaN, infinite) biases nothing rather
// than poisoning the objective.
func hingeKm(value float64) float64 {
	if 0 < value && !math.IsInf(value, 1) {
		return value
	}
	return 0
}

// Splits the nodes into the connected components of the terms
// that weigh in this round. Nothing in the objective couples two components,
// so one's line search must not move another: a single multiple for all of
// them would let one component's gains pay for throwing another's nodes off
// their minimum, and a node with no weighed terms at all -- its own component
// -- off its genesis. Every weighed term joins nodes of one component, and
// belongs to its source's.
func (self *problem) findComponents() {
	n := len(self.nodes)
	parents := make([]uint32, n)
	for i := range parents {
		parents[i] = uint32(i)
	}
	find := func(i uint32) uint32 {
		for parents[i] != i {
			// halve the path as it is walked
			parents[i] = parents[parents[i]]
			i = parents[i]
		}
		return i
	}
	for k := range self.terms {
		if self.terms[k].weight == 0 {
			continue
		}
		a := find(self.terms[k].source)
		b := find(self.terms[k].target)
		if a != b {
			// the smaller index is the root, so a component is numbered by
			// its first node whatever order its terms join it in
			parents[max(a, b)] = min(a, b)
		}
	}
	// components numbered in order of their first node, nodes in order
	numbers := make([]uint32, n)
	counts := []uint32{}
	for i := 0; i < n; i += 1 {
		root := find(uint32(i))
		if root == uint32(i) {
			numbers[i] = uint32(len(counts))
			counts = append(counts, 0)
		} else {
			numbers[i] = numbers[root]
		}
		counts[numbers[i]] += 1
	}
	self.componentStarts = make([]uint32, len(counts)+1)
	for c, count := range counts {
		self.componentStarts[c+1] = self.componentStarts[c] + count
	}
	self.componentNodes = make([]uint32, n)
	fill := make([]uint32, len(counts))
	copy(fill, self.componentStarts[:len(counts)])
	for i := 0; i < n; i += 1 {
		self.componentNodes[fill[numbers[i]]] = uint32(i)
		fill[numbers[i]] += 1
	}
}

// The parallel-tangents step on every component, taken only where it helps,
// returning the largest distance it moved a node: the multiple of the move
// from two iterates back nearest the minimum along it. The multiples double
// from 1 while the objective falls -- the genesis term guarantees it rises
// again -- and then the vertex of the parabola through the last three is
// tried. The search measures the objective without the containment terms, the
// only part costly to evaluate, then prices the whole objective at the
// multiple it picked, backing off by halves when a hinge makes it rise, so
// the objective never rises. A component larger than a chunk is searched with
// its sums in parallel; the smaller ones are searched each on one worker,
// many at once. Either way every sum is taken in a fixed order.
func (self *problem) lineSearch() float64 {
	// one component's search, with its sums in parallel or on the calling
	// worker alone
	search := func(nodes []uint32, inParallel bool) float64 {
		// work on the component's nodes, in fixed chunks
		each := func(work func(i int)) {
			if !inParallel {
				for _, i := range nodes {
					work(int(i))
				}
				return
			}
			self.parallel(self.chunks(len(nodes)), func(c int) {
				start, end := self.chunkRange(c, len(nodes))
				for _, i := range nodes[start:end] {
					work(int(i))
				}
			})
		}
		// a quantity added up over the component's nodes in a fixed order
		sum := func(value func(i int) float64) float64 {
			if !inParallel {
				var total float64
				for _, i := range nodes {
					total += value(int(i))
				}
				return total
			}
			return self.sum(self.chunks(len(nodes)), func(c int) float64 {
				start, end := self.chunkRange(c, len(nodes))
				var partial float64
				for _, i := range nodes[start:end] {
					partial += value(int(i))
				}
				return partial
			})
		}
		// a node's genesis cost and its own terms' costs, which between them
		// count every weighed term of the component once
		nodeCost := func(i int, corrections []Offset, positions []vec3) float64 {
			correction := corrections[i]
			cost := self.genesisWeights[i] * (correction.NorthKm*correction.NorthKm + correction.EastKm*correction.EastKm)
			for k := self.sourceStarts[i]; k < self.sourceStarts[i+1]; k += 1 {
				term := &self.terms[k]
				if term.weight == 0 {
					continue
				}
				impliedKm := self.impliedKm(term)
				residual := surfaceKm(positions[i], positions[term.target]) - impliedKm
				cost += self.pingCost(impliedKm, float64(term.weight), residual)
			}
			return cost
		}
		// the component at a multiple of its moves, and the objective there
		// without the containment terms
		trial := func(multiple float64) float64 {
			each(func(i int) {
				self.trialCorrections[i] = Offset{
					NorthKm: self.corrections[i].NorthKm + multiple*self.moves[i].NorthKm,
					EastKm:  self.corrections[i].EastKm + multiple*self.moves[i].EastKm,
				}
				self.trialPositions[i] = self.frames[i].at(self.trialCorrections[i])
			})
			return sum(func(i int) float64 {
				return nodeCost(i, self.trialCorrections, self.trialPositions)
			})
		}

		largestMoveKm := 0.0
		for _, i := range nodes {
			largestMoveKm = max(largestMoveKm, self.moves[i].LengthKm())
		}
		if largestMoveKm == 0 {
			return 0
		}
		currentBlindCost := sum(func(i int) float64 {
			return nodeCost(i, self.corrections, self.positions)
		})
		lowerMultiple, lowerCost := 0.0, currentBlindCost
		bestMultiple, bestCost := 0.0, currentBlindCost
		upperMultiple, upperCost := 0.0, currentBlindCost
		for multiple := 1.0; !math.IsInf(multiple, 1); multiple *= 2 {
			cost := trial(multiple)
			if !(cost < bestCost) {
				upperMultiple, upperCost = multiple, cost
				break
			}
			lowerMultiple, lowerCost = bestMultiple, bestCost
			bestMultiple, bestCost = multiple, cost
		}
		if bestMultiple == 0 {
			return 0
		}
		if 0 < upperMultiple {
			// the vertex of the parabola through the bracket, which is a
			// minimum since the middle point is the lowest
			lowerSide := (bestMultiple - lowerMultiple) * (bestCost - upperCost)
			upperSide := (bestMultiple - upperMultiple) * (bestCost - lowerCost)
			if denominator := lowerSide - upperSide; denominator != 0 {
				vertex := bestMultiple - 0.5*((bestMultiple-lowerMultiple)*lowerSide-(bestMultiple-upperMultiple)*upperSide)/denominator
				if lowerMultiple < vertex && vertex < upperMultiple && vertex != bestMultiple {
					if cost := trial(vertex); cost < bestCost {
						bestMultiple = vertex
					}
				}
			}
		}

		currentCost := currentBlindCost + sum(func(i int) float64 {
			return self.containmentCost(i, self.regionHinges[i], self.countryHinges[i])
		})
		for multiple := bestMultiple; 0 < multiple; multiple /= 2 {
			cost := trial(multiple)
			// the moving nodes' hinges at the trial positions
			each(func(i int) {
				self.trialRegionHinges[i] = self.regionHinges[i]
				self.trialCountryHinges[i] = self.countryHinges[i]
				if self.moves[i] != (Offset{}) {
					self.trialRegionHinges[i] = self.regionHingeAt(i, self.trialPositions[i])
					self.trialCountryHinges[i] = self.countryHingeAt(i, self.trialPositions[i])
				}
			})
			containment := sum(func(i int) float64 {
				return self.containmentCost(i, self.trialRegionHinges[i], self.trialCountryHinges[i])
			})
			if cost+containment < currentCost {
				each(func(i int) {
					self.corrections[i] = self.trialCorrections[i]
					self.positions[i] = self.trialPositions[i]
					self.regionHinges[i] = self.trialRegionHinges[i]
					self.countryHinges[i] = self.trialCountryHinges[i]
				})
				return multiple * largestMoveKm
			}
			if multiple/2*largestMoveKm < self.settings.MinStepKm {
				break
			}
		}
		return 0
	}

	componentCount := len(self.componentStarts) - 1
	moved := make([]float64, componentCount)
	smallComponents := []int{}
	for c := 0; c < componentCount; c += 1 {
		nodes := self.componentNodes[self.componentStarts[c]:self.componentStarts[c+1]]
		if self.chunk < len(nodes) {
			moved[c] = search(nodes, true)
		} else {
			smallComponents = append(smallComponents, c)
		}
	}
	self.parallel(len(smallComponents), func(j int) {
		c := smallComponents[j]
		moved[c] = search(self.componentNodes[self.componentStarts[c]:self.componentStarts[c+1]], false)
	})
	largestMoveKm := 0.0
	for _, m := range moved {
		largestMoveKm = max(largestMoveKm, m)
	}
	return largestMoveKm
}
