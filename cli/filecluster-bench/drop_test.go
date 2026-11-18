package main

import (
	"context"
	"fmt"
	"io"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
)

// A cluster that counts what reaches it: each GET 16 drive reads and each HEAD 16, as §19.6's basis asks every
// drive; each DELETE 16 drive writes; each PUT 16 writes and its bytes over 4 shards on the busiest drive plus 2 KiB.
type dropCountingCluster struct {
	sidecar.BlobCluster

	stateLock sync.Mutex
	reads     uint64
	writes    uint64
	busiest   uint64
}

func (self *dropCountingCluster) add(reads uint64, writes uint64, bytes uint64) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.reads += reads
	self.writes += writes
	self.busiest += bytes
}

func (self *dropCountingCluster) Get(ctx context.Context, key string) (*sidecar.BlobObject, error) {
	object, err := self.BlobCluster.Get(ctx, key)
	if err == nil {
		self.add(16, 0, 0)
	}
	return object, err
}

func (self *dropCountingCluster) Head(ctx context.Context, key string) (*sidecar.BlobInfo, error) {
	info, err := self.BlobCluster.Head(ctx, key)
	if err == nil {
		self.add(16, 0, 0)
	}
	return info, err
}

func (self *dropCountingCluster) Delete(ctx context.Context, key string) error {
	err := self.BlobCluster.Delete(ctx, key)
	if err == nil {
		self.add(0, 16, 3584)
	}
	return err
}

func (self *dropCountingCluster) PutIfNoneMatch(ctx context.Context, key string, body []byte, options *sidecar.BlobPutOptions) (string, error) {
	etag, err := self.BlobCluster.PutIfNoneMatch(ctx, key, body, options)
	if err == nil {
		self.add(0, 16, uint64(len(body))/4+2048)
	}
	return etag, err
}

func (self *dropCountingCluster) PutIfMatch(ctx context.Context, key string, etag string, body []byte, options *sidecar.BlobPutOptions) (string, error) {
	newEtag, err := self.BlobCluster.PutIfMatch(ctx, key, etag, body, options)
	if err == nil {
		self.add(0, 16, uint64(len(body))/4+2048)
	}
	return newEtag, err
}

func (self *dropCountingCluster) PutStream(ctx context.Context, key string, body io.Reader, byteCount int64, checksumSha256 []byte) (string, error) {
	etag, err := self.BlobCluster.PutStream(ctx, key, body, byteCount, checksumSha256)
	if err == nil {
		self.add(0, 16, uint64(byteCount)/4+2048)
	}
	return etag, err
}

// The cluster's counts as 16 drives, one of them busiest by bytes.
type dropCountingCounters struct {
	cluster *dropCountingCluster
}

func (self *dropCountingCounters) ReadDriveCounters(ctx context.Context, clusterId string) (*server.FileClusterCounterReading, error) {
	self.cluster.stateLock.Lock()
	defer self.cluster.stateLock.Unlock()
	reading := &server.FileClusterCounterReading{}
	for i := range 16 {
		drive := &server.FileClusterDriveCounters{
			DriveId:         fmt.Sprintf("0-0-%d", i),
			ReadsCompleted:  self.cluster.reads / 16,
			WritesCompleted: self.cluster.writes / 16,
		}
		if i == 0 {
			drive.WrittenByteCount = self.cluster.busiest
		}
		reading.Drives = append(reading.Drives, drive)
	}
	return reading, nil
}

func testDropBenchSettings() *DropBenchSettings {
	settings := DefaultDropBenchSettings()
	settings.ScanObjectByteCount = 1000 * 1000
	settings.ScanObjectCount = 8
	settings.CandidateCount = 20
	settings.CandidateByteCount = 1024
	settings.TableOverwriteCount = 6
	settings.SlotDeleteCount = 10
	return settings
}

func newDropTestTargets(clock *sidecartest.FakeClock) (*BenchTarget, *BenchTarget, *dropCountingCluster, *dropCountingCluster, *sidecartest.FakeBlobs) {
	blobs := sidecartest.NewFakeBlobs(sidecartest.NewFakeClock(sidecartest.DefaultClockStart()), nil, nil, nil)
	segmentCluster, _ := blobs.Cluster("seg-1")
	metadataCluster, _ := blobs.Cluster("meta-1")
	segment := &dropCountingCluster{BlobCluster: segmentCluster}
	metadata := &dropCountingCluster{BlobCluster: metadataCluster}
	return &BenchTarget{ClusterId: "seg-1", Cluster: segment, Store: blobs, Counters: &dropCountingCounters{cluster: segment}, Clock: queuedClock{FakeClock: clock}},
		&BenchTarget{ClusterId: "meta-1", Cluster: metadata, Store: blobs, Counters: &dropCountingCounters{cluster: metadata}, Clock: queuedClock{FakeClock: clock}},
		segment, metadata, blobs
}

