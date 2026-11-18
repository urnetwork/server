package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/sidecar/sidecartest"
)

func testPurgeLoadSettings() *PurgeLoadSettings {
	settings := DefaultSegmentPurgeLoadSettings()
	settings.Rates = []*PurgeLoadRate{
		{Name: "chunk", ObjectByteCount: 2048, PerSecond: 4},
		{Name: "index-shard", ObjectByteCount: 512, PerSecond: 6},
	}
	settings.Allowance = 2 * time.Second
	return settings
}

// The purge load puts each of its rates' objects and deletes each its allowance after its put, unconditionally, with
// its HEADs before; what is not yet due when the window ends is left, and nothing more runs after it.
func TestPurgeLoadDeletesEachObjectItsAllowanceAfterItsPut(t *testing.T) {
	clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
	blobs := sidecartest.NewFakeBlobs(sidecartest.NewFakeClock(sidecartest.DefaultClockStart()), nil, nil, nil)
	segment, _ := blobs.Cluster("seg-1")
	settings := testPurgeLoadSettings()
	prefix := "filecluster-bench/0123456789abcdef/"
	done := make(chan struct{})
	var counts *PurgeLoadCounts
	go func() {
		defer close(done)
		counts = runPurgeLoad(context.Background(), queuedClock{FakeClock: clock}, segment, true, settings, 10*time.Second, func(n int64) string {
			return fmt.Sprintf("%su%012d", prefix, n)
		}, nil, &benchNotes{})
	}()
	advanceUntil(t, clock, 50*time.Millisecond, done)
	if counts.Puts != 100 || counts.Failed != 0 || counts.OfferedPutsPerSecond != 10 {
		t.Fatalf("counts %+v", counts)
	}
	// every object put before the window's last allowance was due inside it, each DELETE with its two HEADs; those
	// due after its end are left
	if counts.Deletes < 70 || counts.Puts < counts.Deletes || counts.Heads != 2*counts.Deletes {
		t.Fatalf("counts %+v", counts)
	}
	left, _, err := segment.List(context.Background(), prefix, "", 1000)
	if err != nil || int64(len(left)) != counts.Puts-counts.Deletes {
		t.Fatalf("%d objects left after %d puts and %d deletes: %v", len(left), counts.Puts, counts.Deletes, err)
	}
	// the window ended: a later clock deletes nothing more
	clock.Advance(time.Minute)
	if after, _, _ := segment.List(context.Background(), prefix, "", 1000); len(after) != len(left) {
		t.Fatalf("%d objects after the window, %d at its end", len(after), len(left))
	}
	// its rates' shares: 4 chunks of 2 KiB and 6 shards of 512 B a second, by each arrival's place in its second
	if counts.PutByteCount != 10*(4*2048+6*512) {
		t.Errorf("%d B put", counts.PutByteCount)
	}
}

// Year two's rates: 1 TiB a day of 4 MiB chunks is about 3 a second, beside the index's 45 shards.
func TestPurgeLoadDefaultsAreYearTwos(t *testing.T) {
	settings := DefaultSegmentPurgeLoadSettings()
	if chunks := settings.Rates[0].PerSecond; chunks < 3 || 3.1 < chunks {
		t.Errorf("chunks a second %v", chunks)
	}
	if total := settings.perSecond(); total < 48 || 48.1 < total {
		t.Errorf("total %v", total)
	}
	if slots := DefaultSlotPurgeLoadSettings().perSecond(); slots != 72 {
		t.Errorf("slots %v", slots)
	}
}

