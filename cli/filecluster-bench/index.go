package main

// The index mode (EVSB-M10-08; §22.8's index row, D-TEST-13's counts, R-EVS-24): at M10 on T3's clusters, formatted
// as production, the row's load, the twenty-agent test, a mailbox stream and a notes vault at §19.6's rates, run in
// legs that turn the index's event classes on one at a time, each leg's store writes counted:
//
//	off        the same load with the index off
//	indexed    in-ref index updates on every eligible file-mode ref (owner decision 156)
//	companion  and companion pushes for git-mode refs and record streams (owner decision 172's cadence)
//	rollup     and roll-ups: the expired bucket's removal and the rewritten all files
//	cut        and erasure cuts, with the INDEX_EPOCH rebuilds they cause
//	rewrite    and erasure rewrites
//	migration  and migrated repos
//
// Every write the leg's load causes passes a counting store: creates, overwrites and deletes on the metadata clusters
// by size, rounded up to 1 KiB, and PUTs and DELETEs on the segment cluster, whatever the write's outcome but a
// definite refusal (a 412 or a 404 writes nothing; an unknown outcome is counted as written). The projection
// (projection.go) prices each leg's writes per content push by M2's price table, so each class's increment is its leg
// less the one before it: runs with it off attributing each increment (D-TEST-13), per indexed push, companion push,
// roll-up, erasure cut, rewrite and migrated repo by the leg's own count of them.
//
// What drives the load is an IndexLoad, SEARCH-M10-40's index writers' load driver, which runs the row's writers and
// evs's engines in this process over the store it is handed and finishes every fold, evacuation and purge a leg's
// pushes cause before it returns, so the leg's writes are all in it. Its method takes and returns only std and
// sidecar types, so the driver, in the sidecar module, needs nothing of this package. None is built yet: the mode
// refuses to start until one is named in indexLoads.
//
// A leg shorter than §22.8's hour, a leg whose load did not drain, a smoke, or a run that names no build is never
// D-TEST-13's evidence, and the file says so.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/sidecar"
)

// The index row's file's mode.
const ModeIndex = "index"

// The index's event classes, in the order the legs turn them on (see the file header).
const (
	IndexClassIndexed   = "indexed"
	IndexClassCompanion = "companion"
	IndexClassRollup    = "rollup"
	IndexClassCut       = "cut"
	IndexClassRewrite   = "rewrite"
	IndexClassMigration = "migration"
)

// Every class, in leg order.
var IndexClasses = []string{IndexClassIndexed, IndexClassCompanion, IndexClassRollup, IndexClassCut, IndexClassRewrite, IndexClassMigration}

// What a load counts, by these names: the content pushes of each kind, and each class's events.
const (
	IndexContentInRef   = "content_in_ref"
	IndexContentGitMode = "content_git_mode"
	IndexContentStream  = "content_stream"

	IndexEventIndexedPush       = "indexed_push"
	IndexEventCompanionPush     = "companion_push"
	IndexEventRollup            = "rollup"
	IndexEventCut               = "cut"
	IndexEventIndexEpochRebuild = "index_epoch_rebuild"
	IndexEventRewrite           = "rewrite"
	IndexEventMigratedRepo      = "migrated_repo"
)

// The content push kinds.
var IndexContentKinds = []string{IndexContentInRef, IndexContentGitMode, IndexContentStream}

// The event each class's increment is per: an erasure cut's INDEX_EPOCH rebuilds are its own cost.
var IndexClassEvents = map[string]string{
	IndexClassIndexed:   IndexEventIndexedPush,
	IndexClassCompanion: IndexEventCompanionPush,
	IndexClassRollup:    IndexEventRollup,
	IndexClassCut:       IndexEventCut,
	IndexClassRewrite:   IndexEventRewrite,
	IndexClassMigration: IndexEventMigratedRepo,
}

// The write kinds a leg counts on a metadata cluster.
const (
	CountedCreate    = "create"
	CountedOverwrite = "overwrite"
	CountedDelete    = "delete"
)

// What the index mode runs. DefaultIndexSettings is the run's.
type IndexSettings struct {
	// Each leg's load: 1 h, as §22.8's mix and nvme rows measure an hour.
	LegDuration time.Duration
	// A leg shorter than this is a smoke.
	EvidenceLegDuration time.Duration
	// The legs, each a name and the classes on in it: off, then each class added in IndexClasses' order.
	Legs []*IndexLegRequest
	// A write's size is counted rounded up to this: 1 KiB, the smaller XFS block, so a price rounds no size down.
	SizeClassByteCount int64
}

