package main

// The mix mode (§22.8's mix and nvme rows; §11.13's mix): §11.13's mix with the write budget on, refilled at 0.8
// DWPD for the run, at 1x and 2x year one and at the budget's limit, each phase 25 h of preload (so the cluster holds
// 25 h of batches), 1 h of warm-up and a measured hour, open loop (sidecartest/load's Driver), with a restart of every
// evs process, each reset incomplete, inside each measured hour. The limit phase's pace is the refill over the 2x
// phase's bytes per push on the busiest drive, the load at which the budget just holds.
//
// Each measured hour records the measured cluster's drive counters on either side, every push's outcome (committed,
// refused by the budget, FOLD_BEHIND, or another failure), each store operation's latency, and the stack's own series
// sampled every 30 s: the budget's sheds and priced bytes by class and P0 kind, P0's refill ratio, evs's FOLD_BEHIND
// refusals and evacuation's oldest batch. Then it samples the hour's batches on each metadata cluster and counts
// their records, for records per batch per cluster.
//
// What drives the mix is a MixTarget, and what reads the stack's series a MixObserver: mix_engine.go's in-process
// target runs evsservice's engines on the clusters under test, and its observer reads their metrics. A target names
// the event classes of §11.13's mix it cannot drive, and the report refuses a phase that lacks any as evidence. Not
// safe for concurrent use; one run a target.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/evsservice"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/sidecartest/load"
)

// The event classes of §11.13's mix table.
const (
	MixEventPush          = "push"
	MixEventPart          = "part"
	MixEventSegmentUpload = "segment_upload"
	MixEventFold          = "fold"
	MixEventEvacuation    = "evacuation"
	MixEventGkmCommit     = "gkm_commit"
	MixEventRotation      = "rotation"
	MixEventAdd           = "add"
	MixEventPathEpochCopy = "path_epoch_copy"
	MixEventRemovedEntry  = "removed_entry"
	MixEventProjection    = "projection"
	MixEventReconnect     = "reconnect"
	MixEventFullResync    = "full_resync"
)

// Every event class §11.13's mix names at M2: the report reads a phase as the whole mix only when its target drove
// each.
var MixEvents = []string{
	MixEventPush, MixEventPart, MixEventSegmentUpload, MixEventFold, MixEventEvacuation, MixEventGkmCommit,
	MixEventRotation, MixEventAdd, MixEventPathEpochCopy, MixEventRemovedEntry, MixEventProjection, MixEventReconnect,
	MixEventFullResync,
}

// What the mix mode runs (§22.8's nvme and mix rows). DefaultMixSettings is the run's.
type MixSettings struct {
	// Year one's pushes a second (§19.6): 200.
	YearOnePushesPerSecond float64
	// The phases in order, each a multiple of year one's pushes, the limit's computed: 1x, 2x, limit.
	Phases []string
	// Before each phase's warm-up, so the cluster holds 25 h of batches at its pace: 25 h.
	Preload time.Duration
	// 1 h, and the measured window, 1 h.
	Warmup time.Duration
	Steady time.Duration
	// The budget's refill for the run: 0.8 DWPD (D-TEST-11), not production's 0.768.
	RefillDwpd float64
	// R-OPS-12's drive, 1.92 TB, whose refill the limit phase's pace is.
	DriveByteCount int64
	// How often the stack's series are read in the measured window: 30 s.
	SampleInterval time.Duration
	// Where in the measured window the restart runs, as a share of it: a half.
	RestartShare float64
	// Batches read per metadata cluster for records per batch: 512.
	BatchSampleCount int
	// The keys a batch minute's listing asks for at once: 1,000.
	BatchListLimit int
	// The most of one batch read: 64 MiB, past any batch evs writes (§11.8's bound).
	MaxBatchByteCount int64
	// The most pushes in flight; past it a push fails as the driver counts it: 4,096.
	MaxInFlight int
	// The distinct failure texts a phase keeps: 16.
	MaxFailureKinds int
	// The most latencies kept per store operation, a uniform sample past it: 1,000,000.
	LatencySampleCount int
	// A run whose preload, warm-up or measured window is shorter than these is a smoke: 25 h, 1 h and 1 h.
	EvidencePreload time.Duration
	EvidenceWarmup  time.Duration
	EvidenceSteady  time.Duration
}

