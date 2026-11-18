package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/sidecartest"
)

// A load that writes, per leg, a known set on the store it is handed: per content push a head overwrite, a record
// create of 3,000 B and a batch DELETE on meta-1, and a segment PUT on seg-1; per class on, one more 1 KiB
// overwrite a push; a 412 and a 404 each leg, which write nothing; and one unknown outcome, which may have.
type fakeIndexLoad struct {
	pushes  int
	failLeg string
	// what each leg saw
	legs    []string
	classes [][]string
	closed  bool
}

func (self *fakeIndexLoad) RunLeg(ctx context.Context, store sidecar.BlobStore, name string, classes []string, duration time.Duration) (map[string]int64, map[string]int64, bool, error) {
	self.legs = append(self.legs, name)
	self.classes = append(self.classes, classes)
	if name == self.failLeg {
		return map[string]int64{}, map[string]int64{}, false, errors.New("the load stopped")
	}
	meta, err := store.Cluster("meta-1")
	if err != nil {
		return nil, nil, false, err
	}
	segment, err := store.SegmentCluster()
	if err != nil {
		return nil, nil, false, err
	}
	prefix := fmt.Sprintf("index-test/%s/", name)
	for n := range self.pushes {
		head := fmt.Sprintf("%sh%04d", prefix, n)
		etag, err := meta.PutIfNoneMatch(ctx, head, []byte("v0"), nil)
		if err != nil {
			return nil, nil, false, err
		}
		if _, err := meta.PutIfMatch(ctx, head, etag, []byte("v1"), nil); err != nil {
			return nil, nil, false, err
		}
		if _, err := meta.PutIfNoneMatch(ctx, fmt.Sprintf("%sr%04d", prefix, n), bytes.Repeat([]byte("r"), 3000), nil); err != nil {
			return nil, nil, false, err
		}
		if err := meta.Delete(ctx, fmt.Sprintf("%sr%04d", prefix, n)); err != nil {
			return nil, nil, false, err
		}
		body := []byte(fmt.Sprintf("segment %s %d", name, n))
		checksum := sha256.Sum256(body)
		if _, err := segment.PutStream(ctx, fmt.Sprintf("%ss%04d", prefix, n), bytes.NewReader(body), int64(len(body)), checksum[:]); err != nil {
			return nil, nil, false, err
		}
		for range classes {
			if _, err := meta.PutIfMatch(ctx, head, mustEtag(ctx, meta, head), bytes.Repeat([]byte("i"), 1024), nil); err != nil {
				return nil, nil, false, err
			}
		}
	}
	// refusals write nothing
	if _, err := meta.PutIfNoneMatch(ctx, fmt.Sprintf("%sh%04d", prefix, 0), []byte("again"), nil); !errors.Is(err, sidecar.ErrBlobPrecondition) {
		return nil, nil, false, fmt.Errorf("a create over a key: %v", err)
	}
	if _, err := meta.PutIfMatch(ctx, prefix+"absent", "etag", []byte("x"), nil); !errors.Is(err, sidecar.ErrBlobPrecondition) {
		return nil, nil, false, fmt.Errorf("an overwrite of nothing: %v", err)
	}
	events := map[string]int64{}
	for _, class := range classes {
		events[IndexClassEvents[class]] = int64(self.pushes)
	}
	return map[string]int64{IndexContentInRef: int64(self.pushes)}, events, true, nil
}

func (self *fakeIndexLoad) Close() { self.closed = true }

func mustEtag(ctx context.Context, cluster sidecar.BlobCluster, key string) string {
	info, err := cluster.Head(ctx, key)
	if err != nil {
		return ""
	}
	return info.Etag
}

func countedOf(leg *IndexLeg, kind string, objectByteCount int64) int64 {
	for _, operation := range leg.Operations {
		if operation.Kind == kind && operation.ObjectByteCount == objectByteCount {
			return operation.Count
		}
	}
	return 0
}

