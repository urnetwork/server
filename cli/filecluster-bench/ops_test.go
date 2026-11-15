package main

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
)

// How long a test waits on a run before it fails rather than hangs: a bound on a broken test, never the proof, since
// time in these runs moves only on the fake clock.
const benchTestGuardTimeout = 30 * time.Second

// The fake clock with every callback deferred to an advance: sidecartest.FakeClock calls a callback whose time has
// already come inside AfterFunc itself, and load's Driver, which schedules each arrival from the one before it, then
// recurses through every arrival already due, the last ending the run before the others launch. Real time fires such
// a callback on a goroutine of its own, so no run outside a test meets it. A test's arrivals launch at the advance
// that passes them instead.
type queuedClock struct {
	*sidecartest.FakeClock
}

func (self queuedClock) AfterFunc(timeout time.Duration, function func()) func() bool {
	return self.FakeClock.AfterFunc(max(timeout, time.Nanosecond), function)
}

// Moves the fake clock by step until done closes: each step fires the arrivals and timers it passes, and the run's
// counts depend on no step's timing, since a store answer here takes no time and reads no clock the test moves.
func advanceUntil(t *testing.T, clock *sidecartest.FakeClock, step time.Duration, done <-chan struct{}) {
	t.Helper()
	guard := time.After(benchTestGuardTimeout)
	for {
		select {
		case <-done:
			return
		case <-guard:
			t.Fatal("the run did not end")
		default:
		}
		clock.Advance(step)
		runtime.Gosched()
	}
}

// A cluster that counts what reaches it, so the counters below can write what a drive would for each operation.
type countingCluster struct {
	sidecar.BlobCluster

	stateLock  sync.Mutex
	creates    int64
	overwrites int64
	deletes    int64
	gets       int64
	lists      int64
	liveBytes  int64
	liveCount  int64
	sizes      map[string]int64
}

func (self *countingCluster) PutIfNoneMatch(ctx context.Context, key string, body []byte, options *sidecar.BlobPutOptions) (string, error) {
	etag, err := self.BlobCluster.PutIfNoneMatch(ctx, key, body, options)
	if err == nil {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.creates += 1
		self.liveBytes += int64(len(body))
		self.liveCount += 1
		self.sizes[key] = int64(len(body))
	}
	return etag, err
}

func (self *countingCluster) PutIfMatch(ctx context.Context, key string, etag string, body []byte, options *sidecar.BlobPutOptions) (string, error) {
	newEtag, err := self.BlobCluster.PutIfMatch(ctx, key, etag, body, options)
	if err == nil {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.overwrites += 1
	}
	return newEtag, err
}

func (self *countingCluster) Delete(ctx context.Context, key string) error {
	err := self.BlobCluster.Delete(ctx, key)
	if err == nil {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if size, ok := self.sizes[key]; ok {
			self.deletes += 1
			self.liveBytes -= size
			self.liveCount -= 1
			delete(self.sizes, key)
		}
	}
	return err
}

func (self *countingCluster) Get(ctx context.Context, key string) (*sidecar.BlobObject, error) {
	object, err := self.BlobCluster.Get(ctx, key)
	if err == nil {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.gets += 1
	}
	return object, err
}

func (self *countingCluster) List(ctx context.Context, keyPrefix string, startAfter string, limit int) ([]*sidecar.BlobInfo, bool, error) {
	objects, more, err := self.BlobCluster.List(ctx, keyPrefix, startAfter, limit)
	if err == nil {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.lists += 1
	}
	return objects, more, err
}

// Drive counters that write each operation's cost on the busiest drive and nine tenths of it on the others, 1 KiB
// blocks' costs, and nothing in the background.
type countingCounters struct {
	cluster *countingCluster
	costs   fixtureCosts
	// a drive whose counters are never read
	missingDriveIndex int
}