func DefaultMixSettings() *MixSettings {
	return &MixSettings{
		YearOnePushesPerSecond: 200,
		Phases:                 []string{MixPhaseYearOne, MixPhaseTwiceYear, MixPhaseAtTheLimit},
		Preload:                25 * time.Hour,
		Warmup:                 time.Hour,
		Steady:                 time.Hour,
		RefillDwpd:             0.8,
		DriveByteCount:         1920 * 1000 * 1000 * 1000,
		SampleInterval:         30 * time.Second,
		RestartShare:           0.5,
		BatchSampleCount:       512,
		BatchListLimit:         1000,
		MaxBatchByteCount:      64 * 1024 * 1024,
		MaxInFlight:            4096,
		MaxFailureKinds:        16,
		LatencySampleCount:     1000 * 1000,
		EvidencePreload:        25 * time.Hour,
		EvidenceWarmup:         time.Hour,
		EvidenceSteady:         time.Hour,
	}
}

// What drives the mix.
type MixTarget interface {
	// One push of the mix, its refusal in its error: sidecar.IsEvsRefusal classifies it.
	Push(ctx context.Context, arrival int64) error
	// Restarts every evs process the target runs, each with its resets incomplete.
	Restart(ctx context.Context) error
	// The event classes of §11.13's mix it drives, and those it cannot.
	Events() (driven []string, missing []string)
	// Each store operation's latencies since the last call, by StoreOperation.
	TakeOperationLatencies() map[string][]time.Duration
	// The metadata clusters it writes, whose batches are sampled.
	MetadataClusterIds() []string
	Close()
}

// One series' value at a reading.
type metricSample struct {
	Name   string
	Labels map[string]string
	Value  float64
}

// Reads the stack's series.
type MixObserver interface {
	Observe(ctx context.Context) ([]*metricSample, error)
}

// The series a phase reads, summed over processes and clusters, or the largest for a level.
type mixSeries struct {
	shedCount          float64
	foldBehindCount    float64
	pricedByteCount    float64
	p0PricedByteCounts map[string]float64
	p0RefillRatio      float64
	oldestBatchSeconds float64
}

// The series names it reads (§19.8).
const (
	seriesBudgetIssued     = "urnetwork_evs_budget_issued_bytes_total"
	seriesBudgetShed       = "urnetwork_evs_budget_shed_total"
	seriesBudgetP0Refill   = "urnetwork_evs_budget_p0_refill_ratio"
	seriesEvsRefusals      = "urnetwork_evs_refusals_total"
	seriesOldestBatch      = "urnetwork_evs_evacuation_oldest_batch_seconds"
	seriesIssuedClassP0    = "p0"
	seriesRefusalsCodeName = "code"
)

func readMixSeries(samples []*metricSample) *mixSeries {
	series := &mixSeries{p0PricedByteCounts: map[string]float64{}}
	foldBehind := protocol.EvsRefusalCode_EVS_REFUSAL_FOLD_BEHIND.String()
	for _, sample := range samples {
		switch sample.Name {
		case seriesBudgetShed:
			series.shedCount += sample.Value
		case seriesEvsRefusals:
			if sample.Labels[seriesRefusalsCodeName] == foldBehind {
				series.foldBehindCount += sample.Value
			}
		case seriesBudgetIssued:
			series.pricedByteCount += sample.Value
			if sample.Labels["class"] == seriesIssuedClassP0 {
				series.p0PricedByteCounts[sample.Labels["kind"]] += sample.Value
			}
		case seriesBudgetP0Refill:
			series.p0RefillRatio = max(series.p0RefillRatio, sample.Value)
		case seriesOldestBatch:
			series.oldestBatchSeconds = max(series.oldestBatchSeconds, sample.Value)
		}
	}
	return series
}

