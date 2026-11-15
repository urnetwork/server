package main

// The ops mode (§22.8's nvme row): each operation class alone on a metadata cluster, open loop at a set pace
// (sidecartest/load's Driver), each window between two readings of every drive's counters, from which the price
// table comes. Per object size it creates, then overwrites with If-Match, as a head is overwritten; then it reads
// and lists one size's objects; then it deletes each size's objects, so each class's window holds that class alone.
// An idle window first measures the cluster's background, which the report takes off each class's bytes.
//
// It writes only under its own run's prefix, filecluster-bench/<run id>/, with keys of one length under each size so
// none prefixes another (§11.8's keys row), and deletes what it wrote, so it may run on a cluster that serves; the
// nvme run itself is on M2's staging cluster (§19.11). Not safe for concurrent use; one run a target.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"strconv"
	"sync"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/sidecartest"
	"github.com/urnetwork/sidecar/sidecartest/load"

	"github.com/urnetwork/server"
)

// What the ops mode runs (§22.8's nvme row). DefaultOpsSettings is the run's.
type OpsSettings struct {
	// The sizes create, overwrite and delete run at: 1, 4, 10, 16, 64 and 160 KiB.
	ObjectByteCounts []int64
	// The size GET and LIST run on: 4 KiB, a small metadata object's.
	ReadObjectByteCount int64
	// Each class's measured window: 10 min.
	Duration time.Duration
	// Each class's pace, operations a second: 500, past year two's ~5,000 PUTs a second over its sets' processes'
	// share and far enough above the background to measure.
	Rate float64
	// The most operations in flight; one past it fails, as the driver counts it: 512.
	MaxInFlight int
	// The window with no load before the classes, whose bytes are the background: 2 min.
	IdleWindow time.Duration
	// The keys a LIST asks for: 1,000, a pidx/ listing's page.
	ListPageCount int
	// The distinct failure texts a class keeps: 16.
	MaxFailureKinds int
	// The deletes in flight while the run removes what a failed class left: 64.
	CleanupInFlight int
	// The keys the cleanup lists at once: 1,000.
	CleanupListLimit int
	// A run shorter than this is a smoke (D-TEST-11's 10 min a class).
	EvidenceDuration time.Duration
}

func DefaultOpsSettings() *OpsSettings {
	return &OpsSettings{
		ObjectByteCounts:    []int64{1024, 4 * 1024, 10 * 1024, 16 * 1024, 64 * 1024, 160 * 1024},
		ReadObjectByteCount: 4 * 1024,
		Duration:            10 * time.Minute,
		Rate:                500,
		MaxInFlight:         512,
		IdleWindow:          2 * time.Minute,
		ListPageCount:       1000,
		MaxFailureKinds:     16,
		CleanupInFlight:     64,
		CleanupListLimit:    1000,
		EvidenceDuration:    10 * time.Minute,
	}
}

// Reads a cluster's drive counters: server.FileClusterCounters, over each storage host's exporter.
type DriveCounterReader interface {
	ReadDriveCounters(ctx context.Context, clusterId string) (*server.FileClusterCounterReading, error)
}

// Reads what the storage hosts know of their drives that the exporter does not serve: each drive's XFS log and file
// bytes so far and its SMART log, by drive id (hoststats.go).
type HostStatsReader interface {
	ReadHostStats(ctx context.Context, clusterId string) (map[string]*DriveHostStats, error)
}

// One drive's host stats at one reading.
type DriveHostStats struct {
	XfsLogByteCount  uint64
	XfsFileByteCount uint64
	Smart            *DriveSmart
}

// What a run measures against.
type BenchTarget struct {
	ClusterId string
	Cluster   sidecar.BlobCluster
	// The other clusters a run reads, such as the mix's batches on each metadata cluster; nil reaches only Cluster.
	Store sidecar.BlobStore
	// nil reads no counters, so every drive reads missing and the run is a smoke.
	Counters DriveCounterReader
	// nil reads no XFS or SMART figures, which the run notes.
	HostStats HostStatsReader
	// What waits and the driver read. nil is real time.
	Clock sidecartest.Clock
}

func (self *BenchTarget) clock() sidecartest.Clock {
	if self.Clock == nil {
		return sidecar.RealClock{}
	}
	return self.Clock
}

// One reading of every drive: its counters, its host stats, and the cluster's raw use, any of them absent where
// unread.
type driveReading struct {
	// when the reading began, on the target's clock
	at        time.Time
	counters  *server.FileClusterCounterReading
	hostStats map[string]*DriveHostStats
}

