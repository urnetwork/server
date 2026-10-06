// The bounded report logs once per interval and counts what it held back.
package controller

import (
	"testing"
	"time"
)

// Reports inside an interval are held back and counted on the next report.
func TestBoundedReportAllowsOneReportPerInterval(t *testing.T) {
	report := newBoundedReport(time.Minute)
	startTime := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)

	steps := []struct {
		offset          time.Duration
		ok              bool
		suppressedCount int
	}{
		{offset: 0, ok: true, suppressedCount: 0},
		{offset: time.Second, ok: false},
		{offset: 59 * time.Second, ok: false},
		{offset: time.Minute, ok: true, suppressedCount: 2},
		{offset: time.Minute + time.Second, ok: false},
		{offset: 3 * time.Minute, ok: true, suppressedCount: 1},
	}
	for _, step := range steps {
		suppressedCount, ok := report.Allow(startTime.Add(step.offset))
		if ok != step.ok || suppressedCount != step.suppressedCount {
			t.Fatalf("at +%s: allow = (%d, %t), want (%d, %t)", step.offset, suppressedCount, ok, step.suppressedCount, step.ok)
		}
	}
}