// A bounded uniform sample of durations, safe for concurrent use.
type latencySample struct {
	stateLock sync.Mutex
	limit     int
	offered   int64
	kept      []time.Duration
	random    *rand.Rand
}

func newLatencySample(limit int, seed uint64) *latencySample {
	return &latencySample{limit: max(1, limit), random: rand.New(rand.NewPCG(seed, 0x1a7e))}
}

func (self *latencySample) add(duration time.Duration) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.offered += 1
	if len(self.kept) < self.limit {
		self.kept = append(self.kept, duration)
		return
	}
	// reservoir: each offered duration kept with probability limit/offered
	if i := self.random.Int64N(self.offered); i < int64(self.limit) {
		self.kept[i] = duration
	}
}

func (self *latencySample) take() []time.Duration {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	kept := self.kept
	self.kept = nil
	self.offered = 0
	return kept
}

// A BlobStore whose clusters time each store operation, by StoreOperation: how the mix row's p99 per operation is
// read where the target's requests pass through this process.
type timedBlobs struct {
	sidecar.BlobStore
	operationSamples map[string]*latencySample
	clock            sidecar.Clock
}

func newTimedBlobs(blobs sidecar.BlobStore, limit int, clock sidecar.Clock) *timedBlobs {
	operationSamples := map[string]*latencySample{}
	for i, operation := range []string{StoreOperationGet, StoreOperationPut, StoreOperationList, StoreOperationDelete, StoreOperationHead} {
		operationSamples[operation] = newLatencySample(limit, uint64(i+1))
	}
	return &timedBlobs{BlobStore: blobs, operationSamples: operationSamples, clock: clock}
}

func (self *timedBlobs) wrap(cluster sidecar.BlobCluster, err error) (sidecar.BlobCluster, error) {
	if err != nil {
		return nil, err
	}
	return &timedBlobCluster{BlobCluster: cluster, blobs: self}, nil
}

func (self *timedBlobs) Cluster(clusterId string) (sidecar.BlobCluster, error) {
	return self.wrap(self.BlobStore.Cluster(clusterId))
}

func (self *timedBlobs) FirstCluster() (sidecar.BlobCluster, error) {
	return self.wrap(self.BlobStore.FirstCluster())
}

func (self *timedBlobs) SegmentCluster() (sidecar.BlobCluster, error) {
	return self.wrap(self.BlobStore.SegmentCluster())
}

// Each operation's latencies since the last call.
func (self *timedBlobs) take() map[string][]time.Duration {
	operationLatencies := map[string][]time.Duration{}
	for operation, sample := range self.operationSamples {
		operationLatencies[operation] = sample.take()
	}
	return operationLatencies
}

func (self *timedBlobs) observe(operation string, start time.Time, err error) {
	// a refusal is an answer, timed like any; a failure to answer is not a latency
	var unknown *sidecar.BlobUnknownError
	if errors.As(err, &unknown) {
		return
	}
	self.operationSamples[operation].add(self.clock.Now().Sub(start))
}

type timedBlobCluster struct {
	sidecar.BlobCluster
	blobs *timedBlobs
}

func (self *timedBlobCluster) PutIfNoneMatch(ctx context.Context, key string, body []byte, options *sidecar.BlobPutOptions) (string, error) {
	start := self.blobs.clock.Now()
	etag, err := self.BlobCluster.PutIfNoneMatch(ctx, key, body, options)
	self.blobs.observe(StoreOperationPut, start, err)
	return etag, err
}

func (self *timedBlobCluster) PutIfMatch(ctx context.Context, key string, etag string, body []byte, options *sidecar.BlobPutOptions) (string, error) {
	start := self.blobs.clock.Now()
	newEtag, err := self.BlobCluster.PutIfMatch(ctx, key, etag, body, options)
	self.blobs.observe(StoreOperationPut, start, err)
	return newEtag, err
}

