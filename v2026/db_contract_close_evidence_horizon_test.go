// Close evidence retention at its own window: its pure rules. No database is
// read.
package server

import (
	"context"
	"testing"
	"time"
)

// Without a window the evidence keeps the 13 weeks of migration 857; with
// one, the window, never below the guard's 7 days nor above 91. The window
// the tasks carry is the owner's 7 days (2026-11-23).
func TestCloseEvidenceRetentionFollowsItsWindow(t *testing.T) {
	const day = 24 * time.Hour
	if retention := CloseEvidenceRetention(context.Background()); retention != 91*day {
		t.Fatalf("without a window the retention is %s", retention)
	}
	for _, test := range []struct {
		window time.Duration
		want   time.Duration
	}{
		{91 * day, 91 * day},
		{45 * day, 45 * day},
		{35 * day, 35 * day},
		{14 * day, 14 * day},
		{8*day + time.Hour, 8*day + time.Hour},
		{7 * day, 7 * day},
		{6 * day, 7 * day},
		{0, 7 * day},
		{-day, 7 * day},
		{200 * day, 91 * day},
	} {
		if retention := CloseEvidenceRetention(WithCloseEvidenceWindow(context.Background(), test.window)); retention != test.want {
			t.Errorf("window %s keeps evidence %s, want %s", test.window, retention, test.want)
		}
	}
	if CloseEvidenceRetentionFloor != 7*day || CloseEvidenceRetentionWindow != 7*day || ContractRetention != 91*day {
		t.Fatalf("floor %s, window %s, longest %s", CloseEvidenceRetentionFloor, CloseEvidenceRetentionWindow, ContractRetention)
	}
	// a derived context keeps the window
	ctx, cancel := context.WithTimeout(WithCloseEvidenceWindow(context.Background(), 40*day), time.Minute)
	defer cancel()
	if retention := CloseEvidenceRetention(context.WithoutCancel(ctx)); retention != 40*day {
		t.Fatalf("a derived context keeps %s", retention)
	}
}

// The contract horizon no longer moves the evidence (2026-11-23): a context
// carrying only the reaper's horizon keeps the 91 days, whatever the horizon,
// and one carrying both keeps the window. Before, the evidence followed the
// horizon, 18 days counting as its 35-day floor.
func TestCloseEvidenceRetentionIgnoresTheContractHorizon(t *testing.T) {
	const day = 24 * time.Hour
	for _, horizon := range []time.Duration{14 * day, 18 * day, 35 * day, 45 * day, 91 * day} {
		ctx := WithContractHorizon(context.Background(), horizon)
		if retention := CloseEvidenceRetention(ctx); retention != ContractRetention {
			t.Errorf("a contract horizon of %s keeps evidence %s", horizon, retention)
		}
		if retention := CloseEvidenceRetention(WithCloseEvidenceWindow(ctx, CloseEvidenceRetentionWindow)); retention != 7*day {
			t.Errorf("beside a contract horizon of %s the window keeps evidence %s", horizon, retention)
		}
		// the contract days still follow the horizon
		if retention := ContractDayRetention(WithCloseEvidenceWindow(ctx, CloseEvidenceRetentionWindow)); retention != horizon {
			t.Errorf("the evidence window moved the contract days' retention to %s, horizon %s", retention, horizon)
		}
	}
}

// A week expires once its upper bound is past the window's retention, and
// with no window at the 91 days it always had. The registry's rule for the
// legacy and DEFAULT partitions still keeps both.
func TestCloseEvidenceWeekExpiresAtTheWindow(t *testing.T) {
	const day = 24 * time.Hour
	family := closeEvidencePartitionFamily
	// the week of 2026-11-30, whose upper bound is 8.5 days past
	now := time.Date(2026, 12, 15, 12, 0, 0, 0, time.UTC)
	week := family.RangeAt(now.Add(-15 * day))
	if want := time.Date(2026, 12, 7, 0, 0, 0, 0, time.UTC); !week.Upper.Equal(want) {
		t.Fatalf("week %+v", week)
	}
	window := func(window time.Duration) context.Context {
		return WithCloseEvidenceWindow(context.Background(), window)
	}
	if family.IsExpired(context.Background(), now, week) {
		t.Fatal("a week past 8 days expired without a window")
	}
	if family.IsExpired(WithContractHorizon(context.Background(), 14*day), now.Add(30*day), week) {
		t.Fatal("a week expired at the contract horizon, without a window")
	}
	if !family.IsExpired(window(7*day), now, week) || !family.IsExpired(window(8*day), now, week) {
		t.Fatal("a week past 8 days did not expire at a 7 or 8 day window")
	}
	if family.IsExpired(window(9*day), now, week) {
		t.Fatal("a week 8 days past expired at a 9 day window")
	}
	// exactly on the retention is expired, a moment before is not
	if !family.IsExpired(window(7*day), week.Upper.Add(7*day), week) || family.IsExpired(window(7*day), week.Upper.Add(7*day-time.Millisecond), week) {
		t.Fatal("the expiry boundary is not the upper bound plus the retention")
	}
	// a window below the floor reaches no closer: this week is 1.5 days past
	young := family.RangeAt(now.Add(-8 * day))
	if family.IsExpired(window(time.Hour), now, young) {
		t.Fatal("a week 1.5 days past expired below the floor")
	}
	for _, r := range []PartitionRange{{Legacy: true, Upper: week.Lower}, {Default: true}} {
		if family.IsExpired(window(7*day), now.Add(400*day), r) {
			t.Fatalf("%+v expired", r)
		}
	}
}
