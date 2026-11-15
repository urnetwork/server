package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/evsservice"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
)

func fixtureHddWindow(name string, putRate float64, getRate float64, driveOps float64, refused int64) *HddWindow {
	return &HddWindow{
		Name: name, OfferedPutsPerSecond: putRate, OfferedGetsPerSecond: getRate, Seconds: 3600,
		PutsStored: int64(putRate * 3600), PutsRefused: refused, GetsServed: int64(getRate * 3600),
		DriveOpsPerSecond: driveOps, DriveReads: uint64(getRate * 3600 * 4), DrivesRead: true, CacheDropped: true,
	}
}

// A full hdd run: steps rising to a plateau at 6,500 drive operations a second, year one admitting everything,
// year two refusing 2% and a search step at 324 PUTs a second refusing none.
func fixtureHdd() *HddMeasurement {
	measurement := &HddMeasurement{RowHeader: RowHeader{Mode: ModeHdd, CommitSet: fixtureCommitSet(), ClusterId: "seg-1"}}
	for i, ops := range []float64{3000, 4200, 5600, 6400, 6500} {
		measurement.CapacitySteps = append(measurement.CapacitySteps, fixtureHddWindow(fmt.Sprintf("capacity-%02d", i), 200, 800, ops, 0))
	}
	measurement.YearOne = fixtureHddWindow("year-one", 40, 160, 1200, 0)
	measurement.YearTwo = fixtureHddWindow("year-two", 400, 1600, 3300, 400*36*2)
	measurement.Search = []*HddWindow{fixtureHddWindow("search-00", 360, 1440, 3200, 360*36), fixtureHddWindow("search-01", 324, 1296, 3100, 0)}
	measurement.AdmissionCapacityOps = 6500
	return measurement
}

func TestHddVerdictFindsThePlateauAndTheAdmittedThroughput(t *testing.T) {
	verdict := JudgeHdd(fixtureHdd(), DefaultHddSettings())
	if !verdict.Pass || !verdict.PlateauReached || verdict.CapacityOps != 6500 || !verdict.YearOneAdmitsAll {
		t.Fatalf("verdict %+v", verdict)
	}
	// year two refused 2% and the first search step 1%: the second, at 324 PUTs a second with none refused, is it
	if verdict.AdmittedPutsPerSecond != 324 {
		t.Fatalf("admitted %v", verdict.AdmittedPutsPerSecond)
	}
	if want := 1 - 4.0/16; verdict.YearTwoCacheHit != want {
		t.Fatalf("cache hit %v, not %v", verdict.YearTwoCacheHit, want)
	}
	var text strings.Builder
	if err := WriteHddVerdict(&text, fixtureHdd(), verdict, DefaultHddSettings()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text.String(), "file_clusters: {seg-1: {hdd_capacity_ops: 6500}}") {
		t.Fatalf("no capacity line:\n%s", text.String())
	}
}

func TestHddPlateauIsTheLargerOfItsLastTwoSteps(t *testing.T) {
	steps := []*HddWindow{
		{DriveOpsPerSecond: 3000, DrivesRead: true},
		{DriveOpsPerSecond: 5000, DrivesRead: true},
		// overload: the next step achieved less
		{DriveOpsPerSecond: 4900, DrivesRead: true},
	}
	if index, ok := hddCapacityPlateau(steps, 0.05); !ok || index != 1 {
		t.Fatalf("plateau at %d, %t", index, ok)
	}
	// still rising: no plateau
	if _, ok := hddCapacityPlateau(steps[:2], 0.05); ok {
		t.Fatal("a plateau in rising steps")
	}
	steps[1].DrivesRead = false
	if _, ok := hddCapacityPlateau(steps, 0.05); ok {
		t.Fatal("a plateau from an unread step")
	}
}

func TestHddVerdictFailsEachShortfall(t *testing.T) {
	cases := []struct {
		reason string
		mutate func(measurement *HddMeasurement)
	}{
		{"smoke", func(measurement *HddMeasurement) { measurement.Smoke = true }},
		{"names no build", func(measurement *HddMeasurement) { measurement.CommitSet = nil }},
		{"no plateau", func(measurement *HddMeasurement) { measurement.CapacitySteps = measurement.CapacitySteps[:3] }},
		{"page cache", func(measurement *HddMeasurement) { measurement.CapacitySteps[1].CacheDropped = false }},
		{"must admit every one", func(measurement *HddMeasurement) { measurement.YearOne.PutsRefused = 1 }},
		{"no PUT rate", func(measurement *HddMeasurement) { measurement.Search = measurement.Search[:1] }},
		{"no year two", func(measurement *HddMeasurement) { measurement.YearTwo = nil }},
	}
	for _, c := range cases {
		measurement := fixtureHdd()
		c.mutate(measurement)
		verdict := JudgeHdd(measurement, DefaultHddSettings())
		if verdict.Pass || !strings.Contains(strings.Join(verdict.Reasons, "; "), c.reason) {
			t.Errorf("%s: pass %t, reasons %v", c.reason, verdict.Pass, verdict.Reasons)
		}
	}
}