// One leg of the run.
type IndexLegRequest struct {
	Name    string
	Classes []string
}

func DefaultIndexSettings() *IndexSettings {
	legs := []*IndexLegRequest{{Name: "off", Classes: []string{}}}
	for i, class := range IndexClasses {
		legs = append(legs, &IndexLegRequest{Name: class, Classes: slices.Clone(IndexClasses[:i+1])})
	}
	return &IndexSettings{
		LegDuration:         time.Hour,
		EvidenceLegDuration: time.Hour,
		Legs:                legs,
		SizeClassByteCount:  1024,
	}
}

// What drives §22.8's index row: see the file header. RunLeg runs the row's content load for duration over store
// with classes on and the rest off, then every deferred write its pushes caused, and answers the content pushes it
// committed by kind (IndexContent...), each class's events (IndexEvent...), and whether every deferred write ran
// before it returned. Safe for one leg at a time.
type IndexLoad interface {
	RunLeg(ctx context.Context, store sidecar.BlobStore, name string, classes []string, duration time.Duration) (contentPushes map[string]int64, events map[string]int64, drained bool, err error)
	Close()
}

// The loads the mode can run, by -load's name: none until SEARCH-M10-40's index writers' load driver lands.
func indexLoads() map[string]func(ctx context.Context) (IndexLoad, error) {
	return map[string]func(ctx context.Context) (IndexLoad, error){}
}

// One counted write kind at one size class.
type CountedOperation struct {
	Kind            string `json:"kind"`
	ObjectByteCount int64  `json:"object_byte_count"`
	Count           int64  `json:"count"`
}

// One leg's counts.
type IndexLeg struct {
	Name    string   `json:"name"`
	Classes []string `json:"classes"`
	Seconds float64  `json:"seconds"`
	// The content pushes by kind and the class events the load counted.
	ContentPushes map[string]int64 `json:"content_pushes"`
	Events        map[string]int64 `json:"events"`
	// The metadata clusters' writes, sorted by kind and size.
	Operations []*CountedOperation `json:"operations"`
	// The segment cluster's.
	HddPutCount    int64 `json:"hdd_put_count"`
	HddDeleteCount int64 `json:"hdd_delete_count"`
	// The load finished the leg's deferred writes in it.
	Drained bool `json:"drained"`
	// Why the leg did not run or did not finish, empty when it did.
	Failure string `json:"failure"`
}

// The content pushes the leg committed.
func (self *IndexLeg) contentPushCount() int64 {
	var count int64
	for _, kind := range IndexContentKinds {
		count += self.ContentPushes[kind]
	}
	return count
}

// An index run's file.
type IndexMeasurement struct {
	RowHeader
	// Each leg's counts, in the order run.
	Legs []*IndexLeg `json:"legs"`
}

// The leg named name, nil when the run has none.
func (self *IndexMeasurement) Leg(name string) *IndexLeg {
	for _, leg := range self.Legs {
		if leg.Name == name {
			return leg
		}
	}
	return nil
}

// An index file, strict: a field the shape lacks refuses it.
func ParseIndexMeasurement(data []byte) (*IndexMeasurement, error) {
	measurement := &IndexMeasurement{}
	if err := parseRowFile(data, measurement, true); err != nil {
		return nil, err
	}
	if measurement.Mode != ModeIndex {
		return nil, fmt.Errorf("mode %q is no index file", measurement.Mode)
	}
	return measurement, nil
}

// The writes counted so far: see the file header. Safe for concurrent use.
type operationCounts struct {
	sizeClassByteCount int64

	stateLock      sync.Mutex
	kindSizeCounts map[string]map[int64]int64
	hddPutCount    int64
	hddDeleteCount int64
}

func newOperationCounts(sizeClassByteCount int64) *operationCounts {
	return &operationCounts{sizeClassByteCount: max(1, sizeClassByteCount), kindSizeCounts: map[string]map[int64]int64{}}
}

func (self *operationCounts) add(kind string, objectByteCount int64) {
	size := (max(0, objectByteCount) + self.sizeClassByteCount - 1) / self.sizeClassByteCount * self.sizeClassByteCount
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	sizeCounts, ok := self.kindSizeCounts[kind]
	if !ok {
		sizeCounts = map[int64]int64{}
		self.kindSizeCounts[kind] = sizeCounts
	}
	sizeCounts[size] += 1
}

func (self *operationCounts) addHdd(put bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if put {
		self.hddPutCount += 1
	} else {
		self.hddDeleteCount += 1
	}
}

