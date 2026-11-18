package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/sidecar/sidecartest"
)

func fixtureTrash(sweep time.Duration, bytesPerOverwrite uint64, peakInodes uint64) *TrashMeasurement {
	measurement := &TrashMeasurement{
		RowHeader:     RowHeader{Mode: ModeTrash, CommitSet: fixtureCommitSet(), ClusterId: "meta-1"},
		SweepInterval: sweep,
		Seconds:       3600,
		Overwrites:    700 * 3600,
		Drives:        fixtureDrives(bytesPerOverwrite*700*3600, 3*700*3600, 0),
		RawUsageDelta: 0,
		InodeDelta:    0,
	}
	for i := range 6 {
		inodes := peakInodes / uint64(i+1)
		measurement.Samples = append(measurement.Samples, &TrashSample{Seconds: float64(i * 600), ByteCount: inodes * 4096, InodeCount: inodes, Drives: 8})
	}
	return measurement
}

func TestTrashFiguresMeasureTheSweepFromWhatTrashLost(t *testing.T) {
	measurement := &TrashMeasurement{Seconds: 10, Samples: []*TrashSample{
		{ByteCount: 100}, {ByteCount: 300}, {ByteCount: 150}, {ByteCount: 400}, {ByteCount: 200},
	}}
	figures := trashFigures(measurement)
	if figures.PeakTrashByteCount != 400 || figures.SweepBytesPerSecond != 35 || !figures.TrashRead {
		t.Fatalf("figures %+v", figures)
	}
}

func TestTrashVerdictTakesThirtySecondsOnlyIfItHelps(t *testing.T) {
	// 30 s holds fewer inodes in .trash at about the same drive bytes an overwrite: it helps
	verdict := JudgeTrash([]*TrashMeasurement{fixtureTrash(5*time.Minute, 20000, 1000000), fixtureTrash(30*time.Second, 20500, 100000)}, DefaultTrashSettings())
	if !verdict.Pass || verdict.Interval != 30*time.Second {
		t.Fatalf("verdict %+v", verdict)
	}
	var text strings.Builder
	if err := WriteTrashVerdict(&text, verdict); err != nil || !strings.Contains(text.String(), "MINIO_API_DELETE_CLEANUP_INTERVAL=30s") {
		t.Fatalf("%v:\n%s", err, text.String())
	}
	// it writes more a drive: the 5 min default stays
	verdict = JudgeTrash([]*TrashMeasurement{fixtureTrash(5*time.Minute, 20000, 1000000), fixtureTrash(30*time.Second, 22000, 100000)}, DefaultTrashSettings())
	if !verdict.Pass || verdict.Interval != 5*time.Minute {
		t.Fatalf("verdict %+v", verdict)
	}
	// one run alone decides nothing
	verdict = JudgeTrash([]*TrashMeasurement{fixtureTrash(5*time.Minute, 20000, 1000000)}, DefaultTrashSettings())
	if verdict.Pass {
		t.Fatal("one run passed")
	}
	smoke := fixtureTrash(30*time.Second, 20000, 1000)
	smoke.Smoke = true
	verdict = JudgeTrash([]*TrashMeasurement{fixtureTrash(5*time.Minute, 20000, 1000000), smoke}, DefaultTrashSettings())
	if verdict.Pass {
		t.Fatal("a smoke passed")
	}
}

// .trash growing with the overwrites until each sweep empties it.
type fakeTrashReader struct {
	cluster *countingCluster

	stateLock sync.Mutex
	reads     int
}

func (self *fakeTrashReader) ReadTrash(ctx context.Context, clusterId string) (*TrashSample, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.reads += 1
	self.cluster.stateLock.Lock()
	overwrites := uint64(self.cluster.overwrites)
	self.cluster.stateLock.Unlock()
	inodes := overwrites % 50
	return &TrashSample{ByteCount: inodes * 4096, InodeCount: inodes, MaxDriveByteCount: inodes * 4096, MaxDriveInodeCount: inodes, Drives: 8}, nil
}

func TestTrashRunOverwritesAtItsPaceAndSamplesTrash(t *testing.T) {
	clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
	target, counting, _ := newOpsTestTarget(clock, -1)
	trash := &fakeTrashReader{cluster: counting}
	settings := DefaultTrashSettings()
	// an object a window, as a run's 100,000 at 700 a second leave minutes between two overwrites of one
	settings.OverwritesPerSecond = 20
	settings.ObjectCount = 100
	settings.Duration = 5 * time.Second
	settings.SampleInterval = time.Second
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
	if measurement.Overwrites != 100 || measurement.Failed != 0 || counting.overwrites != 100 || counting.creates != 100 {
		t.Fatalf("%d overwrites, %d failed; the store saw %d and %d creates", measurement.Overwrites, measurement.Failed, counting.overwrites, counting.creates)
	}
	if len(measurement.Samples) < 3 {
		t.Fatalf("%d samples", len(measurement.Samples))
	}
	figures := trashFigures(measurement)
	if !figures.DrivesRead || figures.BytesPerOverwrite != 10752 {
		t.Fatalf("figures %+v", figures)
	}
	if !measurement.Smoke {
		t.Fatal("a 5 s window was not a smoke")
	}
}

