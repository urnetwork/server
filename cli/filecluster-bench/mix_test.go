package main

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
)

// A mix target whose pushes answer at once: arrival n%25 == 3 refused by the budget and n%25 == 7 at FOLD_BEHIND,
// the rest committed, with each store operation taking 5 ms.
type fakeMixTarget struct {
	restarts atomic.Int64
	pushes   atomic.Int64
	missing  []string
}

func (self *fakeMixTarget) Push(ctx context.Context, arrival int64) error {
	self.pushes.Add(1)
	switch arrival % 25 {
	case 3:
		return &sidecar.EvsError{Status: 429, Code: protocol.EvsRefusalCode_EVS_REFUSAL_RESOURCE_EXHAUSTED}
	case 7:
		return &sidecar.EvsError{Status: 429, Code: protocol.EvsRefusalCode_EVS_REFUSAL_FOLD_BEHIND}
	}
	return nil
}

func (self *fakeMixTarget) Restart(ctx context.Context) error {
	self.restarts.Add(1)
	return nil
}

func (self *fakeMixTarget) Events() ([]string, []string) {
	driven := []string{}
	for _, event := range MixEvents {
		if !strings.Contains(strings.Join(self.missing, ","), event) {
			driven = append(driven, event)
		}
	}
	return driven, self.missing
}

func (self *fakeMixTarget) TakeOperationLatencies() map[string][]time.Duration {
	return map[string][]time.Duration{StoreOperationPut: {5 * time.Millisecond, 5 * time.Millisecond}}
}

func (self *fakeMixTarget) MetadataClusterIds() []string {
	return []string{"meta-1"}
}

func (self *fakeMixTarget) Close() {}

// The stack's series as rates over the fake clock since start: a shed a second, two FOLD_BEHIND refusals a second,
// 1,000 priced P1 bytes and 10 of P0 rotation a second, and fixed levels.
type fakeMixObserver struct {
	clock *sidecartest.FakeClock
	start time.Time
}

func (self *fakeMixObserver) Observe(ctx context.Context) ([]*metricSample, error) {
	seconds := math.Floor(self.clock.Now().Sub(self.start).Seconds())
	return []*metricSample{
		{Name: seriesBudgetShed, Labels: map[string]string{"cluster": "meta-1"}, Value: seconds},
		{Name: seriesEvsRefusals, Labels: map[string]string{"route": "push", "code": protocol.EvsRefusalCode_EVS_REFUSAL_FOLD_BEHIND.String()}, Value: 2 * seconds},
		{Name: seriesEvsRefusals, Labels: map[string]string{"route": "push", "code": protocol.EvsRefusalCode_EVS_REFUSAL_BUSY.String()}, Value: 7 * seconds},
		{Name: seriesBudgetIssued, Labels: map[string]string{"cluster": "meta-1", "class": "p1", "kind": ""}, Value: 1000 * seconds},
		{Name: seriesBudgetIssued, Labels: map[string]string{"cluster": "meta-1", "class": "p0", "kind": "rotation"}, Value: 10 * seconds},
		{Name: seriesBudgetP0Refill, Labels: map[string]string{"cluster": "meta-1"}, Value: 0.25},
		{Name: seriesOldestBatch, Labels: map[string]string{"cluster": "meta-1"}, Value: 7200},
	}, nil
}

// Drive counters that rise with the fake clock: the busiest drive writes byteRate a second, the others nine tenths.
type clockedCounters struct {
	clock    *sidecartest.FakeClock
	start    time.Time
	byteRate float64
}

func (self *clockedCounters) ReadDriveCounters(ctx context.Context, clusterId string) (*server.FileClusterCounterReading, error) {
	seconds := self.clock.Now().Sub(self.start).Seconds()
	reading := &server.FileClusterCounterReading{RawCapacityByteCount: 1000 * 1000 * 1000 * 1000, FreeInodeCount: 1000 * 1000}
	for i := range fixtureDriveCount {
		written := uint64(self.byteRate * seconds)
		if i != 0 {
			written = written * 9 / 10
		}
		reading.Drives = append(reading.Drives, &server.FileClusterDriveCounters{
			DriveId: fmt.Sprintf("0-0-%d", i), WrittenByteCount: written, WritesCompleted: uint64(100 * seconds),
		})
	}
	return reading, nil
}

func testMixSettings() *MixSettings {
	settings := DefaultMixSettings()
	settings.YearOnePushesPerSecond = 2
	settings.Preload = 0
	settings.Warmup = time.Second
	settings.Steady = 10 * time.Second
	settings.SampleInterval = time.Second
	return settings
}

