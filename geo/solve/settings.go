// The solver's settings: every number of connect/GEOMAP.md §5, with the
// defaults the acceptance tests of §5.6 hold to, and the genesis radii the
// derive job builds its nodes with.

package solve

// How one source's samples toward one target become the round trip of their
// term (§5.1).
type EdgeAggregate int

const (
	// The median of the samples: unbiased under symmetric noise, and it still
	// shrugs off the odd queued sample. The default (D17).
	EdgeAggregateMedian EdgeAggregate = 0
	// The minimum, the classic propagation floor: queueing only ever adds
	// delay, so on a queue-dominated path the least sample is nearest the
	// wire. It is biased low under symmetric noise, which the acceptance tests
	// of §5.6 measure.
	EdgeAggregateMin EdgeAggregate = 1
)

// The most precise level a genesis location was placed at.
type GenesisLevel int

const (
	GenesisLevelCountry GenesisLevel = 1
	GenesisLevelRegion  GenesisLevel = 2
	GenesisLevelCity    GenesisLevel = 3
)

// Every number of connect/GEOMAP.md §5. The derive job calibrates KmPerMs,
// OverheadMs and the two containment weights in one pass (§5.4); the rest are
// the defaults the acceptance tests of §5.6 hold to.
type Settings struct {
	// Implied distance d̂ = KmPerMs · max(0, rtt − OverheadMs) (§5.1). 100 km
	// per ms of round trip is about two thirds of c one way, the speed of
	// light in fibre, where c in vacuum would overstate what a path can do.
	KmPerMs float64
	// fixed handling time inside every round trip
	OverheadMs    float64
	EdgeAggregate EdgeAggregate

	// Genesis accuracy radii (§5.1) for the genesis sources that carry none of
	// their own: a fresh egress probe, whose location is either city confident
	// or not, and a lookup row written before the radius was recorded, by the
	// level it was placed at.
	ProbedCityRadiusKm  float64
	ProbedOtherRadiusKm float64
	CityRadiusKm        float64
	RegionRadiusKm      float64
	CountryRadiusKm     float64

	// The objective (§5.2). The genesis weight is 1/max(r, GenesisMinRadiusKm)²
	// and a ping's is q·n/max(d̂, DistanceFloorKm)²: a squared relative error,
	// with the floor keeping a very short path from outweighing everything.
	GenesisMinRadiusKm float64
	DistanceFloorKm    float64
	// weights of the squared region and country hinges, per km²
	LambdaRegion  float64
	LambdaCountry float64
	// Huber loss on the ping terms (D9): beyond a relative residual
	// |d − d̂|/max(d̂, DistanceFloorKm) of HuberRelative a ping costs linearly,
	// so one wildly inflated ping cannot drag a node.
	Huber         bool
	HuberRelative float64
	// Asymmetric ping terms (D9): a round trip can overstate a distance
	// (detours, queues) but not understate it, so a ping whose round trip is
	// longer than the geometry needs (d < d̂) weighs AsymmetricSlackWeight of
	// one that is shorter than the geometry allows.
	Asymmetric            bool
	AsymmetricSlackWeight float64

	// The solver (§5.3): block Gauss–Newton with Levenberg–Marquardt damping.
	// MaxIterations caps the sweeps of one solve, and a solve stops early once
	// no node's step is as long as MinStepKm, or once the objective has fallen
	// by no more than StagnationRelativeImprovement of itself over the last
	// StagnationSweeps sweeps: past that the sweeps are only sliding weakly
	// determined nodes along directions the objective hardly sees, which the
	// publish gate's PublishMaxLastStepKm then refuses. StagnationSweeps 0
	// turns the stagnation stop off.
	MaxIterations                 int
	MinStepKm                     float64
	StagnationSweeps              int
	StagnationRelativeImprovement float64
	// Each node's damping μ starts at InitialDamping, multiplies by
	// DampingDecrease after a step that lowers the objective and by
	// DampingIncrease after one that does not.
	InitialDamping  float64
	DampingIncrease float64
	DampingDecrease float64
	// the step of the central differences that give each residual's partial
	// derivatives with respect to a node's north and east offsets
	DerivativeStepKm float64
	// The goroutines a solve runs on; 0 is as many as the Go scheduler runs at
	// once. ParallelChunk is the nodes (or terms) one of them takes at a time.
	// The chunks, not the workers, fix the order every partial sum is combined
	// in, so a solve gives the same answer bit for bit on any number of
	// workers; a different chunk size is a different order, and may differ in
	// the last bits.
	Workers       int
	ParallelChunk int

	// The samples of one ordered pair the aggregator keeps for its median: the
	// first this many, then a uniform sample of them all (reservoir sampling),
	// so its memory is bounded by the pairs, not the pings.
	PairSampleReservoir int

	// Publishing (§5.4, D11): a correction is published only with at least
	// MinDerivePings co-signed pings to at least MinDerivePeers distinct peers,
	// and only for a node that moved no more than PublishMaxLastStepKm in the
	// solve's last sweep -- one still moving when the solve stopped has not
	// arrived anywhere yet. Two peers fix a position only up to the line
	// through them, where either of two mirror points explains the pings
	// equally; a third peer off that line breaks the tie. The derive job and
	// the monitor's thin-evidence share read the peer gate from here.
	MinDerivePings       int
	MinDerivePeers       int
	PublishMaxLastStepKm float64

	// Source reputation (§5.5): ReputationRounds solves, each weighted by the
	// scores of the one before. A source's weight is
	// clamp(1/(1 + z_max²), MinQ, 1), and a source beyond ExcludeZ on any
	// statistic but coverage is left out of the solve.
	ReputationRounds int
	MinQ             float64
	ExcludeZ         float64
	// The least population spread each z-score divides by. A population
	// tighter than this has no spread worth standardizing: without a floor,
	// one refusal among sources that were never refused, or rounding among
	// residuals that are all zero, would be many sigma out.
	ScatterFloorKm float64
	BiasFloorKm    float64
	CoverageFloor  float64
	RefusalFloor   float64
}