func (self *countingCounters) ReadDriveCounters(ctx context.Context, clusterId string) (*server.FileClusterCounterReading, error) {
	self.cluster.stateLock.Lock()
	defer self.cluster.stateLock.Unlock()
	busiest := uint64(self.cluster.creates*self.costs.create + self.cluster.overwrites*self.costs.overwrite + self.cluster.deletes*self.costs.delete)
	writes := uint64(3 * (self.cluster.creates + self.cluster.overwrites + self.cluster.deletes))
	reads := uint64(self.cluster.gets + self.cluster.lists)
	reading := &server.FileClusterCounterReading{
		RawUsageByteCount:    uint64(2 * self.cluster.liveBytes),
		RawCapacityByteCount: 15360 * 1000 * 1000 * 1000,
		UsedInodeCount:       uint64(2 * self.cluster.liveCount),
		FreeInodeCount:       7000 * 1000 * 1000,
	}
	for i := range fixtureDriveCount {
		drive := &server.FileClusterDriveCounters{DriveId: fmt.Sprintf("0-0-%d", i), WrittenByteCount: busiest, WritesCompleted: writes, ReadsCompleted: reads}
		if i != 0 {
			drive.WrittenByteCount = busiest * 9 / 10
		}
		if i == self.missingDriveIndex {
			drive = &server.FileClusterDriveCounters{DriveId: drive.DriveId, Missing: true}
		}
		reading.Drives = append(reading.Drives, drive)
	}
	return reading, nil
}

func testOpsSettings() *OpsSettings {
	settings := DefaultOpsSettings()
	settings.ObjectByteCounts = []int64{1024, 16 * 1024}
	settings.ReadObjectByteCount = 1024
	settings.Duration = time.Second
	settings.Rate = 10
	settings.IdleWindow = time.Second
	settings.ListPageCount = 4
	return settings
}

func newOpsTestTarget(clock *sidecartest.FakeClock, missingDriveIndex int) (*BenchTarget, *countingCluster, *sidecartest.FakeBlobs) {
	// the store's own clock stands still, so no deadline of its passes while the test moves the run's
	blobs := sidecartest.NewFakeBlobs(sidecartest.NewFakeClock(sidecartest.DefaultClockStart()), nil, nil, nil)
	cluster, _ := blobs.Cluster("meta-1")
	counting := &countingCluster{BlobCluster: cluster, sizes: map[string]int64{}}
	target := &BenchTarget{
		ClusterId: "meta-1",
		Cluster:   counting,
		Store:     blobs,
		Counters:  &countingCounters{cluster: counting, costs: fixtureCostsOf(1024), missingDriveIndex: missingDriveIndex},
		Clock:     queuedClock{FakeClock: clock},
	}
	return target, counting, blobs
}

func TestOpsRunMeasuresEachClassAloneAndPricesIt(t *testing.T) {
	clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
	target, counting, blobs := newOpsTestTarget(clock, -1)
	settings := testOpsSettings()
	measurement := &Measurement{BlockByteCount: 1024, DataShardCount: 4, Settings: map[string]string{}}
	done := make(chan struct{})
	var runErr error
	go func() {
		defer close(done)
		runErr = RunOps(context.Background(), target, settings, "filecluster-bench/0123456789abcdef/", measurement)
	}()
	advanceUntil(t, clock, 100*time.Millisecond, done)
	if runErr != nil {
		t.Fatal(runErr, measurement.Notes)
	}
	classes := []string{}
	for _, result := range measurement.Ops {
		classes = append(classes, fmt.Sprintf("%s-%d", result.Class, result.ObjectByteCount))
		if result.Count != 10 || result.Failed != 0 || result.Seconds <= 0 {
			t.Fatalf("%s-%d: %d ok, %d failed over %v s", result.Class, result.ObjectByteCount, result.Count, result.Failed, result.Seconds)
		}
	}
	if want := "create-1024 overwrite-1024 create-16384 overwrite-16384 get-1024 list-1024 delete-1024 delete-16384"; strings.Join(classes, " ") != want {
		t.Fatalf("classes %v", classes)
	}
	// the eleventh LIST is the cleanup's, which finds nothing left
	if counting.creates != 20 || counting.overwrites != 20 || counting.deletes != 20 || counting.gets != 10 || counting.lists != 11 {
		t.Fatalf("the store saw %d creates, %d overwrites, %d deletes, %d gets and %d lists", counting.creates, counting.overwrites, counting.deletes, counting.gets, counting.lists)
	}
	// each window holds its class alone, so the table is the counters' costs exactly
	table, _, footprints, err := DerivePriceTable(measurement, DefaultReportSettings())
	if err != nil {
		t.Fatal(err)
	}
	if table.CreateByteCount != 7168 || table.OverwriteByteCount != 10752 || table.DeleteByteCount != 2816 || table.BlockByteCount != 1024 {
		t.Fatalf("priced %+v", table)
	}
	if footprints[0].RawBytesPerOp != 2048 || footprints[0].InodesPerOp != 2 {
		t.Fatalf("footprint %+v", footprints[0])
	}
	// a 1 s class is a smoke, under D-TEST-11's 10 min
	if !measurement.Smoke {
		t.Fatal("a 1 s run was not a smoke")
	}
	// what it wrote is gone
	left, err := blobs.Cluster("meta-1")
	if err != nil {
		t.Fatal(err)
	}
	objects, _, err := left.List(context.Background(), "filecluster-bench/", "", 100)
	if err != nil || len(objects) != 0 {
		t.Fatalf("%d objects left, %v", len(objects), err)
	}
}