func runTestMix(t *testing.T, target MixTarget, byteRate float64) (*Measurement, *sidecartest.FakeClock) {
	t.Helper()
	clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
	blobs := sidecartest.NewFakeBlobs(sidecartest.NewFakeClock(sidecartest.DefaultClockStart()), nil, nil, nil)
	cluster, err := blobs.Cluster("meta-1")
	if err != nil {
		t.Fatal(err)
	}
	bench := &BenchTarget{
		ClusterId: "meta-1",
		Cluster:   cluster,
		Store:     blobs,
		Counters:  &clockedCounters{clock: clock, start: clock.Now(), byteRate: byteRate},
		Clock:     queuedClock{FakeClock: clock},
	}
	measurement := &Measurement{BlockByteCount: 1024, DataShardCount: 4, Settings: map[string]string{}}
	done := make(chan struct{})
	var runErr error
	go func() {
		defer close(done)
		runErr = RunMix(context.Background(), bench, target, &fakeMixObserver{clock: clock, start: clock.Now()}, testMixSettings(), measurement)
	}()
	advanceUntil(t, clock, 100*time.Millisecond, done)
	if runErr != nil {
		t.Fatal(runErr)
	}
	return measurement, clock
}

func TestMixRunRecordsEachPhaseFromItsMeasuredWindow(t *testing.T) {
	target := &fakeMixTarget{missing: []string{}}
	// 3 MiB a second on the busiest drive, so the limit's pace stays small
	byteRate := 3.0 * 1024 * 1024
	measurement, _ := runTestMix(t, target, byteRate)
	names := []string{}
	for _, phase := range measurement.Phases {
		names = append(names, phase.Name)
	}
	if strings.Join(names, ",") != "1x,2x,limit" {
		t.Fatalf("phases %v, notes %v", names, measurement.Notes)
	}
	twice := measurement.Phase(MixPhaseTwiceYear)
	// 4 pushes a second: arrivals 0 to 3 warm up, 4 to 43 are measured; 28 is refused by the budget and 7, 32 at
	// FOLD_BEHIND, of the measured ones
	if twice.OfferedPushesPerSecond != 4 || twice.PushesRefusedByBudget != 1 || twice.PushesFoldBehind != 2 || twice.PushesFailed != 0 {
		t.Fatalf("2x: %v offered, %d refused, %d fold behind, %d failed", twice.OfferedPushesPerSecond, twice.PushesRefusedByBudget, twice.PushesFoldBehind, twice.PushesFailed)
	}
	if twice.PushesCommitted != 37 {
		t.Fatalf("2x committed %d", twice.PushesCommitted)
	}
	// the window is exactly 10 s on the fake clock, its series' deltas the rates times it
	if twice.Seconds != 10 || !twice.SeriesRead || twice.BudgetShedCount != 10 || twice.FoldBehindCount != 20 || twice.PricedByteCount != 10100 {
		t.Fatalf("2x over %v s: series %t, %d sheds, %d fold behind, %d priced", twice.Seconds, twice.SeriesRead, twice.BudgetShedCount, twice.FoldBehindCount, twice.PricedByteCount)
	}
	if twice.P0PricedByteCounts["rotation"] != 100 || twice.P0RefillRatioMax != 0.25 || twice.OldestBatchSecondsMax != 7200 {
		t.Fatalf("2x P0 %v, ratio %v, oldest %v", twice.P0PricedByteCounts, twice.P0RefillRatioMax, twice.OldestBatchSecondsMax)
	}
	busiest, ok := busiestDrive(twice.Drives)
	if !ok || busiest.WrittenByteCount != uint64(byteRate*10) || anyMissing(twice.Drives) {
		t.Fatalf("2x busiest %+v", busiest)
	}
	if !twice.RestartIncluded || target.restarts.Load() != 3 {
		t.Fatalf("restart %t, %d restarts", twice.RestartIncluded, target.restarts.Load())
	}
	if put := twice.OperationLatencyMillis[StoreOperationPut]; put.Count != 2 || put.P99 != 5 {
		t.Fatalf("put latency %+v", put)
	}
	if _, ok := twice.RecordsPerBatch["meta-1"]; !ok {
		t.Fatalf("no records per batch on meta-1: %v", measurement.Notes)
	}

	// the limit's pace: the refill over the 2x phase's bytes per push
	limit := measurement.Phase(MixPhaseAtTheLimit)
	perPush := byteRate * 10 / 37
	wantRate := 0.8 * 1920e9 / 86400 / perPush
	if math.Abs(limit.OfferedPushesPerSecond-wantRate) > 1e-6 {
		t.Fatalf("limit at %v pushes a second, not %v", limit.OfferedPushesPerSecond, wantRate)
	}
	// a 10 s window is a smoke
	if !measurement.Smoke {
		t.Fatal("a 10 s mix was not a smoke")
	}
}

func TestMixRunNamesWhatTheTargetCannotDrive(t *testing.T) {
	target := &fakeMixTarget{missing: []string{MixEventGkmCommit, MixEventRotation}}
	measurement, _ := runTestMix(t, target, 3*1024*1024)
	twice := measurement.Phase(MixPhaseTwiceYear)
	if len(twice.EventsMissing) != 2 || twice.EventsMissing[0] != MixEventGkmCommit {
		t.Fatalf("missing %v", twice.EventsMissing)
	}
	found := false
	for _, note := range measurement.Notes {
		if strings.Contains(note, "cannot drive gkm_commit, rotation") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the notes do not name the missing events: %v", measurement.Notes)
	}
}