// The settings of connect/GEOMAP.md §5.
func DefaultSettings() *Settings {
	return &Settings{
		KmPerMs:       100,
		OverheadMs:    2,
		EdgeAggregate: EdgeAggregateMedian,

		ProbedCityRadiusKm:  25,
		ProbedOtherRadiusKm: 100,
		CityRadiusKm:        25,
		RegionRadiusKm:      100,
		CountryRadiusKm:     500,

		GenesisMinRadiusKm: 5,
		DistanceFloorKm:    20,
		// 10 km over a region line costs 1, what any node pays for moving
		// one accuracy radius from its genesis; the same 1 buys only 2 km
		// over a country line, because GeoLite2's country is far more
		// reliable than its city
		LambdaRegion:          1.0 / (10 * 10),
		LambdaCountry:         1.0 / (2 * 2),
		Huber:                 false,
		HuberRelative:         0.5,
		Asymmetric:            false,
		AsymmetricSlackWeight: 0.25,

		MaxIterations: 100,
		MinStepKm:     0.01,
		// Measured on generated fleets of 10k and 100k nodes: over 20 sweeps a
		// fall of 0.1% stops a run at about half the sweeps the cap allows,
		// with the RMS error unchanged to 0.01% and 2% of the nodes refused
		// as still moving, whom the next run's warm start carries on.
		StagnationSweeps:              20,
		StagnationRelativeImprovement: 1e-3,
		InitialDamping:                1e-3,
		DampingIncrease:               10,
		DampingDecrease:               0.1,
		DerivativeStepKm:              1e-3,
		Workers:                       0,
		ParallelChunk:                 4096,

		PairSampleReservoir: 16,

		MinDerivePings: 3,
		// three, not two (GEOMAP D11, 2026-09-24): at two a node can settle
		// on the mirror of where it is
		MinDerivePeers:       3,
		PublishMaxLastStepKm: 0.01,

		ReputationRounds: 3,
		MinQ:             0.05,
		ExcludeZ:         4,
		ScatterFloorKm:   10,
		BiasFloorKm:      10,
		CoverageFloor:    0.05,
		RefusalFloor:     0.05,
	}
}

// The accuracy radius of a looked-up genesis: the radius GeoLite2 gave with
// the location when its row recorded one (accuracyKm), else the radius for the
// level the location was placed at, for rows written before the radius was
// recorded. accuracyKm is 0 when the row has none.
func (self *Settings) GenesisRadiusKm(accuracyKm float64, level GenesisLevel) float64 {
	// NaN fails the comparison and falls back with a missing radius
	if 0 < accuracyKm {
		return accuracyKm
	}
	switch level {
	case GenesisLevelCity:
		return self.CityRadiusKm
	case GenesisLevelRegion:
		return self.RegionRadiusKm
	default:
		return self.CountryRadiusKm
	}
}

// The accuracy radius of a genesis from a fresh egress probe, which carries no
// radius of its own.
func (self *Settings) ProbedGenesisRadiusKm(cityConfident bool) float64 {
	if cityConfident {
		return self.ProbedCityRadiusKm
	}
	return self.ProbedOtherRadiusKm
}