func TestOpsRunWithADriveUnreadIsASmokeAndPricesNothing(t *testing.T) {
	clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
	target, _, _ := newOpsTestTarget(clock, 4)
	settings := testOpsSettings()
	settings.Duration = 10 * time.Minute
	settings.EvidenceDuration = 10 * time.Minute
	settings.Rate = 0.05
	measurement := &Measurement{BlockByteCount: 1024, DataShardCount: 4, Settings: map[string]string{}}
	done := make(chan struct{})
	var runErr error
	go func() {
		defer close(done)
		runErr = RunOps(context.Background(), target, settings, "filecluster-bench/fedcba9876543210/", measurement)
	}()
	advanceUntil(t, clock, 10*time.Second, done)
	if runErr != nil {
		t.Fatal(runErr)
	}
	if !measurement.Smoke {
		t.Fatal("a run with a drive unread was not a smoke")
	}
	if _, _, _, err := DerivePriceTable(measurement, DefaultReportSettings()); err == nil {
		t.Fatal("priced a run with a drive unread")
	}
}

func TestOpsKeysNeverPrefixOneAnother(t *testing.T) {
	prefix := "filecluster-bench/0123456789abcdef/"
	keys := []string{opsKey(prefix, 1024, 1), opsKey(prefix, 1024, 10), opsKey(prefix, 10240, 1), opsKey(prefix, 1024, 100000)}
	for i, a := range keys {
		for j, b := range keys {
			if i != j && strings.HasPrefix(b, a) {
				t.Fatalf("%s prefixes %s", a, b)
			}
		}
		if !strings.HasPrefix(a, opsSizePrefix(prefix, 1024)) && !strings.HasPrefix(a, opsSizePrefix(prefix, 10240)) {
			t.Fatalf("%s is under no size's prefix", a)
		}
	}
}

func TestDriveDeltasReadAFallenCounterAsMissing(t *testing.T) {
	before := &driveReading{counters: &server.FileClusterCounterReading{Drives: []*server.FileClusterDriveCounters{
		{DriveId: "0-0-0", WrittenByteCount: 1000, WritesCompleted: 10, ReadsCompleted: 5},
		{DriveId: "0-0-1", WrittenByteCount: 5000, WritesCompleted: 50, ReadsCompleted: 5},
		{DriveId: "0-0-2", WrittenByteCount: 1000, WritesCompleted: 10, ReadsCompleted: 5, Healing: true},
	}}}
	after := &driveReading{counters: &server.FileClusterCounterReading{Drives: []*server.FileClusterDriveCounters{
		{DriveId: "0-0-0", WrittenByteCount: 3000, WritesCompleted: 30, ReadsCompleted: 7},
		// a reboot: a fresh baseline, never a credit
		{DriveId: "0-0-1", WrittenByteCount: 10, WritesCompleted: 1, ReadsCompleted: 0},
		{DriveId: "0-0-2", WrittenByteCount: 2000, WritesCompleted: 20, ReadsCompleted: 5},
		// a drive the first reading lacked
		{DriveId: "0-0-3", WrittenByteCount: 2000, WritesCompleted: 20, ReadsCompleted: 5},
	}}}
	deltas := driveDeltas(before, after)
	if len(deltas) != 4 {
		t.Fatalf("%d deltas", len(deltas))
	}
	if deltas[0].Missing || deltas[0].WrittenByteCount != 2000 || deltas[0].WritesCompleted != 20 || deltas[0].ReadsCompleted != 2 {
		t.Fatalf("drive 0: %+v", deltas[0])
	}
	if !deltas[1].Missing || !deltas[3].Missing {
		t.Fatalf("a fallen or new counter read as %+v and %+v", deltas[1], deltas[3])
	}
	if deltas[2].Missing || !deltas[2].Healing {
		t.Fatalf("a drive under heal at the start read %+v", deltas[2])
	}
	busiest, ok := busiestDrive(deltas)
	if !ok || busiest.DriveId != "0-0-0" {
		t.Fatalf("busiest %+v", busiest)
	}
}