// The drop mode runs each window on its cluster at the job's paces, the DELETEs naming what the HEADs read, and
// leaves nothing under its prefix; the figures come from the windows' counters.
func TestDropBenchRunsEachWindowAndComputesTheRow(t *testing.T) {
	clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
	segment, metadata, segmentCluster, _, blobs := newDropTestTargets(clock)
	settings := testDropBenchSettings()
	measurement := &DropBenchMeasurement{RowHeader: RowHeader{CommitSet: fixtureCommitSet(), Settings: map[string]string{}}}
	prefix := "filecluster-bench/0123456789abcdef/"
	done := make(chan struct{})
	var runErr error
	go func() {
		defer close(done)
		runErr = RunDropBench(context.Background(), segment, metadata, settings, prefix, measurement)
	}()
	advanceUntil(t, clock, 50*time.Millisecond, done)
	if runErr != nil {
		t.Fatal(runErr, measurement.Notes)
	}
	want := map[string]int64{
		DropWindowScanPaced: 8, DropWindowScanFull: 8, DropWindowHead: 20, DropWindowDelete: 20, DropWindowTable: 6, DropWindowSlotDelete: 10,
	}
	for name, count := range want {
		window := measurement.Window(name)
		if window == nil || window.Count != count || window.Failed != 0 {
			t.Fatalf("%s: %+v", name, window)
		}
	}
	if scanned := measurement.Window(DropWindowScanPaced).ByteCount; scanned != 8*1000*1000 {
		t.Errorf("the paced scan read %d B", scanned)
	}
	for _, clusterId := range []string{"seg-1", "meta-1"} {
		cluster, _ := blobs.Cluster(clusterId)
		if objects, _, err := cluster.List(context.Background(), prefix, "", 10); err != nil || len(objects) != 0 {
			t.Errorf("%s keeps %d objects under the prefix: %v", clusterId, len(objects), err)
		}
	}
	if !measurement.Smoke {
		t.Error("a run of 8 MB of history is no smoke")
	}
	figures := ComputeDropFigures(measurement, settings)
	// 16 drive operations a GET of 1 MB, a HEAD and a DELETE each, as the fake cluster counts them
	if math.Abs(figures.HddOpsPerScanMegabyte-16) > 0.01 || math.Abs(figures.HddOpsPerHead-16) > 0.01 || math.Abs(figures.HddOpsPerDelete-16) > 0.01 {
		t.Errorf("figures %+v", figures)
	}
	if figures.PacedScanBytesPerSecond <= 0 || float64(settings.Drop.ReadBytesPerSecond)*1.01 < figures.PacedScanBytesPerSecond {
		t.Errorf("the paced scan ran at %.0f B/s, past the job's %d", figures.PacedScanBytesPerSecond, settings.Drop.ReadBytesPerSecond)
	}
	// a 32 KiB table write: its bytes over four shards and 2 KiB on the busiest drive
	if figures.NvmeBytesPerTableWrite != 32*1024/4+2048 || figures.NvmeBytesPerSlotDelete != 3584 {
		t.Errorf("NVMe figures %+v", figures)
	}
	if figures.Evidence || figures.HistoryJobSeconds <= 0 {
		t.Errorf("a smoke's figures read as evidence, or no job time: %+v", figures)
	}
	_ = segmentCluster
}

