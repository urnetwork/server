package model

import (
	"testing"
)

// The validator's seed request (best_available, count 8 raised to 20,
// force_minimum, Quality) loads 1,000 members that carry no minimums, and
// current subscriber facts refuse most of them. Every draw is one primary
// round trip inside the request's single validation budget, and a forced
// request has no fallback. Exhausting that pool must cost one round trip per
// full batch, not one per handful of survivors still needed.
func TestProviderQualityValidationDrawForcedSparsePool(t *testing.T) {
	const count, loaded, admittedPercent = 20, 1000, 1
	roundTrips, read, admitted := 0, 0, 0
	for read < loaded && admitted < count {
		read += min(providerQualityValidationDraw(count-admitted, true), loaded-read)
		admitted = read * admittedPercent / 100
		roundTrips++
	}
	want := (loaded + providerQualityValidationBatchSize - 1) / providerQualityValidationBatchSize
	if roundTrips != want {
		t.Fatalf("forced sparse pool took %d validation round trips, want %d full batches", roundTrips, want)
	}
}

// Native members passed the Quality gate when the pool was exported, so a
// weighted native draw is nearly always admitted and reads only what the
// request still needs. Neither kind of draw exceeds one bounded batch.
func TestProviderQualityValidationDrawNativeReadsOnlyNeeded(t *testing.T) {
	for _, needed := range []int{1, 3, 20, providerQualityValidationBatchSize} {
		if got := providerQualityValidationDraw(needed, false); got != needed {
			t.Fatalf("native draw for %d needed read %d", needed, got)
		}
	}
	for _, forceMinimum := range []bool{false, true} {
		if got := providerQualityValidationDraw(5000, forceMinimum); got != providerQualityValidationBatchSize {
			t.Fatalf("force_minimum=%t draw read %d, want one bounded batch", forceMinimum, got)
		}
	}
}
