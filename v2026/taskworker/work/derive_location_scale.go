package work

import (
	"context"
	"fmt"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/geo/solve"
	"github.com/urnetwork/server/v2026/model"
)

// The derive phase at scale (connect/GEOMAP.md §5.3, "Scale"; D26, D27). The
// job does not partition: it reads the day's pings over several concurrent
// cursors, aggregating into per-ordered-pair terms as rows arrive, and the
// solver sweeps in parallel over the host's cores. A planning step projects
// every run from the last run's measured costs against the solve's budgets,
// and the projection is watched (SIGNALS.md §2.19c, "capacity"): it is an
// alert, not a switch.
//
// The cursors split the day by pinger-id hash, so every ordered pair is read
// whole by one cursor, in (create_time, ping_id) order. Each cursor's
// aggregate of a pair is then the aggregate one cursor over the whole day
// would make, and the cursors' terms merge by union: the merged terms are the
// same for any number of cursors.

// The node id strings of one ingest, shared by its cursors, so that every
// cursor names a node by the same string. A target is pinged from every hash
// range, so without the table each cursor's aggregator would keep its own copy
// of every target's id, and every row would build two new strings. Safe for
// concurrent use: a lookup takes one shard's read lock, and a node's first
// lookup its write lock.
type deriveNodeIdTable struct {
	shards []deriveNodeIdTableShard
}

// One shard of the table, holding the parties whose id ends in its bytes.
type deriveNodeIdTableShard struct {
	stateLock      sync.RWMutex
	nodeKeyNodeIds map[deriveNodeKey]string
}

// A table of `shardCount` shards, at least one.
func newDeriveNodeIdTable(shardCount int) *deriveNodeIdTable {
	shards := make([]deriveNodeIdTableShard, max(1, shardCount))
	for i := range shards {
		shards[i].nodeKeyNodeIds = map[deriveNodeKey]string{}
	}
	return &deriveNodeIdTable{
		shards: shards,
	}
}

// The node id of a party (deriveNodeId), the same string for every caller.
func (self *deriveNodeIdTable) nodeId(nodeKind int, id server.Id) string {
	// an id's last byte is random, so it spreads the parties over the shards
	shard := &self.shards[int(id[len(id)-1])%len(self.shards)]
	key := deriveNodeKey{nodeKind: nodeKind, id: id}
	var nodeId string
	var ok bool
	func() {
		shard.stateLock.RLock()
		defer shard.stateLock.RUnlock()
		nodeId, ok = shard.nodeKeyNodeIds[key]
	}()
	if ok {
		return nodeId
	}
	nodeId = deriveNodeId(nodeKind, id)
	func() {
		shard.stateLock.Lock()
		defer shard.stateLock.Unlock()
		if existingNodeId, ok := shard.nodeKeyNodeIds[key]; ok {
			nodeId = existingNodeId
			return
		}
		shard.nodeKeyNodeIds[key] = nodeId
	}()
	return nodeId
}

// One cursor's share of the window's co-signed direct pings: the terms of the
// pairs whose pingers hash into its range, aggregated as the rows arrive with
// the solver's aggregator, which keeps a bounded reservoir of samples per pair
// (solve.Settings.PairSampleReservoir), so a cursor's memory follows its pairs
// and not its rows. Owned by the cursor's goroutine; the node id table is the
// one shared part.
type deriveCursorInputs struct {
	aggregator *solve.Aggregator
	nodeIds    *deriveNodeIdTable
	// the pairs' terms once the rows are done (finish), when the aggregator
	// is released
	terms []solve.Term

	// the rows of a node pinger kind, and those that could be no sample: an
	// unknown pinger kind, or a node's ping to itself
	cosignedPings int
	droppedPings  int
}

// A cursor's empty inputs, naming nodes through the shared table.
func newDeriveCursorInputs(settings *solve.Settings, nodeIds *deriveNodeIdTable) *deriveCursorInputs {
	return &deriveCursorInputs{
		aggregator: solve.NewAggregator(settings),
		nodeIds:    nodeIds,
	}
}

// Takes one co-signed direct ping as a sample of its pair's term. The ping is
// also an attestation its pinger made and its target received, which the
// merge counts from the term's samples; a ping that is no sample attests
// nothing either.
func (self *deriveCursorInputs) addTerm(term *model.NetworkPingTerm) {
	nodeKind, ok := derivePingerNodeKind(term.PingerKind)
	if !ok {
		self.droppedPings += 1
		return
	}
	self.cosignedPings += 1
	source := self.nodeIds.nodeId(nodeKind, term.PingerId)
	target := self.nodeIds.nodeId(model.DerivedLocationNodeKindExtender, term.TargetExtenderId)
	if !self.aggregator.Add(source, target, float64(term.RttMs)) {
		self.droppedPings += 1
	}
}

