package main

import (
	"slices"
	"testing"
)

// The evaluator passes a sparse physical-core list, whose order must survive
// parsing so every declared core receives exactly one pinned worker.
func TestParseCPUsPreservesSparseSet(t *testing.T) {
	cpus, err := parseCPUs("0,2,4,6,8,10,12,14,16,18")
	if err != nil {
		t.Fatal(err)
	}
	want := []int{0, 2, 4, 6, 8, 10, 12, 14, 16, 18}
	if !slices.Equal(cpus, want) {
		t.Fatalf("CPUs = %v, want %v", cpus, want)
	}
}

// Empty, ranged, negative, nonnumeric, and duplicate targets must fail before
// the fixture can emit its readiness marker.
func TestParseCPUsRejectsAmbiguousTargets(t *testing.T) {
	for _, value := range []string{"", "0-4", "-1", "cpu0", "0,2,2"} {
		if cpus, err := parseCPUs(value); err == nil {
			t.Errorf("parseCPUs(%q) = %v, want error", value, cpus)
		}
	}
}