func TestMixSeriesSumCountersAndTakeTheLargestLevel(t *testing.T) {
	samples := []*metricSample{
		{Name: seriesBudgetShed, Labels: map[string]string{"cluster": "meta-1"}, Value: 3},
		{Name: seriesBudgetShed, Labels: map[string]string{"cluster": "meta-2"}, Value: 4},
		{Name: seriesEvsRefusals, Labels: map[string]string{"route": "push", "code": protocol.EvsRefusalCode_EVS_REFUSAL_FOLD_BEHIND.String()}, Value: 5},
		{Name: seriesEvsRefusals, Labels: map[string]string{"route": "push", "code": protocol.EvsRefusalCode_EVS_REFUSAL_BUSY.String()}, Value: 9},
		{Name: seriesBudgetIssued, Labels: map[string]string{"cluster": "meta-1", "class": "p0", "kind": "revocation"}, Value: 1.5e6},
		{Name: seriesBudgetIssued, Labels: map[string]string{"cluster": "meta-1", "class": "p1", "kind": ""}, Value: 2e6},
		{Name: seriesBudgetP0Refill, Labels: map[string]string{"cluster": "meta-1"}, Value: 0.4},
		{Name: seriesBudgetP0Refill, Labels: map[string]string{"cluster": "meta-2"}, Value: 0.7},
		{Name: seriesOldestBatch, Labels: map[string]string{"cluster": "meta-1"}, Value: 90000},
		{Name: "process_open_fds", Labels: map[string]string{}, Value: 12},
	}
	series := readMixSeries(samples)
	if series.shedCount != 7 || series.foldBehindCount != 5 || series.pricedByteCount != 3.5e6 || series.p0PricedByteCounts["revocation"] != 1.5e6 {
		t.Fatalf("series %+v", series)
	}
	if series.p0RefillRatio != 0.7 || series.oldestBatchSeconds != 90000 {
		t.Fatalf("levels %v and %v", series.p0RefillRatio, series.oldestBatchSeconds)
	}
}

func TestBenchMetricsReadBackAsSamples(t *testing.T) {
	metrics := newBenchMetrics()
	issued := metrics.Counter(seriesBudgetIssued, "", "cluster", "class", "kind")
	issued.Add(100, "meta-1", "p0", "rotation")
	issued.Add(50, "meta-1", "p0", "rotation")
	issued.Add(7, "meta-1", "p1", "")
	metrics.Gauge(seriesBudgetP0Refill, "", "cluster").Set(0.3, "meta-1")
	metrics.Histogram("urnetwork_evs_request_seconds", "", nil, "route").Observe(0.25, "push")
	series := readMixSeries(metrics.samples())
	if series.pricedByteCount != 157 || series.p0PricedByteCounts["rotation"] != 150 || series.p0RefillRatio != 0.3 {
		t.Fatalf("series %+v", series)
	}
}

func TestTimedBlobsTimesAnswersAndNotFailuresToAnswer(t *testing.T) {
	clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
	blobs := sidecartest.NewFakeBlobs(sidecartest.NewFakeClock(sidecartest.DefaultClockStart()), nil, nil, nil)
	timed := newTimedBlobs(blobs, 16, clock)
	cluster, err := timed.Cluster("meta-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cluster.PutIfNoneMatch(context.Background(), "a/_", []byte("x"), nil); err != nil {
		t.Fatal(err)
	}
	// a second create is a 412, an answer, and timed
	if _, err := cluster.PutIfNoneMatch(context.Background(), "a/_", []byte("x"), nil); err == nil {
		t.Fatal("a second create succeeded")
	}
	if _, _, err := cluster.List(context.Background(), "a/", "", 10); err != nil {
		t.Fatal(err)
	}
	timed.observe(StoreOperationGet, clock.Now(), &sidecar.BlobUnknownError{})
	latencies := timed.take()
	if len(latencies[StoreOperationPut]) != 2 || len(latencies[StoreOperationList]) != 1 || len(latencies[StoreOperationGet]) != 0 {
		t.Fatalf("timed %d puts, %d lists, %d gets", len(latencies[StoreOperationPut]), len(latencies[StoreOperationList]), len(latencies[StoreOperationGet]))
	}
	if again := timed.take(); len(again[StoreOperationPut]) != 0 {
		t.Fatal("take kept what it handed back")
	}
}

func TestLatencySampleKeepsABoundedUniformSample(t *testing.T) {
	sample := newLatencySample(100, 1)
	var wait sync.WaitGroup
	for worker := range 4 {
		wait.Go(func() {
			for i := range 1000 {
				sample.add(time.Duration(worker*1000+i) * time.Microsecond)
			}
		})
	}
	wait.Wait()
	kept := sample.take()
	if len(kept) != 100 {
		t.Fatalf("kept %d", len(kept))
	}
}