func TestHostStatsCountsEachDrivesTrash(t *testing.T) {
	root := writeHostStatsRoot(t)
	trash := filepath.Join(root, "srv/file-cluster/meta-1/d1/.minio.sys/tmp/.trash")
	for i := range 3 {
		dir := filepath.Join(trash, fmt.Sprintf("entry-%d", i))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "xl.meta"), bytes.Repeat([]byte("x"), 100*(i+1)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	source := &hostStatsSource{root: root, runSmart: func(ctx context.Context, device string) ([]byte, error) {
		return []byte(`{"data_units_written": 1, "percent_used": 0, "avail_spare": 100, "media_errors": 0}`), nil
	}}
	answer := source.read(context.Background(), []string{"/srv/file-cluster/meta-1/d1", "/srv/file-cluster/meta-1/d 2"})
	first := answer.Drives[0]
	// three entry directories and their three files, 100 + 200 + 300 bytes
	if *first.TrashByteCount != 600 || *first.TrashInodeCount != 6 {
		t.Fatalf("trash %d bytes, %d inodes", *first.TrashByteCount, *first.TrashInodeCount)
	}
	// a drive with no .trash holds nothing there
	if second := answer.Drives[1]; *second.TrashByteCount != 0 || *second.TrashInodeCount != 0 {
		t.Fatalf("second drive's trash %d, %d", *second.TrashByteCount, *second.TrashInodeCount)
	}
}

func TestVerdictModeExitsByEachRowsVerdict(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, measurement any) string {
		data, err := json.Marshal(measurement)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	hdd := write("hdd.json", fixtureHdd())
	slow := write("trash-5m.json", fixtureTrash(5*time.Minute, 20000, 1000000))
	fast := write("trash-30s.json", fixtureTrash(30*time.Second, 20500, 100000))
	stdout := &bytes.Buffer{}
	if code := run(context.Background(), []string{"verdict", "-in", hdd, "-in", slow, "-in", fast}, stdout, &bytes.Buffer{}); code != 0 {
		t.Fatalf("exit %d:\n%s", code, stdout.String())
	}
	if !strings.Contains(stdout.String(), "hdd_capacity_ops: 6500") || !strings.Contains(stdout.String(), "MINIO_API_DELETE_CLEANUP_INTERVAL=30s") {
		t.Fatalf("verdicts:\n%s", stdout.String())
	}
	failing := fixtureHdd()
	failing.YearOne.PutsRefused = 3
	if code := run(context.Background(), []string{"verdict", "-in", write("hdd-failing.json", failing)}, &bytes.Buffer{}, &bytes.Buffer{}); code != 1 {
		t.Fatalf("a failing hdd run exited %d", code)
	}
	// a field the shape lacks refuses the file
	if err := os.WriteFile(filepath.Join(dir, "odd.json"), []byte(`{"mode": "hdd", "odd": 1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := run(context.Background(), []string{"verdict", "-in", filepath.Join(dir, "odd.json")}, &bytes.Buffer{}, &bytes.Buffer{}); code != 2 {
		t.Fatalf("an odd file exited %d", code)
	}
}

// An overwrite whose answer is lost has landed, so the object's ETag is read again: the next overwrite of that
// object succeeds instead of failing on the old ETag for the rest of the window.
func TestOverwriteRereadsTheEtagAfterALostAnswer(t *testing.T) {
	blobs := sidecartest.NewFakeBlobs(sidecartest.NewFakeClock(sidecartest.DefaultClockStart()), nil, nil, nil)
	cluster, _ := blobs.Cluster("meta-1")
	ctx := context.Background()
	key := "filecluster-bench/0123456789abcdef/s000004096/o000000000002"
	etag, err := cluster.PutIfNoneMatch(ctx, key, []byte("v0"), nil)
	if err != nil {
		t.Fatal(err)
	}
	objects := &opsObjects{etags: map[int64]string{2: etag}}
	if _, err := blobs.Faults().Add(&sidecartest.FaultPlan{Op: sidecartest.FaultOpPut, KeyPrefix: key, Result: sidecartest.ApplyThenDrop}); err != nil {
		t.Fatal(err)
	}
	if err := overwriteObject(ctx, cluster, objects, key, 2, etag, []byte("v1")); err == nil {
		t.Fatal("a lost answer read as an overwrite")
	}
	landed, _ := objects.get(2)
	if landed == etag {
		t.Fatal("the ETag was not read again after the lost answer")
	}
	next, _ := objects.get(2)
	if err := overwriteObject(ctx, cluster, objects, key, 2, next, []byte("v2")); err != nil {
		t.Fatalf("the next overwrite: %v", err)
	}
	object, err := cluster.Get(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(object.Body)
	object.Body.Close()
	if string(body) != "v2" {
		t.Fatalf("the object reads %q", body)
	}
}
