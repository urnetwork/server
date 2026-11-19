//go:build sidecar_testhooks && !windows

package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/sidecartest"
)

// The index mode runs SEARCH-M10-40's driver through its counting store: the off and indexed legs commit content
// pushes, the indexed leg's carry their writers' index writes, which cost more metadata writes a push than the off
// leg's, both drain, and the companion leg, which the driver does not run yet, fails closed with its reason and ends the
// run there.
func TestIndexModeRunsTheSearchNotesLoad(t *testing.T) {
	newLoad, ok := indexLoads()[indexLoadSearchNotes]
	if !ok {
		t.Fatalf("no load %q in a sidecar_testhooks build", indexLoadSearchNotes)
	}
	load, err := newLoad(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer load.Close()
	blobs := sidecartest.NewFakeBlobs(sidecar.RealClock{}, nil, nil, nil)
	settings := DefaultIndexSettings()
	settings.LegDuration = 2 * time.Second
	settings.Legs = settings.Legs[:3]
	measurement := &IndexMeasurement{RowHeader: RowHeader{CommitSet: fixtureCommitSet(), Settings: map[string]string{}}}
	err = RunIndex(context.Background(), load, blobs, nil, settings, measurement)
	if err == nil || !strings.Contains(err.Error(), "companion") {
		t.Fatalf("the companion leg: %v", err)
	}
	off, indexed := measurement.Leg("off"), measurement.Leg(IndexClassIndexed)
	if off == nil || indexed == nil || off.Failure != "" || indexed.Failure != "" || !off.Drained || !indexed.Drained {
		t.Fatalf("the legs: %+v %+v", off, indexed)
	}
	if off.ContentPushes[IndexContentInRef] == 0 || indexed.ContentPushes[IndexContentInRef] == 0 || indexed.Events[IndexEventIndexedPush] == 0 {
		t.Fatalf("pushes: off %v, indexed %v %v", off.ContentPushes, indexed.ContentPushes, indexed.Events)
	}
	writesPerPush := func(leg *IndexLeg) float64 {
		count := int64(0)
		for _, operation := range leg.Operations {
			if operation.Kind == CountedCreate || operation.Kind == CountedOverwrite {
				count += operation.Count
			}
		}
		return float64(count) / float64(leg.ContentPushes[IndexContentInRef])
	}
	if writesPerPush(indexed) <= writesPerPush(off) {
		t.Errorf("the indexed leg wrote %v a push, the off leg %v", writesPerPush(indexed), writesPerPush(off))
	}
	if companion := measurement.Leg(IndexClassCompanion); companion == nil || !strings.Contains(companion.Failure, "companion") {
		t.Errorf("the companion leg's failure: %+v", companion)
	}
	if !measurement.Smoke {
		t.Error("legs of two seconds read as evidence")
	}
}