// Everything counted since the last take, sorted by kind and size, and the counts begun again.
func (self *operationCounts) take() (operations []*CountedOperation, hddPutCount int64, hddDeleteCount int64) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	operations = []*CountedOperation{}
	for kind, sizeCounts := range self.kindSizeCounts {
		for size, count := range sizeCounts {
			operations = append(operations, &CountedOperation{Kind: kind, ObjectByteCount: size, Count: count})
		}
	}
	sort.Slice(operations, func(i int, j int) bool {
		if operations[i].Kind != operations[j].Kind {
			return operations[i].Kind < operations[j].Kind
		}
		return operations[i].ObjectByteCount < operations[j].ObjectByteCount
	})
	hddPutCount, hddDeleteCount = self.hddPutCount, self.hddDeleteCount
	self.kindSizeCounts = map[string]map[int64]int64{}
	self.hddPutCount, self.hddDeleteCount = 0, 0
	return operations, hddPutCount, hddDeleteCount
}

// Whether a write's outcome may have written: anything but a definite refusal.
func countedOutcome(err error) bool {
	return err == nil || !(errors.Is(err, sidecar.ErrBlobPrecondition) || errors.Is(err, sidecar.ErrBlobNotFound) || errors.Is(err, sidecar.ErrBlobHeld))
}

// A BlobStore whose clusters count their writes into counts: the segment cluster's as HDD PUTs and DELETEs, every
// other cluster's by kind and size. Safe for concurrent use.
type countingBlobs struct {
	sidecar.BlobStore
	segmentClusterId string
	counts           *operationCounts
}

func newCountingBlobs(store sidecar.BlobStore, counts *operationCounts) (*countingBlobs, error) {
	segment, err := store.SegmentCluster()
	if err != nil {
		return nil, err
	}
	return &countingBlobs{BlobStore: store, segmentClusterId: segment.ClusterId(), counts: counts}, nil
}

func (self *countingBlobs) wrap(cluster sidecar.BlobCluster, err error) (sidecar.BlobCluster, error) {
	if err != nil {
		return nil, err
	}
	return &countingBlobCluster{BlobCluster: cluster, segment: cluster.ClusterId() == self.segmentClusterId, counts: self.counts}, nil
}

func (self *countingBlobs) Cluster(clusterId string) (sidecar.BlobCluster, error) {
	return self.wrap(self.BlobStore.Cluster(clusterId))
}

func (self *countingBlobs) FirstCluster() (sidecar.BlobCluster, error) {
	return self.wrap(self.BlobStore.FirstCluster())
}

func (self *countingBlobs) SegmentCluster() (sidecar.BlobCluster, error) {
	return self.wrap(self.BlobStore.SegmentCluster())
}

type countingBlobCluster struct {
	sidecar.BlobCluster
	segment bool
	counts  *operationCounts
}

func (self *countingBlobCluster) count(kind string, objectByteCount int64, err error) {
	if !countedOutcome(err) {
		return
	}
	if self.segment {
		self.counts.addHdd(kind != CountedDelete)
		return
	}
	self.counts.add(kind, objectByteCount)
}

func (self *countingBlobCluster) PutIfNoneMatch(ctx context.Context, key string, body []byte, options *sidecar.BlobPutOptions) (string, error) {
	etag, err := self.BlobCluster.PutIfNoneMatch(ctx, key, body, options)
	self.count(CountedCreate, int64(len(body)), err)
	return etag, err
}

func (self *countingBlobCluster) PutIfMatch(ctx context.Context, key string, etag string, body []byte, options *sidecar.BlobPutOptions) (string, error) {
	newEtag, err := self.BlobCluster.PutIfMatch(ctx, key, etag, body, options)
	self.count(CountedOverwrite, int64(len(body)), err)
	return newEtag, err
}

func (self *countingBlobCluster) PutStream(ctx context.Context, key string, body io.Reader, byteCount int64, checksumSha256 []byte) (string, error) {
	etag, err := self.BlobCluster.PutStream(ctx, key, body, byteCount, checksumSha256)
	self.count(CountedCreate, byteCount, err)
	return etag, err
}

func (self *countingBlobCluster) Delete(ctx context.Context, key string) error {
	err := self.BlobCluster.Delete(ctx, key)
	self.count(CountedDelete, 0, err)
	return err
}

