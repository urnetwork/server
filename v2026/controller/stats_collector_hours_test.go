package controller

import (
	"testing"
	"time"
)

// The closed hour walk that feeds the per-extender counters: each hour once,
// only once it has settled, and never further back than the catch-up bound.

func TestStatsClosedHourWaitsForTheSettle(t *testing.T) {
	hour := time.Date(2026, 9, 18, 14, 0, 0, 0, time.UTC)

	// just after the turn, the hour that ended has not settled
	if closed := statsClosedHour(hour.Add(10 * time.Second)); !closed.Equal(hour.Add(-time.Hour)) {
		t.Fatalf("closed hour just after the turn = %s", closed)
	}
	// once settled, it has
	if closed := statsClosedHour(hour.Add(statsHourBucketSettle)); !closed.Equal(hour) {
		t.Fatalf("closed hour after the settle = %s", closed)
	}
	// and a non-utc clock reads the same
	local := hour.Add(statsHourBucketSettle).In(time.FixedZone("x", 3*3600))
	if closed := statsClosedHour(local); !closed.Equal(hour) {
		t.Fatalf("closed hour in another zone = %s", closed)
	}
}

func TestStatsClosedHoursSinceListsEachHourOnce(t *testing.T) {
	hour := time.Date(2026, 9, 18, 14, 0, 0, 0, time.UTC)
	now := hour.Add(5 * time.Minute)

	// nothing new since the closed hour itself
	if hours := statsClosedHoursSince(hour, now); len(hours) != 0 {
		t.Fatalf("hours since the closed hour = %v", hours)
	}
	// three hours behind: the three that closed, oldest first
	hours := statsClosedHoursSince(hour.Add(-3*time.Hour), now)
	if len(hours) != 3 {
		t.Fatalf("hours = %v, expected three", hours)
	}
	for i, expected := range []time.Time{hour.Add(-2 * time.Hour), hour.Add(-time.Hour), hour} {
		if !hours[i].Equal(expected) {
			t.Fatalf("hours[%d] = %s, expected %s", i, hours[i], expected)
		}
	}
	// a week behind is bounded to the catch-up
	hours = statsClosedHoursSince(hour.Add(-7*24*time.Hour), now)
	if len(hours) != int(statsHourBucketMaxCatchup/time.Hour) {
		t.Fatalf("a week behind walked %d hours, expected %d", len(hours), statsHourBucketMaxCatchup/time.Hour)
	}
	if !hours[len(hours)-1].Equal(hour) {
		t.Fatalf("the bounded walk ends at %s, expected %s", hours[len(hours)-1], hour)
	}
	// the cursor pattern: seed at the closed hour, then walk what closes
	cursor := statsClosedHour(now)
	if hours := statsClosedHoursSince(cursor, now.Add(2*time.Hour)); len(hours) != 2 {
		t.Fatalf("after seeding, two hours later walked %v", hours)
	}
}
