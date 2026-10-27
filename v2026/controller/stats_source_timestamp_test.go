package controller

import (
	"fmt"
	"strconv"
	"testing"
	"time"
)

// Decimal metric seconds are an independent oracle. The old nanosecond
// conversion disagrees at 270 of these 1,000 exactly representable cache
// milliseconds, without any clock, Redis or scheduling dependency.
func TestStatsSourceTimestampSecondsCanonicalCachePrecision(t *testing.T) {
	base := time.Date(2026, time.October, 7, 0, 0, 0, 0, time.UTC)
	legacyDifferences := 0
	for millisecond := range 1000 {
		at := base.Add(time.Duration(millisecond) * time.Millisecond)
		want, err := strconv.ParseFloat(fmt.Sprintf("1791331200.%03d", millisecond), 64)
		if err != nil {
			t.Fatal(err)
		}
		for _, source := range []time.Time{at, time.UnixMilli(at.UnixMilli()), at.Add(999999 * time.Nanosecond)} {
			if got := statsSourceTimestampSeconds(source); got != want {
				t.Fatalf("source millisecond %d changed metric seconds: got %.9f want %.9f", millisecond, got, want)
			}
		}
		if float64(at.UnixNano())/float64(time.Second) != want {
			legacyDifferences++
		}
	}
	if legacyDifferences != 270 {
		t.Fatalf("legacy conversion control differences=%d, want270", legacyDifferences)
	}
}