// HDD counters whose drive operations follow each window's offered rate up to a ceiling: each PUT 16 drive
// operations and each GET 16, as §19.6's basis, the cluster saturating at ceiling a second.
type saturatingHddCounters struct {
	clock   *sidecartest.FakeClock
	ceiling float64
	puts    atomic.Int64
	gets    atomic.Int64

	stateLock  sync.Mutex
	lastAt     time.Time
	operations float64
	lastCount  int64
}

func (self *saturatingHddCounters) ReadDriveCounters(ctx context.Context, clusterId string) (*server.FileClusterCounterReading, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	now := self.clock.Now()
	count := self.puts.Load() + self.gets.Load()
	if !self.lastAt.IsZero() {
		if seconds := now.Sub(self.lastAt).Seconds(); 0 < seconds {
			self.operations += min(float64(count-self.lastCount)*16, self.ceiling*seconds)
		}
	}
	self.lastAt = now
	self.lastCount = count
	reading := &server.FileClusterCounterReading{}
	for i := range 16 {
		reading.Drives = append(reading.Drives, &server.FileClusterDriveCounters{
			DriveId: fmt.Sprintf("0-%d-%d", i/16, i), ReadsCompleted: uint64(self.operations / 32), WritesCompleted: uint64(self.operations / 32),
		})
	}
	return reading, nil
}

// An admission refusing every 40th PUT of a window past 100 PUTs a second, by a count the test sets per window.
type fakeHddAdmission struct {
	stateLock sync.Mutex
	admitted  int64
	refuseAll bool
	puts      atomic.Int64
	closed    bool
}

func (self *fakeHddAdmission) Admit(networkId connect.Id, byteCount int64, class evsservice.HddUploadClass) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.admitted += 1
	if self.refuseAll && self.admitted%40 == 0 {
		return &evsservice.HddRefusal{ClusterId: "seg-1", RetryAfter: 5 * time.Second}
	}
	return nil
}

func (self *fakeHddAdmission) CountPut() { self.puts.Add(1) }

func (self *fakeHddAdmission) Close() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.closed = true
}

func TestHddRunStepsToThePlateauThenRunsTheMixUnderAdmission(t *testing.T) {
	clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
	blobs := sidecartest.NewFakeBlobs(sidecartest.NewFakeClock(sidecartest.DefaultClockStart()), nil, nil, nil)
	segment, err := blobs.Cluster("seg-1")
	if err != nil {
		t.Fatal(err)
	}
	counters := &saturatingHddCounters{clock: clock, ceiling: 4000}
	cluster := &hddCountingCluster{BlobCluster: segment, counters: counters}
	target := &BenchTarget{ClusterId: "seg-1", Cluster: cluster, Store: blobs, Counters: counters, Clock: queuedClock{FakeClock: clock}}
	settings := DefaultHddSettings()
	settings.ObjectByteCount = 1024
	settings.CapacityStartRate = 100
	settings.CapacityStepDuration = 2 * time.Second
	settings.MixDuration = 2 * time.Second
	settings.SearchDuration = 2 * time.Second
	settings.YearOnePutsPerSecond, settings.YearOneGetsPerSecond = 4, 16
	settings.YearTwoPutsPerSecond, settings.YearTwoGetsPerSecond = 40, 160
	settings.AdmissionSettle = time.Second
	admission := &fakeHddAdmission{}
	dropped := atomic.Int64{}
	measurement := &HddMeasurement{RowHeader: RowHeader{CommitSet: fixtureCommitSet(), Settings: map[string]string{}}}
	done := make(chan struct{})
	var runErr error
	go func() {
		defer close(done)
		runErr = RunHdd(context.Background(), target, settings, "filecluster-bench/0123456789abcdef/",
			func(ctx context.Context) error { dropped.Add(1); return nil }, 0,
			func(ctx context.Context, capacityOps int64) (hddAdmitter, error) {
				if capacityOps <= 0 {
					return nil, fmt.Errorf("capacity %d", capacityOps)
				}
				return admission, nil
			}, measurement)
	}()
	advanceUntil(t, clock, 50*time.Millisecond, done)
	if runErr != nil {
		t.Fatal(runErr, measurement.Notes)
	}
	// 100, 125, 156, ... operations a second at 16 drive operations each rise toward the 4,000 ceiling, where a step
	// under 5% above the last ends the search; how many steps that takes depends on how far the fake clock ran past
	// each window's end while its last reading was taken, so the plateau rule's own steps are hddCapacityPlateau's
	// test, and this one holds the run's shape
	if len(measurement.CapacitySteps) < 2 {
		t.Fatalf("%d steps", len(measurement.CapacitySteps))
	}
	for _, step := range measurement.CapacitySteps {
		if !step.DrivesRead || !step.CacheDropped || 4000 < step.DriveOpsPerSecond {
			t.Fatalf("step %+v", step)
		}
	}
	if int64(dropped.Load()) != int64(len(measurement.CapacitySteps)) {
		t.Fatalf("dropped the cache %d times over %d steps", dropped.Load(), len(measurement.CapacitySteps))
	}
	verdict := JudgeHdd(measurement, settings)
	if !verdict.PlateauReached || verdict.CapacityOps <= 0 || measurement.AdmissionCapacityOps != verdict.CapacityOps {
		t.Fatalf("verdict %+v, admission against %d", verdict, measurement.AdmissionCapacityOps)
	}
	if measurement.YearOne == nil || measurement.YearOne.PutsRefused != 0 || measurement.YearOne.PutsStored != 8 {
		t.Fatalf("year one %+v", measurement.YearOne)
	}
	if !verdict.YearOneAdmitsAll || verdict.AdmittedPutsPerSecond <= 0 {
		t.Fatalf("verdict %+v", verdict)
	}
	if !measurement.Smoke {
		t.Fatal("2 s steps were not a smoke")
	}
	left, _, err := segment.List(context.Background(), "filecluster-bench/", "", 10)
	if err != nil || len(left) != 0 {
		t.Fatalf("%d objects left, %v", len(left), err)
	}
}