// The span between two readings: what their counters' differences cover.
func readingSeconds(before *driveReading, after *driveReading) float64 {
	if before == nil || after == nil {
		return 0
	}
	return after.at.Sub(before.at).Seconds()
}

func (self *BenchTarget) read(ctx context.Context, notes *benchNotes) *driveReading {
	reading := &driveReading{at: self.clock().Now()}
	if self.Counters != nil {
		counters, err := self.Counters.ReadDriveCounters(ctx, self.ClusterId)
		if err != nil {
			notes.add(fmt.Sprintf("drive counters unread: %v", err))
		} else {
			reading.counters = counters
		}
	} else {
		notes.add("no drive counter source: device bytes and IOs unread")
	}
	if self.HostStats != nil {
		hostStats, err := self.HostStats.ReadHostStats(ctx, self.ClusterId)
		if err != nil {
			notes.add(fmt.Sprintf("host stats unread: %v", err))
		} else {
			reading.hostStats = hostStats
		}
	} else {
		notes.add("no host stats command: XFS log and file bytes and SMART unread")
	}
	return reading
}

// Each drive's counters between two readings. A drive either reading lacks, or whose counter fell (a reboot, a
// fresh baseline), reads missing.
func driveDeltas(before *driveReading, after *driveReading) []*DriveDelta {
	if after.counters == nil {
		return nil
	}
	beforeDrives := map[string]*server.FileClusterDriveCounters{}
	if before.counters != nil {
		for _, drive := range before.counters.Drives {
			beforeDrives[drive.DriveId] = drive
		}
	}
	deltas := []*DriveDelta{}
	for _, drive := range after.counters.Drives {
		delta := &DriveDelta{DriveId: drive.DriveId, Healing: drive.Healing, Missing: true}
		deltas = append(deltas, delta)
		previous, ok := beforeDrives[drive.DriveId]
		if !ok || previous.Missing || drive.Missing {
			continue
		}
		delta.Healing = delta.Healing || previous.Healing
		if drive.WrittenByteCount < previous.WrittenByteCount || drive.WritesCompleted < previous.WritesCompleted || drive.ReadsCompleted < previous.ReadsCompleted {
			continue
		}
		delta.WrittenByteCount = drive.WrittenByteCount - previous.WrittenByteCount
		delta.WritesCompleted = drive.WritesCompleted - previous.WritesCompleted
		delta.ReadsCompleted = drive.ReadsCompleted - previous.ReadsCompleted
		delta.Missing = false
		if before.hostStats != nil && after.hostStats != nil {
			if start, ok := before.hostStats[drive.DriveId]; ok {
				if end, ok := after.hostStats[drive.DriveId]; ok && start.XfsLogByteCount <= end.XfsLogByteCount && start.XfsFileByteCount <= end.XfsFileByteCount {
					logByteCount := end.XfsLogByteCount - start.XfsLogByteCount
					fileByteCount := end.XfsFileByteCount - start.XfsFileByteCount
					delta.XfsLogByteCount = &logByteCount
					delta.XfsFileByteCount = &fileByteCount
				}
			}
		}
	}
	return deltas
}

// The cluster's raw bytes and inodes between two readings, zero where either is unread.
func usageDeltas(before *driveReading, after *driveReading) (int64, int64) {
	if before.counters == nil || after.counters == nil {
		return 0, 0
	}
	return int64(after.counters.RawUsageByteCount) - int64(before.counters.RawUsageByteCount),
		int64(after.counters.UsedInodeCount) - int64(before.counters.UsedInodeCount)
}

// Each drive's SMART log at a reading, in drive id order of the counters.
func smartOf(reading *driveReading) []*DriveSmart {
	smarts := []*DriveSmart{}
	if reading.hostStats == nil || reading.counters == nil {
		return smarts
	}
	for _, drive := range reading.counters.Drives {
		if stats, ok := reading.hostStats[drive.DriveId]; ok && stats.Smart != nil {
			smart := *stats.Smart
			smart.DriveId = drive.DriveId
			smarts = append(smarts, &smart)
		}
	}
	return smarts
}

// A run's notes, each once, safe for concurrent use.
type benchNotes struct {
	stateLock sync.Mutex
	notes     []string
}

func (self *benchNotes) add(note string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	for _, existing := range self.notes {
		if existing == note {
			return
		}
	}
	self.notes = append(self.notes, note)
}

func (self *benchNotes) list() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]string{}, self.notes...)
}

// The keys one size's objects take under a run's prefix: the size and the object's number at fixed widths, so no
// key prefixes another.
func opsKey(prefix string, objectByteCount int64, n int64) string {
	return fmt.Sprintf("%ss%09d/o%012d", prefix, objectByteCount, n)
}

