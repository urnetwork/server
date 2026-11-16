// Close evidence retention at the contract horizon: its pure rules. No
// database is read.
package server

import (
	"context"
	"testing"
	"time"
)

// Without a horizon the evidence keeps the 13 weeks of migration 857; with
// one, the horizon, never below the guard's 35 days nor above 91.
func TestCloseEvidenceRetentionFollowsTheHorizon(t *testing.T) {
	const day = 24 * time.Hour
	if retention := CloseEvidenceRetention(context.Background()); retention != 91*day {
		t.Fatalf("without a horizon the retention is %s", retention)
	}
	for _, test := range []struct {
		horizon time.Duration
		want    time.Duration
	}{
		{91 * day, 91 * day},
		{45 * day, 45 * day},
		{35 * day, 35 * day},
		{36*day + time.Hour, 36*day + time.Hour},
		{30 * day, 35 * day},
		{0, 35 * day},
		{-day, 35 * day},
		{200 * day, 91 * day},
	} {
		if retention := CloseEvidenceRetention(WithContractHorizon(context.Background(), test.horizon)); retention != test.want {
			t.Errorf("horizon %s keeps evidence %s, want %s", test.horizon, retention, test.want)
		}
	}
	if CloseEvidenceRetentionFloor != 35*day || ContractRetention != 91*day {
		t.Fatalf("floor %s, longest %s", CloseEvidenceRetentionFloor, ContractRetention)
	}
	// a derived context keeps the horizon
	ctx, cancel := context.WithTimeout(WithContractHorizon(context.Background(), 40*day), time.Minute)
	defer cancel()
	if retention := CloseEvidenceRetention(context.WithoutCancel(ctx)); retention != 40*day {
		t.Fatalf("a derived context keeps %s", retention)
	}
}

// A week expires once its upper bound is past the horizon's retention, and
// with no horizon at the 91 days it always had. The registry's rule for the
// legacy and DEFAULT partitions still keeps both.
func TestCloseEvidenceWeekExpiresAtTheHorizon(t *testing.T) {
	const day = 24 * time.Hour
	family := closeEvidencePartitionFamily
	// the week of 2026-10-26, whose upper bound is 43.5 days past
	now := time.Date(2026, 12, 15, 12, 0, 0, 0, time.UTC)
	week := family.RangeAt(now.Add(-50 * day))
	if want := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC); !week.Upper.Equal(want) {
		t.Fatalf("week %+v", week)
	}
	horizon := func(horizon time.Duration) context.Context {
		return WithContractHorizon(context.Background(), horizon)
	}
	if family.IsExpired(context.Background(), now, week) {
		t.Fatal("a week past 43 days expired without a horizon")
	}
	if !family.IsExpired(horizon(35*day), now, week) || !family.IsExpired(horizon(40*day), now, week) {
		t.Fatal("a week past 43 days did not expire at a 35 or 40 day horizon")
	}
	if family.IsExpired(horizon(45*day), now, week) {
		t.Fatal("a week 43 days past expired at a 45 day horizon")
	}
	// exactly on the retention is expired, a moment before is not
	if !family.IsExpired(horizon(35*day), week.Upper.Add(35*day), week) || family.IsExpired(horizon(35*day), week.Upper.Add(35*day-time.Millisecond), week) {
		t.Fatal("the expiry boundary is not the upper bound plus the retention")
	}
	// a horizon below the floor reaches no closer: this week is 29.5 days past
	young := family.RangeAt(now.Add(-36 * day))
	if family.IsExpired(horizon(20*day), now, young) {
		t.Fatal("a week 29 days past expired below the floor")
	}
	for _, r := range []PartitionRange{{Legacy: true, Upper: week.Lower}, {Default: true}} {
		if family.IsExpired(horizon(35*day), now.Add(400*day), r) {
			t.Fatalf("%+v expired", r)
		}
	}
}
