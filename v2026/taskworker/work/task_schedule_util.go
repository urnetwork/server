package work

import (
	"time"
)

// nextWeeklyOffPeak returns the next 10:00 UTC that is at least 6 days away,
// so weekly maintenance sweeps land in a low-traffic window and keep a ~7 day
// cadence regardless of when the previous run finished.
func nextWeeklyOffPeak(now time.Time) time.Time {
	t := now.UTC().Add(6 * 24 * time.Hour)
	anchor := time.Date(t.Year(), t.Month(), t.Day(), 10, 0, 0, 0, time.UTC)
	if anchor.Before(t) {
		anchor = anchor.Add(24 * time.Hour)
	}
	return anchor
}