// Aggregates the cursor's pairs into terms and releases the aggregator, whose
// reservoirs are most of an ingest's memory and are not needed once the terms
// are. Each cursor finishes on its own goroutine, so the aggregation runs in
// parallel too.
func (self *deriveCursorInputs) finish() {
	self.terms = self.aggregator.Terms()
	self.aggregator = nil
}

// Merges the cursors' inputs into the solver's. Every ordered pair was read
// whole by exactly one cursor, so the terms merge by union, ordered by source
// and then target, and are the terms one cursor over the whole window would
// have aggregated. A pair read by two cursors would mean the split no longer
// keeps pairs whole, and its two aggregates could not be combined into one:
// the merge refuses it. The nodes are the terms' sources and targets, and each
// term's samples are attestations its source made and its target received.
// The cursors' terms are moved into the merge, not copied.
func mergeDeriveInputs(cursorInputs []*deriveCursorInputs) *deriveInputs {
	inputs := &deriveInputs{
		nodeKeys: map[string]deriveNodeKey{},
		refusals: map[string]*solve.Refusals{},
	}
	termCount := 0
	for _, cursor := range cursorInputs {
		termCount += len(cursor.terms)
	}
	terms := make([]solve.Term, 0, termCount)
	for _, cursor := range cursorInputs {
		terms = append(terms, cursor.terms...)
		cursor.terms = nil
		inputs.cosignedPings += cursor.cosignedPings
		inputs.droppedPings += cursor.droppedPings
	}
	slices.SortFunc(terms, func(a solve.Term, b solve.Term) int {
		if c := strings.Compare(a.Source, b.Source); c != 0 {
			return c
		}
		return strings.Compare(a.Target, b.Target)
	})
	addNode := func(nodeId string) {
		if _, ok := inputs.nodeKeys[nodeId]; ok {
			return
		}
		key, ok := deriveNodeKeyOf(nodeId)
		if !ok {
			panic(fmt.Errorf("[derive]a term names %q, which is no node id", nodeId))
		}
		inputs.nodeKeys[nodeId] = key
	}
	for i := range terms {
		term := &terms[i]
		if 0 < i && terms[i-1].Source == term.Source && terms[i-1].Target == term.Target {
			panic(fmt.Errorf("[derive]the pair %s to %s was read by two cursors", term.Source, term.Target))
		}
		addNode(term.Source)
		addNode(term.Target)
		inputs.counts(term.Source).PingsAsPinger += term.Samples
		inputs.counts(term.Target).PingsAsTarget += term.Samples
	}
	inputs.terms = terms
	return inputs
}

// Reads the window's pings into the solver's inputs. The co-signed direct
// pings, by far the most rows, are read over DeriveReadCursors concurrent
// cursors split by pinger-id hash range (model.NetworkPingHashRanges), each on
// its own database connection, so the taskworker's pool must hold them beside
// its other work; each cursor aggregates its rows as they arrive and holds no
// row. The refusals and the relayed co-signatures, a small share of the day,
// are counted as they stream from one query each. A cursor that fails cancels
// the others and fails the derivation: a derivation over part of the day is
// no derivation, and the task retries.
func ingestDeriveInputs(
	ctx context.Context,
	settings *solve.Settings,
	jobSettings *DeriveLocationsSettings,
	minCreateTime time.Time,
) *deriveInputs {
	hashRanges := model.NetworkPingHashRanges(jobSettings.DeriveReadCursors)
	// a few shards a cursor keeps the cursors off each other's locks
	nodeIds := newDeriveNodeIdTable(4 * len(hashRanges))
	cursorInputs := make([]*deriveCursorInputs, len(hashRanges))
	cursorCtx, cursorCancel := context.WithCancel(ctx)
	defer cursorCancel()

	var stateLock sync.Mutex
	var failure any
	var wait sync.WaitGroup
	for i, hashRange := range hashRanges {
		cursorInputs[i] = newDeriveCursorInputs(settings, nodeIds)
		wait.Add(1)
		go func() {
			defer wait.Done()
			r := server.HandleError(func() {
				model.GetNetworkPingTermRange(cursorCtx, minCreateTime, hashRange, cursorInputs[i].addTerm)
				cursorInputs[i].finish()
			})
			if r == nil {
				return
			}
			func() {
				stateLock.Lock()
				defer stateLock.Unlock()
				if failure == nil {
					failure = r
				}
			}()
			cursorCancel()
		}()
	}
	wait.Wait()
	if failure != nil {
		panic(failure)
	}
	inputs := mergeDeriveInputs(cursorInputs)
	model.GetNetworkPingRelayedCosignCounts(ctx, minCreateTime, inputs.addRelayedCosigns)
	model.GetNetworkPingRefusals(ctx, minCreateTime, inputs.addRefusal)
	return inputs
}