func (self *timedBlobCluster) Get(ctx context.Context, key string) (*sidecar.BlobObject, error) {
	start := self.blobs.clock.Now()
	object, err := self.BlobCluster.Get(ctx, key)
	self.blobs.observe(StoreOperationGet, start, err)
	return object, err
}

func (self *timedBlobCluster) GetRange(ctx context.Context, key string, offset int64, byteCount int64) (*sidecar.BlobObject, error) {
	start := self.blobs.clock.Now()
	object, err := self.BlobCluster.GetRange(ctx, key, offset, byteCount)
	self.blobs.observe(StoreOperationGet, start, err)
	return object, err
}

func (self *timedBlobCluster) Head(ctx context.Context, key string) (*sidecar.BlobInfo, error) {
	start := self.blobs.clock.Now()
	info, err := self.BlobCluster.Head(ctx, key)
	self.blobs.observe(StoreOperationHead, start, err)
	return info, err
}

func (self *timedBlobCluster) List(ctx context.Context, keyPrefix string, startAfter string, limit int) ([]*sidecar.BlobInfo, bool, error) {
	start := self.blobs.clock.Now()
	objects, more, err := self.BlobCluster.List(ctx, keyPrefix, startAfter, limit)
	self.blobs.observe(StoreOperationList, start, err)
	return objects, more, err
}

func (self *timedBlobCluster) Delete(ctx context.Context, key string) error {
	start := self.blobs.clock.Now()
	err := self.BlobCluster.Delete(ctx, key)
	self.blobs.observe(StoreOperationDelete, start, err)
	return err
}

// How a push's error classifies.
func classifyPushError(err error) string {
	switch {
	case err == nil:
		return ""
	case sidecar.IsEvsRefusal(err, protocol.EvsRefusalCode_EVS_REFUSAL_RESOURCE_EXHAUSTED):
		return "budget"
	case sidecar.IsEvsRefusal(err, protocol.EvsRefusalCode_EVS_REFUSAL_FOLD_BEHIND):
		return "fold_behind"
	}
	return "other"
}

// Records per batch on cluster over the minutes from's through to's, from up to sampleCount batches spread over them:
// a window inside one minute samples that minute.
func sampleRecordsPerBatch(ctx context.Context, cluster sidecar.BlobCluster, from time.Time, to time.Time, sampleCount int, listLimit int, maxBatchByteCount int64) (float64, int, error) {
	minutes := []uint32{}
	for minute := evsservice.BatchMinute(from); minute <= evsservice.BatchMinute(to); minute += 1 {
		minutes = append(minutes, minute)
	}
	if len(minutes) == 0 {
		return 0, 0, nil
	}
	perMinute := max(1, sampleCount/len(minutes))
	records := 0
	batches := 0
	for _, minute := range minutes {
		objects, _, err := cluster.List(ctx, evsservice.BatchMinutePrefix(minute), "", min(listLimit, perMinute))
		if err != nil {
			return 0, batches, err
		}
		for _, object := range objects {
			blob, err := cluster.Get(ctx, object.Key)
			if err != nil {
				return 0, batches, err
			}
			body, err := io.ReadAll(io.LimitReader(blob.Body, maxBatchByteCount+1))
			blob.Body.Close()
			if err != nil {
				return 0, batches, err
			}
			batch, err := sidecar.ParseEvsBatchFormat2(body)
			if err != nil {
				// a batch this build cannot read counts nothing, as evacuation leaves it alarming
				continue
			}
			records += len(batch.Records)
			batches += 1
		}
	}
	if batches == 0 {
		return 0, 0, nil
	}
	return float64(records) / float64(batches), batches, nil
}