func opsSizePrefix(prefix string, objectByteCount int64) string {
	return fmt.Sprintf("%ss%09d/", prefix, objectByteCount)
}

// An object's bytes: its number repeated, so each differs and none compresses to nothing.
func opsBody(objectByteCount int64, n int64, generation int) []byte {
	seed := []byte(strconv.FormatInt(n, 36) + "." + strconv.Itoa(generation) + ";")
	return bytes.Repeat(seed, int(objectByteCount)/len(seed)+1)[:objectByteCount]
}

// One size's objects as the run made them: each object's ETag, by number, for If-Match and the deletes.
type opsObjects struct {
	stateLock sync.Mutex
	etags     map[int64]string
}

func (self *opsObjects) set(n int64, etag string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.etags[n] = etag
}

func (self *opsObjects) get(n int64) (string, bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	etag, ok := self.etags[n]
	return etag, ok
}

func (self *opsObjects) remove(n int64) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	delete(self.etags, n)
}

func (self *opsObjects) count() int64 {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return int64(len(self.etags))
}

// Runs the ops mode into measurement: see the file header. prefix is the run's own, ending in "/".
func RunOps(ctx context.Context, target *BenchTarget, settings *OpsSettings, prefix string, measurement *Measurement) error {
	if settings.Rate <= 0 || settings.Duration <= 0 || len(settings.ObjectByteCounts) == 0 {
		return fmt.Errorf("an ops run needs a pace, a window and sizes, not %v, %v and %v", settings.Rate, settings.Duration, settings.ObjectByteCounts)
	}
	clock := target.clock()
	notes := &benchNotes{}
	measurement.Mode = ModeOps
	measurement.ClusterId = target.ClusterId
	measurement.Started = clock.Now()
	defer func() {
		measurement.Ended = clock.Now()
		measurement.Notes = append(measurement.Notes, notes.list()...)
	}()

	// the background
	idleStart := target.read(ctx, notes)
	measurement.SmartBefore = smartOf(idleStart)
	if idleStart.counters != nil {
		measurement.DriveCount = len(idleStart.counters.Drives)
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-clock.After(settings.IdleWindow):
	}
	idleEnd := target.read(ctx, notes)
	measurement.Idle = &IdleWindow{Seconds: readingSeconds(idleStart, idleEnd), Drives: driveDeltas(idleStart, idleEnd)}

	sizeObjects := map[int64]*opsObjects{}
	for _, objectByteCount := range settings.ObjectByteCounts {
		sizeObjects[objectByteCount] = &opsObjects{etags: map[int64]string{}}
	}
	defer func() {
		// what a failed or cancelled class left, best effort and never measured
		cleanupCtx := context.WithoutCancel(ctx)
		if err := cleanupPrefix(cleanupCtx, target.Cluster, prefix, settings.CleanupListLimit, settings.CleanupInFlight); err != nil {
			notes.add(fmt.Sprintf("cleanup left objects under %s: %v", prefix, err))
		}
	}()

	// one class's window: the counters on either side of the driver's run
	window := func(class string, objectByteCount int64, workload load.Workload) error {
		before := target.read(ctx, notes)
		driver := load.NewDriver(&load.DriverSettings{
			Name:            fmt.Sprintf("%s-%d", class, objectByteCount),
			Rate:            settings.Rate,
			Steady:          settings.Duration,
			MaxInFlight:     settings.MaxInFlight,
			MaxFailureKinds: settings.MaxFailureKinds,
			Clock:           clock,
		}, workload)
		driverReport := driver.Run(ctx)
		after := target.read(ctx, notes)
		rawUsageDelta, inodeDelta := usageDeltas(before, after)
		measurement.Ops = append(measurement.Ops, &OpsResult{
			Class:           class,
			ObjectByteCount: objectByteCount,
			Seconds:         readingSeconds(before, after),
			Count:           driverReport.Succeeded,
			Failed:          driverReport.Failed,
			LatencyMillis:   driverReport.LatencyMillis,
			Drives:          driveDeltas(before, after),
			RawUsageDelta:   rawUsageDelta,
			InodeDelta:      inodeDelta,
		})
		for kind, count := range driverReport.FailureKinds {
			notes.add(fmt.Sprintf("%s at %d B: %d failed: %s", class, objectByteCount, count, kind))
		}
		return ctx.Err()
	}

	for _, objectByteCount := range settings.ObjectByteCounts {
		objects := sizeObjects[objectByteCount]
		err := window(OpsClassCreate, objectByteCount, func(ctx context.Context, n int64) error {
			etag, err := target.Cluster.PutIfNoneMatch(ctx, opsKey(prefix, objectByteCount, n), opsBody(objectByteCount, n, 0), nil)
			if err != nil {
				return err
			}
			objects.set(n, etag)
			return nil
		})
		if err != nil {
			return err
		}
		created := objects.count()
		if created == 0 {
			return fmt.Errorf("no object of %d B was created", objectByteCount)
		}
		err = window(OpsClassOverwrite, objectByteCount, func(ctx context.Context, n int64) error {
			// every arrival overwrites an object the create made, round the ones it made
			object := n % created
			etag, ok := objects.get(object)
			if !ok {
				return errors.New("no object to overwrite")
			}
			newEtag, err := target.Cluster.PutIfMatch(ctx, opsKey(prefix, objectByteCount, object), etag, opsBody(objectByteCount, object, int(n/created)+1), nil)
			if err != nil {
				return err
			}
			objects.set(object, newEtag)
			return nil
		})
		if err != nil {
			return err
		}
	}

	readObjects := sizeObjects[settings.ReadObjectByteCount]
	if readObjects != nil && 0 < readObjects.count() {
		readCount := readObjects.count()
		err := window(OpsClassGet, settings.ReadObjectByteCount, func(ctx context.Context, n int64) error {
			object, err := target.Cluster.Get(ctx, opsKey(prefix, settings.ReadObjectByteCount, n%readCount))
			if err != nil {
				return err
			}
			// the whole body, so a GET's time is its last byte's
			_, err = io.Copy(io.Discard, object.Body)
			object.Body.Close()
			return err
		})
		if err != nil {
			return err
		}
		// pages from where a draw lands, seeded so a run repeats
		listRandomLock := sync.Mutex{}
		listRandom := rand.New(rand.NewPCG(0xb3, 0x7c))
		err = window(OpsClassList, settings.ReadObjectByteCount, func(ctx context.Context, n int64) error {
			start := func() int64 {
				listRandomLock.Lock()
				defer listRandomLock.Unlock()
				return listRandom.Int64N(readCount)
			}()
			_, _, err := target.Cluster.List(ctx, opsSizePrefix(prefix, settings.ReadObjectByteCount), opsKey(prefix, settings.ReadObjectByteCount, start), settings.ListPageCount)
			return err
		})
		if err != nil {
			return err
		}
	} else {
		notes.add(fmt.Sprintf("no %d B objects to read or list: GET and LIST not run", settings.ReadObjectByteCount))
	}

	for _, objectByteCount := range settings.ObjectByteCounts {
		objects := sizeObjects[objectByteCount]
		err := window(OpsClassDelete, objectByteCount, func(ctx context.Context, n int64) error {
			if _, ok := objects.get(n); !ok {
				// a delete of a key never made writes nothing worth pricing
				return errors.New("no object left to delete")
			}
			if err := target.Cluster.Delete(ctx, opsKey(prefix, objectByteCount, n)); err != nil {
				return err
			}
			objects.remove(n)
			return nil
		})
		if err != nil {
			return err
		}
	}
	measurement.SmartAfter = smartOf(target.read(ctx, notes))

	if settings.Duration < settings.EvidenceDuration {
		measurement.Smoke = true
		notes.add(fmt.Sprintf("each class ran %s, under D-TEST-11's %s: a smoke", settings.Duration, settings.EvidenceDuration))
	}
	for _, result := range measurement.Ops {
		if anyMissing(result.Drives) {
			measurement.Smoke = true
			notes.add("a window read no drive's counters, so nothing can be priced: a smoke")
			break
		}
	}
	return nil
}

// Deletes every object under prefix, inFlight at a time, best effort: what it could not delete stays, counted in the
// error.
func cleanupPrefix(ctx context.Context, cluster sidecar.BlobCluster, prefix string, listLimit int, inFlight int) error {
	failed := 0
	startAfter := ""
	for {
		objects, more, err := cluster.List(ctx, prefix, startAfter, listLimit)
		if err != nil {
			return err
		}
		if len(objects) == 0 {
			break
		}
		work := make(chan string)
		var wait sync.WaitGroup
		failedLock := sync.Mutex{}
		for range max(1, inFlight) {
			wait.Go(func() {
				for key := range work {
					if err := cluster.Delete(ctx, key); err != nil {
						func() {
							failedLock.Lock()
							defer failedLock.Unlock()
							failed += 1
						}()
					}
				}
			})
		}
		for _, object := range objects {
			work <- object.Key
		}
		close(work)
		wait.Wait()
		startAfter = objects[len(objects)-1].Key
		if !more {
			break
		}
	}
	if 0 < failed {
		return fmt.Errorf("%d deletes failed", failed)
	}
	return nil
}