// Samples the heap while a run ingests and solves, for the run's peak above
// the heap it started from. Heap in use includes garbage not yet collected, so
// the peak is an upper bound on what the run needed, which is the right side
// to err on for a budget. The sampler goroutine and the caller share the peak
// under stateLock.
type deriveMemoryPeak struct {
	baseline uint64

	stateLock sync.Mutex
	peak      uint64

	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once
}

// Starts sampling every `interval` until finish, or until the context ends. A
// non-positive interval samples only at the start and at finish, rather than
// spinning.
func startDeriveMemoryPeak(ctx context.Context, interval time.Duration) *deriveMemoryPeak {
	// the heap as the run finds it, without the garbage of whatever ran
	// before it
	runtime.GC()
	stats := &runtime.MemStats{}
	runtime.ReadMemStats(stats)
	self := &deriveMemoryPeak{
		baseline: stats.HeapAlloc,
		peak:     stats.HeapAlloc,
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
	go func() {
		defer close(self.done)
		if interval <= 0 {
			return
		}
		for {
			select {
			case <-ctx.Done():
				return
			case <-self.stop:
				return
			case <-time.After(interval):
			}
			self.sample()
		}
	}()
	return self
}

// Raises the peak to the heap in use now.
func (self *deriveMemoryPeak) sample() {
	stats := &runtime.MemStats{}
	runtime.ReadMemStats(stats)
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.peak = max(self.peak, stats.HeapAlloc)
	}()
}

// Stops the sampler and returns the peak above the baseline. A second call,
// as a deferred one is, only reads the peak.
func (self *deriveMemoryPeak) finish() int64 {
	self.stopOnce.Do(func() {
		self.sample()
		close(self.stop)
		<-self.done
	})
	var peak uint64
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		peak = self.peak
	}()
	if peak < self.baseline {
		return 0
	}
	return int64(peak - self.baseline)
}

// What one run cost, and the unit costs that split into.
type deriveRunCosts struct {
	terms        int
	nodes        int
	sweeps       int
	cores        int
	solveSeconds float64
	peakBytes    int64

	secondsPerTermSweep float64
	secondsPerNodeSweep float64
	bytesPerTerm        float64
	bytesPerNode        float64
}

// Splits a run's solve time and peak heap into per-unit costs. One run gives
// one sum of term and node costs, so the split keeps the settings' proportion
// between them and scales both to the measurement; a run with nothing to
// measure keeps the settings' costs.
func measureDeriveRunCosts(
	jobSettings *DeriveLocationsSettings,
	terms int,
	nodes int,
	sweeps int,
	cores int,
	solveSeconds float64,
	peakBytes int64,
) *deriveRunCosts {
	costs := &deriveRunCosts{
		terms:               terms,
		nodes:               nodes,
		sweeps:              sweeps,
		cores:               cores,
		solveSeconds:        solveSeconds,
		peakBytes:           peakBytes,
		secondsPerTermSweep: jobSettings.SecondsPerTermSweep,
		secondsPerNodeSweep: jobSettings.SecondsPerNodeSweep,
		bytesPerTerm:        jobSettings.BytesPerTerm,
		bytesPerNode:        jobSettings.BytesPerNode,
	}
	sweepUnits := float64(sweeps) * (float64(terms)*jobSettings.SecondsPerTermSweep + float64(nodes)*jobSettings.SecondsPerNodeSweep)
	if 0 < sweepUnits && 0 < solveSeconds {
		scale := solveSeconds / sweepUnits
		costs.secondsPerTermSweep = scale * jobSettings.SecondsPerTermSweep
		costs.secondsPerNodeSweep = scale * jobSettings.SecondsPerNodeSweep
	}
	byteUnits := float64(terms)*jobSettings.BytesPerTerm + float64(nodes)*jobSettings.BytesPerNode
	if 0 < byteUnits && 0 < peakBytes {
		scale := float64(peakBytes) / byteUnits
		costs.bytesPerTerm = scale * jobSettings.BytesPerTerm
		costs.bytesPerNode = scale * jobSettings.BytesPerNode
	}
	return costs
}