// Runs the index mode's legs into measurement: see the file header. The leg that fails ends the run, its failure in
// its leg and the rest unrun.
func RunIndex(ctx context.Context, load IndexLoad, store sidecar.BlobStore, clock sidecar.Clock, settings *IndexSettings, measurement *IndexMeasurement) error {
	if load == nil {
		return errors.New("no index load")
	}
	if clock == nil {
		clock = sidecar.RealClock{}
	}
	notes := &benchNotes{}
	measurement.Mode = ModeIndex
	measurement.Started = clock.Now()
	defer func() {
		measurement.Ended = clock.Now()
		measurement.Notes = append(measurement.Notes, notes.list()...)
	}()
	counts := newOperationCounts(settings.SizeClassByteCount)
	counting, err := newCountingBlobs(store, counts)
	if err != nil {
		return err
	}
	var runErr error
	for _, request := range settings.Legs {
		leg := &IndexLeg{Name: request.Name, Classes: slices.Clone(request.Classes), ContentPushes: map[string]int64{}, Events: map[string]int64{}, Operations: []*CountedOperation{}}
		measurement.Legs = append(measurement.Legs, leg)
		if runErr != nil {
			leg.Failure = "not run: an earlier leg failed"
			continue
		}
		counts.take()
		started := clock.Now()
		contentPushes, events, drained, err := load.RunLeg(ctx, counting, request.Name, slices.Clone(request.Classes), settings.LegDuration)
		leg.Seconds = clock.Now().Sub(started).Seconds()
		leg.Operations, leg.HddPutCount, leg.HddDeleteCount = counts.take()
		for kind, count := range contentPushes {
			leg.ContentPushes[kind] = count
		}
		for event, count := range events {
			leg.Events[event] = count
		}
		leg.Drained = drained
		if err != nil {
			leg.Failure = err.Error()
			runErr = fmt.Errorf("leg %s: %w", request.Name, err)
			continue
		}
		if !drained {
			notes.add(fmt.Sprintf("leg %s ended with deferred writes not run: its counts lack them", request.Name))
		}
		if leg.contentPushCount() == 0 {
			notes.add(fmt.Sprintf("leg %s committed no content push", request.Name))
		}
	}
	if settings.LegDuration < settings.EvidenceLegDuration {
		measurement.Smoke = true
		notes.add(fmt.Sprintf("legs of %s, under §22.8's %s: a smoke", settings.LegDuration, settings.EvidenceLegDuration))
	}
	for _, leg := range measurement.Legs {
		if !leg.Drained || leg.Failure != "" {
			measurement.Smoke = true
			notes.add("a leg failed or did not drain: a smoke")
			break
		}
	}
	return runErr
}

func runIndexMode(ctx context.Context, args []string, stdout io.Writer) (bool, error) {
	defaults := DefaultIndexSettings()
	settings := *defaults
	flags := flag.NewFlagSet("filecluster-bench index", flag.ContinueOnError)
	common := &runFlags{}
	common.register(flags, false)
	loadName := flags.String("load", "", "the index load to run: one of indexLoads (SEARCH-M10-40's driver)")
	flags.DurationVar(&settings.LegDuration, "leg", defaults.LegDuration, "each leg's load")
	legNames := flags.String("legs", "", "the legs to run, comma-separated, of off and "+strings.Join(IndexClasses, ", ")+"; every leg when empty")
	if err := flags.Parse(args); err != nil {
		return false, err
	}
	newLoad, ok := indexLoads()[*loadName]
	if !ok {
		return false, refuseStart("no index load %q: the index writers' load driver (SEARCH-M10-40) has not landed, so the index row cannot run", *loadName)
	}
	if *legNames != "" {
		wanted := strings.Split(*legNames, ",")
		legs := []*IndexLegRequest{}
		for _, leg := range defaults.Legs {
			if slices.Contains(wanted, leg.Name) {
				legs = append(legs, leg)
			}
		}
		if len(legs) != len(wanted) {
			return false, refuseStart("-legs names a leg there is not: %s", *legNames)
		}
		settings.Legs = legs
	}
	clusters, err := openClusters(ctx, common.clustersPath)
	if err != nil {
		return false, err
	}
	defer clusters.Close()
	measurement := &IndexMeasurement{}
	if err := common.rowHeader(ctx, &measurement.RowHeader, ModeIndex, clusters, "", map[string]string{
		"load": *loadName,
		"leg":  settings.LegDuration.String(),
		"legs": *legNames,
	}); err != nil {
		return false, err
	}
	load, err := newLoad(ctx)
	if err != nil {
		return false, refuseStart("the index load: %v", err)
	}
	defer load.Close()
	runErr := RunIndex(ctx, load, clusters, nil, &settings, measurement)
	if err := writeRowFile(common.outPath, stdout, measurement); err != nil {
		return false, err
	}
	if runErr != nil {
		return false, runErr
	}
	return true, nil
}