func TestHddRunSearchesDownWhileRefusalsReachTheShare(t *testing.T) {
	clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
	blobs := sidecartest.NewFakeBlobs(sidecartest.NewFakeClock(sidecartest.DefaultClockStart()), nil, nil, nil)
	segment, _ := blobs.Cluster("seg-1")
	counters := &saturatingHddCounters{clock: clock, ceiling: 100000}
	target := &BenchTarget{ClusterId: "seg-1", Cluster: &hddCountingCluster{BlobCluster: segment, counters: counters}, Store: blobs, Counters: counters, Clock: queuedClock{FakeClock: clock}}
	settings := DefaultHddSettings()
	settings.ObjectByteCount = 1024
	settings.MixDuration = 10 * time.Second
	settings.SearchDuration = 10 * time.Second
	settings.SearchMaxSteps = 3
	settings.YearOnePutsPerSecond, settings.YearOneGetsPerSecond = 2, 8
	settings.YearTwoPutsPerSecond, settings.YearTwoGetsPerSecond = 8, 32
	settings.AdmissionSettle = time.Second
	// every window refuses a fortieth of its PUTs, 2.5%, so every search step runs and none is admitted
	admission := &fakeHddAdmission{refuseAll: true}
	measurement := &HddMeasurement{RowHeader: RowHeader{CommitSet: fixtureCommitSet(), Settings: map[string]string{}}}
	done := make(chan struct{})
	var runErr error
	go func() {
		defer close(done)
		runErr = RunHdd(context.Background(), target, settings, "filecluster-bench/fedcba9876543210/", nil, 5000,
			func(ctx context.Context, capacityOps int64) (hddAdmitter, error) { return admission, nil }, measurement)
	}()
	advanceUntil(t, clock, 50*time.Millisecond, done)
	if runErr != nil {
		t.Fatal(runErr)
	}
	if len(measurement.CapacitySteps) != 0 || measurement.AdmissionCapacityOps != 5000 {
		t.Fatalf("ran %d capacity steps against a given capacity", len(measurement.CapacitySteps))
	}
	if len(measurement.Search) != 3 {
		t.Fatalf("%d search steps", len(measurement.Search))
	}
	verdict := JudgeHdd(measurement, settings)
	if verdict.Pass || verdict.AdmittedPutsPerSecond != 0 {
		t.Fatalf("verdict %+v", verdict)
	}
	if !admission.closed {
		t.Fatal("the admission was not closed")
	}
}

// The segment cluster's PutStream and Get, counted for the saturating counters.
type hddCountingCluster struct {
	sidecar.BlobCluster
	counters *saturatingHddCounters
}

func (self *hddCountingCluster) PutStream(ctx context.Context, key string, body io.Reader, byteCount int64, checksumSha256 []byte) (string, error) {
	etag, err := self.BlobCluster.PutStream(ctx, key, body, byteCount, checksumSha256)
	if err == nil {
		self.counters.puts.Add(1)
	}
	return etag, err
}

func (self *hddCountingCluster) Get(ctx context.Context, key string) (*sidecar.BlobObject, error) {
	object, err := self.BlobCluster.Get(ctx, key)
	if err == nil {
		self.counters.gets.Add(1)
	}
	return object, err
}