// The planner's projection of a run.
type deriveProjection struct {
	// what it was projected from: "run" for the last run's measurements,
	// "count" for the exact count of a first run
	source  string
	terms   int
	nodes   int
	sweeps  int
	seconds float64
	bytes   int64
}

// Projects a run from the measured counts and unit costs of the one before
// it, at the cores this host has now:
//
//	seconds = sweeps × (terms × SecondsPerTermSweep + nodes × SecondsPerNodeSweep) × SweepSafetyFactor
//	bytes   = terms × BytesPerTerm + nodes × BytesPerNode
//
// The unit costs were measured at the last run's cores; the sweep is parallel
// over the nodes, so its time scales with the cores the next run has. Pure.
func projectDeriveRun(jobSettings *DeriveLocationsSettings, last *deriveRunCosts, cores int) *deriveProjection {
	coreScale := 1.0
	if 0 < last.cores && 0 < cores {
		coreScale = float64(last.cores) / float64(cores)
	}
	sweeps := last.sweeps
	if sweeps <= 0 {
		sweeps = jobSettings.DefaultSweeps
	}
	seconds := float64(sweeps) *
		(float64(last.terms)*last.secondsPerTermSweep + float64(last.nodes)*last.secondsPerNodeSweep) *
		jobSettings.SweepSafetyFactor *
		coreScale
	bytes := float64(last.terms)*last.bytesPerTerm + float64(last.nodes)*last.bytesPerNode
	return &deriveProjection{
		source:  "run",
		terms:   last.terms,
		nodes:   last.nodes,
		sweeps:  sweeps,
		seconds: seconds,
		bytes:   int64(bytes),
	}
}

// The planning step at the start of a run: the projection of this run from
// the last recorded one, or, when none is recorded, from the exact count of
// the window's terms and nodes at the settings' unit costs, as if measured on
// one core. countTermsAndNodes is only called for a first run.
func planDeriveRun(
	jobSettings *DeriveLocationsSettings,
	history []*model.DeriveLocationsRun,
	cores int,
	countTermsAndNodes func() (int64, int64),
) *deriveProjection {
	if 0 < len(history) && 0 < history[0].SecondsPerTermSweep {
		last := history[0]
		return projectDeriveRun(jobSettings, &deriveRunCosts{
			terms:               last.Terms,
			nodes:               last.Nodes,
			sweeps:              last.Sweeps,
			cores:               last.Cores,
			solveSeconds:        last.SolveSeconds,
			peakBytes:           last.PeakBytes,
			secondsPerTermSweep: last.SecondsPerTermSweep,
			secondsPerNodeSweep: last.SecondsPerNodeSweep,
			bytesPerTerm:        last.BytesPerTerm,
			bytesPerNode:        last.BytesPerNode,
		}, cores)
	}
	terms, nodes := countTermsAndNodes()
	projection := projectDeriveRun(jobSettings, &deriveRunCosts{
		terms:               int(terms),
		nodes:               int(nodes),
		sweeps:              jobSettings.DefaultSweeps,
		cores:               1,
		secondsPerTermSweep: jobSettings.SecondsPerTermSweep,
		secondsPerNodeSweep: jobSettings.SecondsPerNodeSweep,
		bytesPerTerm:        jobSettings.BytesPerTerm,
		bytesPerNode:        jobSettings.BytesPerNode,
	}, cores)
	projection.source = "count"
	return projection
}

// Logs a projection against the budgets, as shares of each.
func logDeriveProjection(when string, jobSettings *DeriveLocationsSettings, projection *deriveProjection, cores int) {
	gib := func(bytes int64) string {
		return fmt.Sprintf("%.2fGiB", float64(bytes)/(1024*1024*1024))
	}
	glog.Infof(
		"[derive]%s: projected from the %s: %d terms, %d nodes, %d sweeps at %d cores -> %.1fs of MaxSolveSeconds=%.0fs (%.0f%%), %s of MaxSolveBytes=%s (%.0f%%)\n",
		when,
		map[string]string{"run": "last run", "count": "window's exact count"}[projection.source],
		projection.terms,
		projection.nodes,
		projection.sweeps,
		cores,
		projection.seconds,
		jobSettings.MaxSolveSeconds,
		100*projection.seconds/jobSettings.MaxSolveSeconds,
		gib(projection.bytes),
		gib(jobSettings.MaxSolveBytes),
		100*float64(projection.bytes)/float64(jobSettings.MaxSolveBytes),
	)
}