// The row's computed figures, on windows written by hand: the 60 GB history's job time is its scans at the slower of
// the measured and the paced read rate and its candidates at the slower of the measured and the job's delete rate.
func TestDropFiguresComputeTheHistorysJobTime(t *testing.T) {
	settings := DefaultDropBenchSettings()
	drives := func(reads uint64, writes uint64, bytes uint64) []*DriveDelta {
		return []*DriveDelta{{DriveId: "0-0-0", ReadsCompleted: reads, WritesCompleted: writes, WrittenByteCount: bytes}}
	}
	measurement := &DropBenchMeasurement{
		RowHeader: RowHeader{Mode: ModeDropBench, CommitSet: fixtureCommitSet()},
		Windows: []*DropWindow{
			// 20 MB/s, under the job's 32: the measured rate sets the job's
			{Name: DropWindowScanPaced, Seconds: 100, Count: 500, ByteCount: 2000 * 1000 * 1000, Drives: drives(64000, 0, 0)},
			{Name: DropWindowScanFull, Seconds: 20, Count: 500, ByteCount: 2000 * 1000 * 1000, Drives: drives(64000, 0, 0)},
			{Name: DropWindowHead, Seconds: 60, Count: 600, Drives: drives(9600, 0, 0)},
			{Name: DropWindowDelete, Seconds: 60, Count: 600, Drives: drives(0, 19200, 0)},
			{Name: DropWindowTable, Seconds: 10, Count: 100, Drives: drives(0, 300, 2252800)},
			{Name: DropWindowSlotDelete, Seconds: 20, Count: 200, Drives: drives(0, 600, 716800)},
		},
	}
	figures := ComputeDropFigures(measurement, settings)
	if !figures.Evidence || len(figures.Reasons) != 0 {
		t.Fatalf("figures %+v", figures)
	}
	if figures.PacedScanBytesPerSecond != 20*1000*1000 || figures.FullScanBytesPerSecond != 100*1000*1000 || figures.HddOpsPerScanMegabyte != 32 {
		t.Errorf("scan %+v", figures)
	}
	if figures.HddOpsPerHead != 16 || figures.HddOpsPerDelete != 32 || figures.DeletesPerSecond != 10 {
		t.Errorf("HEAD and DELETE %+v", figures)
	}
	// 20 MB/s at 32 operations a MB, and 10 DELETEs a second with their HEADs at 48
	if figures.HddOpsPerSecondRunning != 20*32+10*48 {
		t.Errorf("a running job's operations %v", figures.HddOpsPerSecondRunning)
	}
	if figures.NvmeBytesPerTableWrite != 22528 || figures.NvmeBytesPerSlotDelete != 3584 || figures.NvmeBytesPerJobHour != 60*22528 {
		t.Errorf("NVMe %+v", figures)
	}
	candidates := 0.25 * 60e9 / (4 * 1024 * 1024)
	wantSeconds := 2.1*60e9/(20*1000*1000) + candidates/10
	if math.Abs(figures.HistoryCandidateCount-candidates) > 1e-6 || math.Abs(figures.HistoryJobSeconds-wantSeconds) > 1e-6 {
		t.Errorf("the history's job: %v candidates in %v s, want %v in %v", figures.HistoryCandidateCount, figures.HistoryJobSeconds, candidates, wantSeconds)
	}
	var text strings.Builder
	if err := WriteDropFigures(&text, measurement, figures); err != nil || !strings.Contains(text.String(), "computed") {
		t.Errorf("the text %q, %v", text.String(), err)
	}
	// a missing window and an unread counter are no evidence
	measurement.Windows = measurement.Windows[1:]
	measurement.Windows[0].Drives[0].Missing = true
	figures = ComputeDropFigures(measurement, settings)
	if figures.Evidence || len(figures.Reasons) < 2 {
		t.Errorf("an incomplete run: %+v", figures)
	}
}

// The pause holds the load while the drives' operations a second since its last read pass its line, reads them at
// most each interval, pauses on unread counters, and keeps the wall time it held once, however many waited on it.
func TestLoadPauseHoldsPastItsLineAndKeepsTheTimeHeld(t *testing.T) {
	clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
	segment, _, cluster, _, _ := newDropTestTargets(clock)
	pause := newLoadPause(segment, 100, time.Second, &benchNotes{})
	ctx := context.Background()
	if !pause.proceed(ctx) {
		t.Fatal("the first read paused")
	}
	// 160 operations in a second, past the line of 100
	cluster.add(80, 80, 0)
	clock.Advance(time.Second)
	if pause.proceed(ctx) {
		t.Fatal("160 operations a second did not pause")
	}
	// within the interval it reads nothing and holds
	clock.Advance(500 * time.Millisecond)
	cluster.add(1000, 0, 0)
	if pause.proceed(ctx) {
		t.Fatal("the pause let go within its interval")
	}
	// the next read: 1,000 in 1.5 s, still past it; then a quiet second
	clock.Advance(500 * time.Millisecond)
	if pause.proceed(ctx) {
		t.Fatal("1,000 operations in 1.5 s did not hold")
	}
	clock.Advance(time.Second)
	if !pause.proceed(ctx) {
		t.Fatal("a quiet second held the load")
	}
	if held := pause.pausedDuration(); held != 2*time.Second {
		t.Fatalf("held %s, want 2s", held)
	}
	// a nil pause, and one with no line, hold nothing
	var none *loadPause
	if !none.proceed(ctx) || none.pausedDuration() != 0 || !newLoadPause(segment, 0, time.Second, &benchNotes{}).proceed(ctx) {
		t.Fatal("a pause with no line held")
	}
}