// M10's hdd row: year one's and year two's mix with admission on and the purges beside each, against M3's capacity,
// on the run's prefix, emptied after; the verdict passes only with the purges kept and year one admitting all.
func TestHddM10RunsTheMixWithPurgesBesideIt(t *testing.T) {
	clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
	blobs := sidecartest.NewFakeBlobs(sidecartest.NewFakeClock(sidecartest.DefaultClockStart()), nil, nil, nil)
	segment, err := blobs.Cluster("seg-1")
	if err != nil {
		t.Fatal(err)
	}
	counters := &saturatingHddCounters{clock: clock, ceiling: 100000}
	cluster := &hddCountingCluster{BlobCluster: segment, counters: counters}
	target := &BenchTarget{ClusterId: "seg-1", Cluster: cluster, Store: blobs, Counters: counters, Clock: queuedClock{FakeClock: clock}}
	settings := DefaultHddSettings()
	settings.ObjectByteCount = 1024
	settings.MixDuration = 4 * time.Second
	settings.YearOnePutsPerSecond, settings.YearOneGetsPerSecond = 4, 16
	settings.YearTwoPutsPerSecond, settings.YearTwoGetsPerSecond = 8, 32
	settings.AdmissionSettle = time.Second
	settings.Purges = testPurgeLoadSettings()
	admission := &fakeHddAdmission{}
	measurement := &HddMeasurement{RowHeader: RowHeader{CommitSet: fixtureCommitSet(), Settings: map[string]string{}}, Bucket: "sidecar-m10-scratch"}
	done := make(chan struct{})
	var runErr error
	go func() {
		defer close(done)
		runErr = RunHddM10(context.Background(), target, settings, "filecluster-bench/0123456789abcdef/", 1000000,
			func(ctx context.Context, capacityOps int64) (hddAdmitter, error) { return admission, nil }, measurement)
	}()
	advanceUntil(t, clock, 50*time.Millisecond, done)
	if runErr != nil {
		t.Fatal(runErr, measurement.Notes)
	}
	if !measurement.M10 || measurement.AdmissionCapacityOps != 1000000 || len(measurement.CapacitySteps) != 0 {
		t.Fatalf("measurement %+v", measurement)
	}
	for _, window := range []*HddWindow{measurement.YearOne, measurement.YearTwo} {
		if window == nil || window.Purges == nil || window.Purges.Puts != 40 || window.Purges.Failed != 0 || window.PutsStored == 0 {
			t.Fatalf("window %+v, purges %+v", window, window.Purges)
		}
	}
	verdict := JudgeHddM10(measurement, settings)
	if !verdict.YearOneAdmitsAll || !verdict.PurgesKept || verdict.AdmittedPutsPerSecond <= 0 || verdict.Pass {
		t.Fatalf("a smoke's verdict %+v", verdict)
	}
	if left, _, err := segment.List(context.Background(), "filecluster-bench/", "", 10); err != nil || len(left) != 0 {
		t.Fatalf("%d objects left, %v", len(left), err)
	}
	var text strings.Builder
	if err := WriteHddM10Verdict(&text, measurement, verdict); err != nil || !strings.Contains(text.String(), "purges 40 PUTs") {
		t.Fatalf("the verdict's text %q, %v", text.String(), err)
	}
	// a full run passes, and fails each shortfall
	measurement.Smoke = false
	if verdict := JudgeHddM10(measurement, settings); !verdict.Pass {
		t.Fatalf("a full run's verdict %+v", verdict)
	}
	measurement.YearTwo.Purges.Puts = 1
	if verdict := JudgeHddM10(measurement, settings); verdict.Pass || verdict.PurgesKept {
		t.Fatalf("purges short of their rate: %+v", verdict)
	}
	measurement.YearTwo.Purges.Puts = 40
	measurement.YearOne.PutsRefused = 1
	if verdict := JudgeHddM10(measurement, settings); verdict.Pass || verdict.YearOneAdmitsAll {
		t.Fatalf("year one refusing: %+v", verdict)
	}
}