// Each leg's writes go through the counting store: metadata creates, overwrites and deletes by size rounded up to
// 1 KiB, segment PUTs as HDD PUTs, refusals not at all, and the counts start again at each leg, so a class's leg
// counts one more overwrite a push than the leg before it.
func TestIndexRunCountsEachLegsWritesThroughTheCountingStore(t *testing.T) {
	blobs := sidecartest.NewFakeBlobs(sidecartest.NewFakeClock(sidecartest.DefaultClockStart()), nil, nil, nil)
	load := &fakeIndexLoad{pushes: 10}
	settings := DefaultIndexSettings()
	measurement := &IndexMeasurement{RowHeader: RowHeader{CommitSet: fixtureCommitSet(), Settings: map[string]string{}}}
	if err := RunIndex(context.Background(), load, blobs, sidecartest.NewFakeClock(sidecartest.DefaultClockStart()), settings, measurement); err != nil {
		t.Fatal(err)
	}
	if len(measurement.Legs) != 1+len(IndexClasses) || measurement.Mode != ModeIndex {
		t.Fatalf("%d legs, mode %q", len(measurement.Legs), measurement.Mode)
	}
	for i, leg := range measurement.Legs {
		if len(leg.Classes) != i || load.legs[i] != leg.Name {
			t.Fatalf("leg %d is %s with %v", i, leg.Name, leg.Classes)
		}
		if creates := countedOf(leg, CountedCreate, 1024); creates != 10 {
			t.Errorf("%s: %d 1 KiB creates", leg.Name, creates)
		}
		if records := countedOf(leg, CountedCreate, 3072); records != 10 {
			t.Errorf("%s: %d 3 KiB records", leg.Name, records)
		}
		if deletes := countedOf(leg, CountedDelete, 0); deletes != 10 {
			t.Errorf("%s: %d deletes", leg.Name, deletes)
		}
		if overwrites := countedOf(leg, CountedOverwrite, 1024); overwrites != int64(10+10*i) {
			t.Errorf("%s: %d overwrites, want %d", leg.Name, overwrites, 10+10*i)
		}
		if leg.HddPutCount != 10 || leg.HddDeleteCount != 0 || !leg.Drained || leg.Failure != "" {
			t.Errorf("%s: %d HDD PUTs, %d DELETEs, drained %v, %q", leg.Name, leg.HddPutCount, leg.HddDeleteCount, leg.Drained, leg.Failure)
		}
		if leg.contentPushCount() != 10 {
			t.Errorf("%s: %d content pushes", leg.Name, leg.contentPushCount())
		}
	}
	if measurement.Smoke {
		t.Errorf("a full run reads as a smoke: %v", measurement.Notes)
	}
	// the file reads back strictly
	data, err := json.Marshal(measurement)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseIndexMeasurement(data)
	if err != nil || len(parsed.Legs) != len(measurement.Legs) {
		t.Fatalf("parsed %v", err)
	}
	if _, err := ParseIndexMeasurement(bytes.Replace(data, []byte(`"legs"`), []byte(`"lags"`), 1)); err == nil {
		t.Error("a field the shape lacks was read")
	}
	if _, err := ParseIndexMeasurement(bytes.Replace(data, []byte(`"mode": "index"`), []byte(`"mode": "hdd"`), 1)); err == nil && !strings.Contains(string(data), `"mode":"index"`) {
		t.Error("an hdd file was read as an index file")
	}
}

// An unknown outcome may have written, so it is counted; a definite refusal is not.
func TestCountedOutcomesCountWhatMayHaveWritten(t *testing.T) {
	for _, c := range []struct {
		err  error
		want bool
	}{
		{err: nil, want: true},
		{err: &sidecar.BlobUnknownError{Err: errors.New("timeout")}, want: true},
		{err: fmt.Errorf("wrapped: %w", sidecar.ErrBlobPrecondition), want: false},
		{err: sidecar.ErrBlobNotFound, want: false},
		{err: sidecar.ErrBlobHeld, want: false},
	} {
		if got := countedOutcome(c.err); got != c.want {
			t.Errorf("%v: %v", c.err, got)
		}
	}
}

// A leg that fails ends the run, the legs after it unrun, and the file is a smoke; so is a run of short legs.
func TestIndexRunStopsAtAFailedLegAndIsASmoke(t *testing.T) {
	blobs := sidecartest.NewFakeBlobs(sidecartest.NewFakeClock(sidecartest.DefaultClockStart()), nil, nil, nil)
	load := &fakeIndexLoad{pushes: 2, failLeg: IndexClassCompanion}
	measurement := &IndexMeasurement{RowHeader: RowHeader{CommitSet: fixtureCommitSet(), Settings: map[string]string{}}}
	err := RunIndex(context.Background(), load, blobs, nil, DefaultIndexSettings(), measurement)
	if err == nil || !strings.Contains(err.Error(), "companion") {
		t.Fatalf("the run: %v", err)
	}
	if !measurement.Smoke || len(load.legs) != 3 {
		t.Fatalf("smoke %v after legs %v", measurement.Smoke, load.legs)
	}
	for _, leg := range measurement.Legs[3:] {
		if leg.Failure == "" {
			t.Errorf("%s ran after a failed leg", leg.Name)
		}
	}
	settings := DefaultIndexSettings()
	settings.LegDuration = time.Minute
	short := &IndexMeasurement{RowHeader: RowHeader{CommitSet: fixtureCommitSet(), Settings: map[string]string{}}}
	fresh := sidecartest.NewFakeBlobs(sidecartest.NewFakeClock(sidecartest.DefaultClockStart()), nil, nil, nil)
	if err := RunIndex(context.Background(), &fakeIndexLoad{pushes: 1}, fresh, nil, settings, short); err != nil || !short.Smoke {
		t.Errorf("legs of a minute: smoke %v, %v", short.Smoke, err)
	}
}

// With no load driver built the index mode refuses to start, naming what it waits on.
func TestIndexModeRefusesWithoutALoad(t *testing.T) {
	_, err := runIndexMode(context.Background(), []string{"-load", "search", "-smoke"}, &bytes.Buffer{})
	var start *startError
	if !errors.As(err, &start) || !strings.Contains(err.Error(), "SEARCH-M10-40") {
		t.Fatalf("the index mode with no load: %v", err)
	}
	if code := run(context.Background(), []string{"index", "-load", ""}, &bytes.Buffer{}, &bytes.Buffer{}); code != 2 {
		t.Errorf("exit %d", code)
	}
}