// Runs the mix's phases into measurement: see the file header. bench reads the measured cluster's counters.
func RunMix(ctx context.Context, bench *BenchTarget, target MixTarget, observer MixObserver, settings *MixSettings, measurement *Measurement) error {
	if settings.YearOnePushesPerSecond <= 0 || settings.Steady <= 0 || len(settings.Phases) == 0 {
		return fmt.Errorf("a mix run needs a pace, a window and phases, not %v, %v and %v", settings.YearOnePushesPerSecond, settings.Steady, settings.Phases)
	}
	clock := bench.clock()
	notes := &benchNotes{}
	measurement.Mode = ModeMix
	measurement.ClusterId = bench.ClusterId
	measurement.Started = clock.Now()
	defer func() {
		measurement.Ended = clock.Now()
		measurement.Notes = append(measurement.Notes, notes.list()...)
	}()
	driven, missing := target.Events()
	if 0 < len(missing) {
		notes.add("the target cannot drive " + strings.Join(missing, ", ") + ": no phase is the whole mix")
	}
	measurement.SmartBefore = smartOf(bench.read(ctx, notes))

	for _, name := range settings.Phases {
		factor := 0.0
		switch name {
		case MixPhaseYearOne:
			factor = 1
		case MixPhaseTwiceYear:
			factor = 2
		case MixPhaseAtTheLimit:
			// the refill over the 2x phase's bytes per push on its busiest drive
			twice := measurement.Phase(MixPhaseTwiceYear)
			if twice == nil {
				notes.add("the limit phase needs the 2x phase first: not run")
				continue
			}
			busiest, ok := busiestDrive(twice.Drives)
			if !ok || anyMissing(twice.Drives) || twice.PushesCommitted == 0 {
				notes.add("the 2x phase read no drive's bytes per push, so the limit is unknown: not run")
				continue
			}
			perPush := float64(busiest.WrittenByteCount) / float64(twice.PushesCommitted)
			refillPerSecond := settings.RefillDwpd * float64(settings.DriveByteCount) / 86400
			factor = refillPerSecond / perPush / settings.YearOnePushesPerSecond
		default:
			return fmt.Errorf("a phase %q", name)
		}
		phase, err := runMixPhase(ctx, bench, target, observer, settings, name, factor, driven, missing, notes)
		if phase != nil {
			measurement.Phases = append(measurement.Phases, phase)
		}
		if err != nil {
			return err
		}
	}
	measurement.SmartAfter = smartOf(bench.read(ctx, notes))

	if settings.Preload < settings.EvidencePreload || settings.Warmup < settings.EvidenceWarmup || settings.Steady < settings.EvidenceSteady {
		measurement.Smoke = true
		notes.add(fmt.Sprintf("phases of %s preload, %s warm-up and %s measured, under D-TEST-11's %s, %s and %s: a smoke",
			settings.Preload, settings.Warmup, settings.Steady, settings.EvidencePreload, settings.EvidenceWarmup, settings.EvidenceSteady))
	}
	for _, phase := range measurement.Phases {
		if anyMissing(phase.Drives) {
			measurement.Smoke = true
			notes.add("a phase read no drive's counters: a smoke")
			break
		}
	}
	return nil
}