// M10's hdd run is refused without M3's capacity, and on the cluster's own bucket or none.
func TestHddM10RefusesWithoutCapacityOrABucketOfItsOwn(t *testing.T) {
	var start *startError
	if _, err := runHddMode(context.Background(), []string{"-m10", "-cluster", "seg-1", "-smoke"}, &strings.Builder{}); !strings.Contains(fmt.Sprint(err), "-capacity") {
		t.Fatalf("no capacity: %v", err)
	}
	path := filepath.Join(t.TempDir(), "clusters.yml")
	if err := os.WriteFile(path, []byte(`clusters:
  meta-1: {kind: metadata, first: true, endpoints: [127.0.0.1:52101], access_key: a, secret_key: s, bucket: meta, region: us-east-1}
  seg-1: {kind: segment, endpoints: [127.0.0.1:52201], access_key: a, secret_key: s, bucket: seg, region: us-east-1}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, bucket := range []string{"", "seg"} {
		if _, err := openClustersOnBucket(context.Background(), path, "seg-1", bucket); err == nil || !strings.Contains(err.Error(), "bucket of their own") {
			t.Errorf("bucket %q: %v", bucket, err)
		} else if !errors.As(err, &start) {
			t.Errorf("bucket %q: not a refusal to start: %v", bucket, err)
		}
	}
}

// M10's trash row: slot purges beside the overwrites, the file marked M10, and its pair judged apart from M3's.
func TestTrashM10RunsSlotPurgesBesideTheOverwrites(t *testing.T) {
	clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
	target, counting, _ := newOpsTestTarget(clock, -1)
	trash := &fakeTrashReader{cluster: counting}
	settings := DefaultTrashSettings()
	settings.OverwritesPerSecond = 20
	settings.ObjectCount = 100
	settings.Duration = 5 * time.Second
	settings.SampleInterval = time.Second
	settings.Purges = DefaultSlotPurgeLoadSettings()
	settings.Purges.Rates[0].PerSecond = 4
	settings.Purges.Allowance = time.Second
	measurement := &TrashMeasurement{SweepInterval: 30 * time.Second, RowHeader: RowHeader{CommitSet: fixtureCommitSet(), Settings: map[string]string{}}}
	done := make(chan struct{})
	var runErr error
	go func() {
		defer close(done)
		runErr = RunTrash(context.Background(), target, trash, settings, "filecluster-bench/0123456789abcdef/", measurement)
	}()
	advanceUntil(t, clock, 100*time.Millisecond, done)
	if runErr != nil {
		t.Fatal(runErr)
	}
	if !measurement.M10 || measurement.Purges == nil || measurement.Purges.Puts != 20 || measurement.Purges.Deletes == 0 ||
		measurement.Purges.Puts < measurement.Purges.Deletes || measurement.Purges.Heads != measurement.Purges.Deletes || measurement.Purges.Failed != 0 {
		t.Fatalf("purges %+v", measurement.Purges)
	}
	if measurement.Overwrites != 100 || len(measurement.Samples) < 6 {
		t.Fatalf("%d overwrites, %d samples", measurement.Overwrites, len(measurement.Samples))
	}
	// M10's pair alone, M3's alone, never mixed
	slow := *measurement
	slow.SweepInterval = 5 * time.Minute
	if verdict := JudgeTrash([]*TrashMeasurement{&slow, measurement}, DefaultTrashSettings()); !verdict.M10 {
		t.Fatalf("an M10 pair: %+v", verdict)
	}
	m3 := *measurement
	m3.M10, m3.Purges = false, nil
	if verdict := JudgeTrash([]*TrashMeasurement{&m3, measurement}, DefaultTrashSettings()); verdict.Pass || !strings.Contains(strings.Join(verdict.Reasons, ";"), "judged apart") {
		t.Fatalf("a mixed pair: %+v", verdict)
	}
	failed := *measurement
	failed.Purges = &PurgeLoadCounts{Puts: 20, Failed: 1}
	if verdict := JudgeTrash([]*TrashMeasurement{&slow, &failed}, DefaultTrashSettings()); verdict.Pass {
		t.Fatalf("a pair whose purges failed passed: %+v", verdict)
	}
}
