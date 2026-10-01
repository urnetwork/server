// The scale tests of connect/GEOMAP.md §5.8: a parallel solve the same bit for
// bit on any number of workers, the reputation's coverage at fleet sizes, and
// the solver's cost measured at 10k and 100k nodes and extrapolated to the
// target of a million extenders and a million providers, with the full target
// itself behind GEOMAP_SCALE=1.

package solve

import (
	"context"
	"fmt"
	"math"
	mathrand "math/rand"
	"os"
	"reflect"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

// The budgets the extrapolated target run is held to (§5.8 item 1): a typical
// run on the taskworker host's cores within the time, and the solve's memory
// within the bytes. A 96-core speed-up cannot be measured on a smaller host,
// so the time is extrapolated at a stated parallel efficiency -- the share of
// ideal the sweeps reach on the budget's cores -- which each host checks on
// its own cores whenever it is quiet enough to measure one.
type scaleBudget struct {
	seconds    float64
	cores      int
	efficiency float64
	bytes      int64
}

// The budgets of §5.8: 600 s on 96 cores at half of ideal, 8 GiB.
func defaultScaleBudget() scaleBudget {
	return scaleBudget{
		seconds:    600,
		cores:      96,
		efficiency: 0.5,
		bytes:      8 * 1024 * 1024 * 1024,
	}
}

// The continents a fleet is spread over, as latitude and longitude boxes.
var scaleContinents = [][4]float64{
	{36, 60, -10, 30},
	{25, 50, -125, -70},
	{-35, 5, -75, -40},
	{10, 45, 70, 140},
	{-30, 30, 10, 40},
	{-40, -15, 115, 155},
}

// A generated fleet: extenders, each pinging extenderPeers extenders of its own
// continent, and providers, every other one of which pings providerPeers
// extenders of its continent -- the half of the providers that reconnect in a
// day. Each source's peers are drawn from its continent in fixed strides, so
// they are distinct; the round trips are the truth's with 0.3 ms of noise and
// two samples; each genesis is moved from its truth by up to its radius. The
// terms are generated in parallel chunks, each from its own seeded source, so
// the fleet is the same on any number of cores. Returns the nodes, the terms
// and each node's truth.
func newScaleFleet(extenders int, providers int, extenderPeers int, providerPeers int, seed int64, settings *Settings) ([]Node, []Term, []LatLon) {
	continentCount := len(scaleContinents)
	random := mathrand.New(mathrand.NewSource(seed))
	nodes := make([]Node, extenders+providers)
	truths := make([]LatLon, extenders+providers)
	radiiKm := []float64{5, 10, 20, 50, 100, 200, 500}
	for i := range nodes {
		kind, index, count := "e", i, extenders
		expectedPeers := extenderPeers
		if extenders <= i {
			kind, index, count = "p", i-extenders, providers
			expectedPeers = providerPeers
		}
		continent := scaleContinents[index*continentCount/count]
		truth := LatLon{
			Latitude:  continent[0] + random.Float64()*(continent[1]-continent[0]),
			Longitude: continent[2] + random.Float64()*(continent[3]-continent[2]),
		}
		radiusKm := radiiKm[random.Intn(len(radiiKm))]
		bearing := random.Float64() * 2 * math.Pi
		shiftKm := random.Float64() * radiusKm
		truths[i] = truth
		nodes[i] = Node{
			Id:            fmt.Sprintf("%s%07d", kind, index),
			Genesis:       Move(truth, Offset{NorthKm: shiftKm * math.Cos(bearing), EastKm: shiftKm * math.Sin(bearing)}),
			RadiusKm:      radiusKm,
			ExpectedPeers: expectedPeers,
		}
	}

	// the terms of source i start at termStarts[i]
	termStarts := make([]int, extenders+providers+1)
	for i := range nodes {
		peers := extenderPeers
		if extenders <= i {
			peers = 0
			if (i-extenders)%2 == 0 {
				peers = providerPeers
			}
		}
		termStarts[i+1] = termStarts[i] + peers
	}
	terms := make([]Term, termStarts[len(nodes)])
	const chunk = 16 * 1024
	chunkCount := (len(nodes) + chunk - 1) / chunk
	var group sync.WaitGroup
	var nextChunk atomic.Int64
	for w := 0; w < runtime.GOMAXPROCS(0); w += 1 {
		group.Add(1)
		go func() {
			defer group.Done()
			for {
				c := int(nextChunk.Add(1) - 1)
				if chunkCount <= c {
					return
				}
				chunkRandom := mathrand.New(mathrand.NewSource(seed*1000003 + int64(c)))
				for i := c * chunk; i < min(len(nodes), (c+1)*chunk); i += 1 {
					peers := termStarts[i+1] - termStarts[i]
					if peers == 0 {
						continue
					}
					// the extenders of the source's continent
					index, count := i, extenders
					if extenders <= i {
						index, count = i-extenders, providers
					}
					continent := index * continentCount / count
					first := continent * extenders / continentCount
					size := (continent+1)*extenders/continentCount - first
					stride := size / (peers + 1)
					for j := 0; j < peers; j += 1 {
						// distinct by stride, and never the source itself
						offset := 1 + j*stride + chunkRandom.Intn(stride)
						target := first
						if i < extenders {
							target += (i - first + offset) % size
						} else {
							target += offset % size
						}
						rttMs := DistanceKm(truths[i], truths[target])/settings.KmPerMs + settings.OverheadMs
						terms[termStarts[i]+j] = Term{
							Source:  nodes[i].Id,
							Target:  nodes[target].Id,
							RttMs:   max(0, rttMs+0.3*chunkRandom.NormFloat64()),
							Samples: 2,
						}
					}
				}
			}
		}()
	}
	group.Wait()
	return nodes, terms, truths
}

// A containment that is a pure function of the point, so any worker may call
// it at once: a node of the "west" region east of 10°E is outside it by how
// far.
type scaleContainment struct{}

// Implements Containment.
func (self scaleContainment) RegionHinge(p LatLon, regionKey string) float64 {
	if regionKey != "west" {
		return 0
	}
	return max(0, (p.Longitude-10)*equatorKmPerDegree*math.Cos(p.Latitude*math.Pi/180))
}

// Implements Containment.
func (self scaleContainment) CountryHinge(p LatLon, countryKey string) float64 {
	return 0
}

// A parallel solve equals the single-worker solve bit for bit (§5.8 item 1):
// every node's step and every sum is fixed by the chunks, not the workers.
// The chunks are small here so that even this fleet is many of them, and the
// containment and the refusals are on so that every parallel path is taken.
func TestSolveIsBitIdenticalOnAnyWorkerCount(t *testing.T) {
	settings := DefaultSettings()
	nodes, terms, _ := newScaleFleet(600, 600, 64, 16, 5, settings)
	for i := range nodes {
		// the nodes west of the line, which pay only when a step crosses it
		if nodes[i].Genesis.Longitude < 10 {
			nodes[i].RegionKey = "west"
		}
	}
	nodeIdRefusals := map[string]Refusals{}
	for i := 0; i < len(nodes); i += 7 {
		nodeIdRefusals[nodes[i].Id] = Refusals{AsPinger: i % 5, PingsAsPinger: 40, AsTarget: i % 3, PingsAsTarget: 40}
	}
	var reference *Result
	for _, workers := range []int{1, 2, 3, 7, runtime.GOMAXPROCS(0)} {
		workerSettings := DefaultSettings()
		workerSettings.Workers = workers
		workerSettings.ParallelChunk = 64
		workerSettings.MaxIterations = 30
		result := Solve(nodes, terms, nodeIdRefusals, scaleContainment{}, workerSettings)
		if reference == nil {
			reference = result
			t.Logf("%d nodes, %d terms: sweeps %v, residual %.6f km, excluded %d", len(nodes), result.TermCount, result.Sweeps, result.ResidualKm, len(result.Excluded))
			continue
		}
		if !reflect.DeepEqual(result, reference) {
			for i := range result.Nodes {
				if result.Nodes[i] != reference.Nodes[i] {
					t.Fatalf("%d workers: %s differs from one worker's: %+v, want %+v", workers, result.Nodes[i].Id, result.Nodes[i], reference.Nodes[i])
				}
			}
			t.Fatalf("%d workers: the result differs from one worker's", workers)
		}
	}
}

// A population where every source measured exactly its expected sample shows
// no coverage penalty at any fleet size (§5.8 item 6).
func TestReputationNoCoveragePenaltyAtFleetSize(t *testing.T) {
	settings := DefaultSettings()
	for _, sources := range []int{1000, 10 * 1000, 100 * 1000} {
		nodes, terms, _ := newScaleFleet(sources/2, sources/2, 64, 16, int64(sources), settings)
		problem := newProblem(nodes, terms, nil, nil, settings)
		scores := problem.score()
		measured := 0
		for i := range problem.nodes {
			if !scores.has[statisticCoverage][i] {
				continue
			}
			measured += 1
			if scores.values[statisticCoverage][i] != 1 || scores.z[statisticCoverage][i] != 0 {
				t.Fatalf("%d sources: %s coverage %v, z %v", sources, problem.nodes[i].Id, scores.values[statisticCoverage][i], scores.z[statisticCoverage][i])
			}
		}
		t.Logf("%d sources: %d measured, every coverage 1 and every coverage z 0", sources, measured)
		if measured == 0 {
			t.Fatalf("%d sources: no source measured", sources)
		}
	}
}

// Seconds per sweep of a solve of the fleet on a number of workers, as wall
// time and as the process's processor time: the difference between runs of
// many sweeps and of one, which takes out the setup, with a step tolerance
// nothing meets so that every sweep runs. Each is the best of a few runs,
// since other work on the host can only slow one.
func scaleSweepSeconds(nodes []Node, terms []Term, workers int, sweeps int) (float64, float64) {
	const repeats = 2
	processorSeconds := func() float64 {
		var usage syscall.Rusage
		syscall.Getrusage(syscall.RUSAGE_SELF, &usage)
		return float64(usage.Utime.Sec+usage.Stime.Sec) + float64(usage.Utime.Usec+usage.Stime.Usec)/1e6
	}
	solveSeconds := func(maxIterations int) (float64, float64) {
		settings := DefaultSettings()
		settings.Workers = workers
		settings.ReputationRounds = 1
		settings.MaxIterations = maxIterations
		settings.MinStepKm = 1e-12
		settings.StagnationSweeps = 0
		bestWall, bestProcessor := math.Inf(1), math.Inf(1)
		for r := 0; r < repeats; r += 1 {
			start := time.Now()
			startProcessor := processorSeconds()
			Solve(nodes, terms, nil, nil, settings)
			bestWall = min(bestWall, time.Since(start).Seconds())
			bestProcessor = min(bestProcessor, processorSeconds()-startProcessor)
		}
		return bestWall, bestProcessor
	}
	manyWall, manyProcessor := solveSeconds(1 + sweeps)
	oneWall, oneProcessor := solveSeconds(1)
	return max(0, manyWall-oneWall) / float64(sweeps), max(0, manyProcessor-oneProcessor) / float64(sweeps)
}

// The parallelism this host gives processor-bound work right now: the speed-up
// on a number of workers of a kernel with no serial part at all, a fixed
// amount of arithmetic split evenly among them. On an idle host it is the
// worker count; on a busy one, what is left of it. Best of a few runs.
func scaleHostParallelism(workers int) float64 {
	const totalSteps = 400 * 1000 * 1000
	kernelSeconds := func(workers int) float64 {
		best := math.Inf(1)
		for r := 0; r < 2; r += 1 {
			var group sync.WaitGroup
			results := make([]float64, workers)
			start := time.Now()
			for w := 0; w < workers; w += 1 {
				group.Add(1)
				go func() {
					defer group.Done()
					x := float64(w)
					for j := 0; j < totalSteps/workers; j += 1 {
						x = x*0.999999 + 1e-9
					}
					results[w] = x
				}()
			}
			group.Wait()
			best = min(best, time.Since(start).Seconds())
			runtime.KeepAlive(results)
		}
		return best
	}
	return kernelSeconds(1) / kernelSeconds(workers)
}

// The bytes a solve holds for a problem, by count: every array the problem
// and its result keep, at its element size, and the two maps from ids at an
// estimated 48 bytes an entry. A count, unlike a heap reading, does not depend
// on when the collector last ran.
func scaleProblemBytes(problem *problem) (int64, int64) {
	n := int64(len(problem.nodes))
	termBytes := int64(len(problem.terms))*int64(unsafe.Sizeof(problemTerm{})) +
		int64(len(problem.incoming))*4
	nodeBytes := n*int64(unsafe.Sizeof(Node{})) +
		// nodeIdIndexes, and the result's own index
		2*n*48 +
		// sourceStarts, incomingStarts, resultOrder, colorNodes, componentNodes
		5*n*4 +
		n*int64(unsafe.Sizeof(tangentFrame{})) +
		// genesisWeights, regionHinges, countryHinges, damping, q,
		// stepLengths, trialRegionHinges, trialCountryHinges
		8*n*8 +
		// pinned, regionEnabled, countryEnabled, excluded
		4*n +
		// corrections, moves, trialCorrections; positions, trialPositions
		3*n*int64(unsafe.Sizeof(Offset{})) + 2*n*int64(unsafe.Sizeof(vec3{})) +
		// the fixed statistics, and a scoring: values, has and z of each
		// statistic, q and excluded
		int64(statisticCount)*n*(8+1) + int64(statisticCount)*n*(8+1+8) + n*(8+1) +
		// the result's nodes
		n*int64(unsafe.Sizeof(NodeResult{}))
	return termBytes, nodeBytes
}

// The solver's cost measured at 10k and 100k nodes and extrapolated to the
// target (§5.8 item 1): 72 million terms and two million nodes. A sweep's
// processor time on one worker is fitted as a·terms + b·nodes from two fleets
// of 100k nodes that differ only in how many peers each source pings; the
// typical sweep count is measured on the 10k fleet; and the run on the
// budget's cores is extrapolated at the budget's parallel efficiency. The
// extrapolated typical run must fit the budget's time and the problem's bytes
// at the target its memory. The speed-up across this host's cores, each read
// against the parallelism the host gave a kernel with no serial part at the
// time, is reported, and must reach the budget's efficiency whenever the host
// is quiet enough to measure it.
func TestSolveScaleExtrapolatesWithinBudget(t *testing.T) {
	budget := defaultScaleBudget()
	settings := DefaultSettings()
	const targetTerms = 72 * 1000 * 1000
	const targetNodes = 2 * 1000 * 1000
	const measuredSweeps = 3

	// the typical run, on the 10k fleet
	smallNodes, smallTerms, _ := newScaleFleet(5*1000, 5*1000, 64, 16, 1, settings)
	smallResult := Solve(smallNodes, smallTerms, nil, nil, settings)
	typicalSweeps := 0
	for _, sweeps := range smallResult.Sweeps {
		typicalSweeps += sweeps
	}
	_, smallSeconds := scaleSweepSeconds(smallNodes, smallTerms, 1, measuredSweeps)
	t.Logf("10k nodes, %d terms: a run takes %v sweeps (%d); one worker %.3f processor s a sweep, %.0f ns a term", smallResult.TermCount, smallResult.Sweeps, typicalSweeps, smallSeconds, 1e9*smallSeconds/float64(smallResult.TermCount))

	// The per-term and per-node cost, from two 100k fleets, in processor time
	// on one worker, which other work on the host inflates far less than wall
	// time.
	nodes, terms, _ := newScaleFleet(50*1000, 50*1000, 64, 16, 2, settings)
	sparseNodes, sparseTerms, _ := newScaleFleet(50*1000, 50*1000, 16, 4, 3, settings)
	denseWallSeconds, denseSeconds := scaleSweepSeconds(nodes, terms, 1, measuredSweeps)
	_, sparseSeconds := scaleSweepSeconds(sparseNodes, sparseTerms, 1, measuredSweeps)
	denseTermCount := float64(len(terms))
	sparseTermCount := float64(len(sparseTerms))
	nodeCount := float64(len(nodes))
	// denseSeconds = a·denseTerms + b·nodes, sparseSeconds = a·sparseTerms + b·nodes
	secondsPerTerm := (denseSeconds - sparseSeconds) / (denseTermCount - sparseTermCount)
	secondsPerNode := max(0, (denseSeconds-secondsPerTerm*denseTermCount)/nodeCount)
	t.Logf("100k nodes: %.0f terms %.3f s a sweep, %.0f terms %.3f s a sweep, on one worker: %.0f ns a term and %.0f ns a node",
		denseTermCount, denseSeconds, sparseTermCount, sparseSeconds, 1e9*secondsPerTerm, 1e9*secondsPerNode)

	// The speed-up across this host's cores, each measured against the
	// parallelism the host gave a kernel with no serial part at the time: the
	// ratio is what the solver would reach with the cores to itself.
	cores := runtime.GOMAXPROCS(0)
	speedUps := []string{}
	hostParallelism := 0.0
	ownEfficiency := 0.0
	for _, workers := range []int{2, 4, 8, cores} {
		if cores < workers || (workers == cores && workers == 8) {
			continue
		}
		wallSeconds, _ := scaleSweepSeconds(nodes, terms, workers, measuredSweeps)
		speedUp := denseWallSeconds / wallSeconds
		available := min(float64(workers), scaleHostParallelism(workers))
		if workers == cores {
			hostParallelism = available
		}
		ownSpeedUp := min(float64(workers), speedUp*float64(workers)/available)
		if workers == cores {
			ownEfficiency = ownSpeedUp / float64(workers)
		}
		speedUps = append(speedUps, fmt.Sprintf("%d workers ×%.2f (the host gave ×%.2f, so ×%.2f on its own)", workers, speedUp, available, ownSpeedUp))
	}
	budgetSpeedUp := budget.efficiency * float64(budget.cores)
	// the host's load averages, for the record the speed-up is read against
	hostLoad := "unknown"
	if loadBytes, err := os.ReadFile("/proc/loadavg"); err == nil {
		hostLoad = string(loadBytes)
	}
	t.Logf("speed-up on the 100k fleet (host load %s): %v; %.0f%% of ideal on its %d cores, against the budget's %.0f%% on %d (×%.0f)",
		hostLoad, speedUps, 100*ownEfficiency, cores, 100*budget.efficiency, budget.cores, budgetSpeedUp)

	// bytes, and the setup
	problem := newProblem(nodes, terms, nil, nil, settings)
	termBytes, nodeBytes := scaleProblemBytes(problem)
	bytesPerTerm := float64(termBytes) / denseTermCount
	bytesPerNode := float64(nodeBytes) / nodeCount
	setupSettings := DefaultSettings()
	setupSettings.Workers = 1
	start := time.Now()
	newProblem(nodes, terms, nil, nil, setupSettings)
	setupSeconds := time.Since(start).Seconds()
	t.Logf("the problem holds %.1f bytes a term and %.0f a node; built in %.2f s on one worker", bytesPerTerm, bytesPerNode, setupSeconds)

	targetSweepSeconds := secondsPerTerm*targetTerms + secondsPerNode*targetNodes
	// the setup counted as if none of it ran in parallel
	targetSetupSeconds := setupSeconds * targetTerms / denseTermCount
	typicalSeconds := targetSetupSeconds + float64(typicalSweeps)*targetSweepSeconds/budgetSpeedUp
	worstSeconds := targetSetupSeconds + float64(settings.ReputationRounds*settings.MaxIterations)*targetSweepSeconds/budgetSpeedUp
	targetBytes := bytesPerTerm*targetTerms + bytesPerNode*targetNodes
	t.Logf("target (%d terms, %d nodes): %.1f s a sweep on one worker, %.2f s on %d cores; a typical run of %d sweeps %.0f s and the worst of %d %.0f s, with the setup %.0f s; %.2f GiB",
		targetTerms, targetNodes, targetSweepSeconds, targetSweepSeconds/budgetSpeedUp, budget.cores, typicalSweeps, typicalSeconds,
		settings.ReputationRounds*settings.MaxIterations, worstSeconds, targetSetupSeconds, targetBytes/(1024*1024*1024))
	if !(typicalSeconds < budget.seconds) {
		t.Fatalf("a typical target run is %.0f s on %d cores, over the %.0f s budget", typicalSeconds, budget.cores, budget.seconds)
	}
	// The budget's efficiency, checked on this host's own cores: only when
	// the host gives even a kernel with no serial part most of them, since a
	// busy host measures its load, not the solve.
	if 0.8*float64(cores) <= hostParallelism && ownEfficiency < budget.efficiency {
		t.Fatalf("the sweeps reach %.0f%% of ideal on %d cores, under the budget's %.0f%%", 100*ownEfficiency, cores, 100*budget.efficiency)
	}
	if hostParallelism < 0.8*float64(cores) {
		t.Logf("the host gave a perfectly parallel kernel only ×%.2f of its %d cores, too busy to check the efficiency", hostParallelism, cores)
	}
	if !(targetBytes < float64(budget.bytes)) {
		t.Fatalf("the target problem is %.2f GiB, over the %.2f GiB budget", targetBytes/(1024*1024*1024), float64(budget.bytes)/(1024*1024*1024))
	}
}

// The full target, a million extenders and a million providers, generated and
// solved on this host, with the budget's time scaled to its cores (§5.8 item
// 1, gated by GEOMAP_SCALE=1). The heap is sampled while the solve runs; the
// solve's bytes are the most the heap held beyond what the fleet itself held
// before the solve.
func TestSolveFullTargetOnThisHost(t *testing.T) {
	if os.Getenv("GEOMAP_SCALE") != "1" {
		t.Skip("the full target runs with GEOMAP_SCALE=1")
	}
	budget := defaultScaleBudget()
	cores := runtime.GOMAXPROCS(0)
	budgetSeconds := budget.seconds * float64(budget.cores) / float64(cores)
	settings := DefaultSettings()

	start := time.Now()
	nodes, terms, _ := newScaleFleet(1000*1000, 1000*1000, 64, 16, 7, settings)
	generateSeconds := time.Since(start).Seconds()
	termCount := len(terms)
	runtime.GC()
	var baseline runtime.MemStats
	runtime.ReadMemStats(&baseline)

	ctx, cancel := context.WithCancel(context.Background())
	peakHeapBytes := baseline.HeapInuse
	sampled := make(chan struct{})
	go func() {
		defer close(sampled)
		for {
			var stats runtime.MemStats
			runtime.ReadMemStats(&stats)
			peakHeapBytes = max(peakHeapBytes, stats.HeapInuse)
			select {
			case <-ctx.Done():
				return
			case <-time.After(250 * time.Millisecond):
			}
		}
	}()
	start = time.Now()
	result := Solve(nodes, terms, nil, nil, settings)
	solveSeconds := time.Since(start).Seconds()
	cancel()
	<-sampled

	solveBytes := int64(peakHeapBytes) - int64(baseline.HeapInuse)
	published := 0
	for i := range result.Nodes {
		if Publishable(&result.Nodes[i], settings) {
			published += 1
		}
	}
	t.Logf("%d nodes and %d terms generated in %.0f s; solved in %.0f s on %d cores (sweeps %v, converged %v, stagnated %v), %d publishable, refused %+v, residual %.2f km (genesis %.2f km); the fleet held %.2f GiB, the solve %.2f GiB more at most",
		len(nodes), termCount, generateSeconds, solveSeconds, cores, result.Sweeps, result.Converged, result.Stagnated, published, result.PublishRefusals, result.ResidualKm, result.GenesisResidualKm,
		float64(baseline.HeapInuse)/(1024*1024*1024), float64(solveBytes)/(1024*1024*1024))
	if !(solveSeconds < budgetSeconds) {
		t.Fatalf("the full target took %.0f s on %d cores, over the budget's %.0f s scaled to them", solveSeconds, cores, budgetSeconds)
	}
	if !(solveBytes < budget.bytes) {
		t.Fatalf("the solve held %.2f GiB, over the %.2f GiB budget", float64(solveBytes)/(1024*1024*1024), float64(budget.bytes)/(1024*1024*1024))
	}
}
