package controller

import (
	"testing"
	"time"

	"github.com/go-playground/assert/v2"
)

// TestFreeGrantWindow pins the daily free grant: it covers the day and stays valid
// for one hour past the end of it, so consecutive daily grants overlap and a client
// never sees a gap at midnight.
func TestFreeGrantWindow(t *testing.T) {
	now := time.Date(2026, 7, 11, 15, 42, 13, 0, time.UTC)

	startTime, endTime := FreeGrantWindow(now)

	// starts at the top of the day
	assert.Equal(t, startTime, time.Date(2026, 7, 11, 0, 0, 0, 0, time.UTC))
	// ends 1 hour after the end of the day
	assert.Equal(t, endTime, time.Date(2026, 7, 12, 1, 0, 0, 0, time.UTC))

	// the window covers the whole day it was granted for
	assert.Equal(t, startTime.Before(now), true)
	assert.Equal(t, now.Before(endTime), true)

	// month boundary rolls over correctly
	startTime, endTime = FreeGrantWindow(time.Date(2026, 7, 31, 23, 59, 0, 0, time.UTC))
	assert.Equal(t, startTime, time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC))
	assert.Equal(t, endTime, time.Date(2026, 8, 1, 1, 0, 0, 0, time.UTC))
}

// TestProGrantWindow pins the monthly Pro grant: the full allowance lands at the
// start of the month and stays valid one day past the end of the month. That extra
// day is also the grace in which a lapsed subscriber is still Pro, because the Pro
// entitlement is "has an in-window pro balance".
func TestProGrantWindow(t *testing.T) {
	now := time.Date(2026, 7, 11, 15, 42, 13, 0, time.UTC)

	startTime, endTime := ProGrantWindow(now)

	// starts at the top of the month
	assert.Equal(t, startTime, time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC))
	// ends 1 day after the end of the month (Aug 1 + 1 day)
	assert.Equal(t, endTime, time.Date(2026, 8, 2, 0, 0, 0, 0, time.UTC))

	assert.Equal(t, startTime.Before(now), true)
	assert.Equal(t, now.Before(endTime), true)

	// december rolls into the next year
	startTime, endTime = ProGrantWindow(time.Date(2026, 12, 25, 0, 0, 0, 0, time.UTC))
	assert.Equal(t, startTime, time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC))
	assert.Equal(t, endTime, time.Date(2027, 1, 2, 0, 0, 0, 0, time.UTC))

	// february (28 days) -- AddDate handles the short month
	startTime, endTime = ProGrantWindow(time.Date(2026, 2, 14, 0, 0, 0, 0, time.UTC))
	assert.Equal(t, startTime, time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC))
	assert.Equal(t, endTime, time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC))
}

// TestGrantWindowsOverlap is the property that matters operationally: each grant is
// still valid when the next one lands, so there is never a moment with no balance.
func TestGrantWindowsOverlap(t *testing.T) {
	day := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	_, endToday := FreeGrantWindow(day)
	startTomorrow, _ := FreeGrantWindow(day.AddDate(0, 0, 1))
	// tomorrow's grant starts before today's expires
	assert.Equal(t, startTomorrow.Before(endToday), true)

	month := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	_, endThisMonth := ProGrantWindow(month)
	startNextMonth, _ := ProGrantWindow(month.AddDate(0, 1, 0))
	assert.Equal(t, startNextMonth.Before(endThisMonth), true)
}