// One phase: its preload and warm-up unmeasured, then its measured window. See the file header.
func runMixPhase(
	ctx context.Context,
	bench *BenchTarget,
	target MixTarget,
	observer MixObserver,
	settings *MixSettings,
	name string,
	factor float64,
	driven []string,
	missing []string,
	notes *benchNotes,
) (*MixPhase, error) {
	clock := bench.clock()
	rate := factor * settings.YearOnePushesPerSecond
	phase := &MixPhase{
		Name:                   name,
		Factor:                 factor,
		RefillDwpd:             settings.RefillDwpd,
		Seconds:                settings.Steady.Seconds(),
		OfferedPushesPerSecond: rate,
		PushFailureKinds:       map[string]int64{},
		OperationLatencyMillis: map[string]load.Percentiles{},
		RecordsPerBatch:        map[string]float64{},
		BatchesSampled:         map[string]int{},
		EventsDriven:           slices.Clone(driven),
		EventsMissing:          slices.Clone(missing),
	}
	unmeasured := settings.Preload + settings.Warmup
	// arrival n is due at n / rate, so the measured window's arrivals are those due at or past the unmeasured span
	firstMeasured := int64(unmeasured.Seconds() * rate)
	var budgetRefused, foldBehind, failed atomic.Int64

	var before *driveReading
	var after *driveReading
	var firstSeries *mixSeries
	var lastSeries *mixSeries
	seriesOk := true
	// set by the window's closing sample, after which a periodic one still in flight is dropped
	seriesClosed := false
	var restartErr error
	restarted := false
	stateLock := sync.Mutex{}
	// the window's first sample opens it, its last closes it, and those between only raise the levels' maxima
	const (
		sampleFirst = iota
		samplePeriodic
		sampleLast
	)
	sampleSeries := func(which int) {
		var samples []*metricSample
		var err error
		if observer == nil {
			err = errors.New("no observer of the stack's series")
		} else {
			samples, err = observer.Observe(ctx)
		}
		stateLock.Lock()
		defer stateLock.Unlock()
		if seriesClosed {
			return
		}
		if err != nil {
			seriesOk = false
			notes.add(fmt.Sprintf("the stack's series unread: %v", err))
		} else {
			series := readMixSeries(samples)
			switch which {
			case sampleFirst:
				firstSeries = series
			case sampleLast:
				lastSeries = series
			}
			phase.P0RefillRatioMax = max(phase.P0RefillRatioMax, series.p0RefillRatio)
			phase.OldestBatchSecondsMax = max(phase.OldestBatchSecondsMax, series.oldestBatchSeconds)
		}
		if which == sampleLast {
			seriesClosed = true
		}
	}

	driver := load.NewDriver(&load.DriverSettings{
		Name:            "mix-" + name,
		Rate:            rate,
		Warmup:          unmeasured,
		Steady:          settings.Steady,
		MaxInFlight:     settings.MaxInFlight,
		MaxFailureKinds: settings.MaxFailureKinds,
		Clock:           clock,
	}, func(ctx context.Context, arrival int64) error {
		err := target.Push(ctx, arrival)
		if arrival < firstMeasured {
			return err
		}
		switch classifyPushError(err) {
		case "budget":
			budgetRefused.Add(1)
		case "fold_behind":
			foldBehind.Add(1)
		case "other":
			failed.Add(1)
		}
		return err
	})

	// the measured window's edges and its samples, on the driver's clock: its end and restart are set from its start,
	// so the window is Steady long on that clock whatever ran between
	sampleCtx, stopSampling := context.WithCancel(ctx)
	defer stopSampling()
	samplingDone := make(chan struct{})
	endDone := make(chan struct{})
	restartDone := make(chan struct{})
	operationLatencies := map[string][]time.Duration{}
	stops := []func() bool{}
	defer func() {
		stateLock.Lock()
		defer stateLock.Unlock()
		for _, stop := range stops {
			stop()
		}
	}()
	restart := func() {
		defer close(restartDone)
		err := target.Restart(ctx)
		stateLock.Lock()
		defer stateLock.Unlock()
		restarted = true
		restartErr = err
	}
	end := func() {
		defer close(endDone)
		latencies := target.TakeOperationLatencies()
		reading := bench.read(ctx, notes)
		sampleSeries(sampleLast)
		stopSampling()
		stateLock.Lock()
		defer stateLock.Unlock()
		after = reading
		operationLatencies = latencies
	}
	stopStart := clock.AfterFunc(unmeasured, func() {
		target.TakeOperationLatencies()
		reading := bench.read(ctx, notes)
		sampleSeries(sampleFirst)
		stopRestart := clock.AfterFunc(time.Duration(float64(settings.Steady)*settings.RestartShare), restart)
		stopEnd := clock.AfterFunc(settings.Steady, end)
		stateLock.Lock()
		before = reading
		stops = append(stops, stopRestart, stopEnd)
		stateLock.Unlock()
		go func() {
			defer close(samplingDone)
			for {
				select {
				case <-sampleCtx.Done():
					return
				case <-clock.After(settings.SampleInterval):
				}
				sampleSeries(samplePeriodic)
			}
		}()
	})
	defer stopStart()

	report := driver.Run(ctx)
	// the window's end may come after the driver's last attempt ended: wait for it, and for the sampler to stop
	select {
	case <-endDone:
		<-samplingDone
	case <-ctx.Done():
		return phase, ctx.Err()
	}
	// the window has closed, its restart ended and its sampler stopped, so nothing writes what it read any more
	<-restartDone
	stateLock.Lock()
	windowBefore, windowAfter, windowFirst, windowLast := before, after, firstSeries, lastSeries
	windowSeriesOk, windowRestarted, windowRestartErr := seriesOk, restarted, restartErr
	stateLock.Unlock()

	phase.PushesCommitted = report.Succeeded
	phase.PushesRefusedByBudget = budgetRefused.Load()
	phase.PushesFoldBehind = foldBehind.Load()
	phase.PushesFailed = failed.Load()
	phase.PushFailureKinds = report.FailureKinds
	phase.PushLatencyMillis = report.LatencyMillis
	for operation, latencies := range operationLatencies {
		if 0 < len(latencies) {
			phase.OperationLatencyMillis[operation] = load.PercentilesOf(latencies)
		}
	}
	if windowBefore != nil && windowAfter != nil {
		phase.Seconds = readingSeconds(windowBefore, windowAfter)
		phase.Drives = driveDeltas(windowBefore, windowAfter)
		phase.RawUsageDelta, phase.InodeDelta = usageDeltas(windowBefore, windowAfter)
		if windowAfter.counters != nil {
			phase.RawCapacityByteCount = windowAfter.counters.RawCapacityByteCount
			phase.RawUsageByteCount = windowAfter.counters.RawUsageByteCount
			phase.UsedInodeCount = windowAfter.counters.UsedInodeCount
			phase.FreeInodeCount = windowAfter.counters.FreeInodeCount
		}
	}
	phase.SeriesRead = windowSeriesOk && windowFirst != nil && windowLast != nil
	if phase.SeriesRead {
		phase.BudgetShedCount = int64(windowLast.shedCount - windowFirst.shedCount)
		phase.FoldBehindCount = int64(windowLast.foldBehindCount - windowFirst.foldBehindCount)
		phase.PricedByteCount = int64(windowLast.pricedByteCount - windowFirst.pricedByteCount)
		phase.P0PricedByteCounts = map[string]int64{}
		for kind, value := range windowLast.p0PricedByteCounts {
			if delta := int64(value - windowFirst.p0PricedByteCounts[kind]); 0 < delta {
				phase.P0PricedByteCounts[kind] = delta
			}
		}
	}
	phase.RestartIncluded = windowRestarted && windowRestartErr == nil
	if windowRestartErr != nil {
		notes.add(fmt.Sprintf("phase %s's restart failed: %v", name, windowRestartErr))
	}

	clusterIds := target.MetadataClusterIds()
	sort.Strings(clusterIds)
	for _, clusterId := range clusterIds {
		cluster, err := clusterOf(bench, clusterId)
		if err != nil {
			notes.add(fmt.Sprintf("records per batch on %s: %v", clusterId, err))
			continue
		}
		if windowBefore == nil || windowAfter == nil {
			continue
		}
		recordsPerBatch, batches, err := sampleRecordsPerBatch(ctx, cluster, windowBefore.at, windowAfter.at, settings.BatchSampleCount, settings.BatchListLimit, settings.MaxBatchByteCount)
		if err != nil {
			notes.add(fmt.Sprintf("records per batch on %s: %v", clusterId, err))
			continue
		}
		phase.RecordsPerBatch[clusterId] = recordsPerBatch
		phase.BatchesSampled[clusterId] = batches
	}
	return phase, nil
}

// One of the clusters the bench reaches, by id: its own when the id is its own, else through its store.
func clusterOf(bench *BenchTarget, clusterId string) (sidecar.BlobCluster, error) {
	if clusterId == bench.ClusterId {
		return bench.Cluster, nil
	}
	if bench.Store == nil {
		return nil, fmt.Errorf("no store reaches %s", clusterId)
	}
	return bench.Store.Cluster(clusterId)
}
