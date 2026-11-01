package model

import (
	"slices"
	"testing"
	"time"
)

// Pure: the blocks an operator block award can still read are the blocks
// with traffic past the cutoff, through the one after the open block, whose
// reliability window was inside client_reliability retention when the oldest
// award still running started, 13 hours before now (OperatorBlockPointsMaxTime
// and an hour), to the minute the award checks. Blocks 15 to 21 open on the
// Sundays from 10-04 to 11-15.
func TestOperatorBlockPointsAwardableBlocks(t *testing.T) {
	at := func(month time.Month, day int, hour int, minute int) time.Time {
		return time.Date(2026, month, day, hour, minute, 0, 0, time.UTC)
	}
	if !operatorBlockStart(15).Equal(at(time.October, 4, 0, 0)) || !operatorBlockStart(21).Equal(at(time.November, 15, 0, 0)) {
		t.Fatal("the block numbering moved", operatorBlockStart(15), operatorBlockStart(21))
	}
	if OperatorBlockPointsMaxTime != 12*time.Hour {
		t.Fatal("an award's claim bound is not 12 hours", OperatorBlockPointsMaxTime)
	}
	for _, sample := range []struct {
		name   string
		cutoff time.Time
		now    time.Time
		want   []int64
	}{
		// an award started 13 hours ago checked retention back to 10-17 21:00,
		// before block 17 opened
		{name: "mid block", cutoff: at(time.October, 6, 12, 0), now: at(time.November, 8, 10, 0), want: []int64{17, 18, 19, 20, 21}},
		// that award's retention reached back to exactly when block 17 opened
		{name: "on the retention boundary", cutoff: at(time.October, 6, 12, 0), now: at(time.November, 8, 13, 0), want: []int64{17, 18, 19, 20, 21}},
		// a minute later block 17's first reliability block was past it
		{name: "a minute past the boundary", cutoff: at(time.October, 6, 12, 0), now: at(time.November, 8, 13, 1), want: []int64{18, 19, 20, 21}},
		// the first block's window starts at the cutoff, inside retention
		{name: "a recent cutoff", cutoff: at(time.October, 30, 12, 0), now: at(time.November, 8, 10, 0), want: []int64{18, 19, 20, 21}},
		{name: "a cutoff in the open block", cutoff: at(time.November, 9, 0, 0), now: at(time.November, 8, 10, 0), want: []int64{20, 21}},
		{name: "a cutoff after the next block opens", cutoff: at(time.November, 16, 0, 0), now: at(time.November, 8, 10, 0), want: []int64{21}},
		{name: "a cutoff before the genesis", cutoff: time.Date(2020, 1, 7, 0, 0, 0, 0, time.UTC), now: at(time.November, 8, 14, 0), want: []int64{18, 19, 20, 21}},
	} {
		if got := operatorBlockPointsAwardableBlocks(sample.cutoff, sample.now); !slices.Equal(got, sample.want) {
			t.Errorf("%s: awardable blocks %v, want %v", sample.name, got, sample.want)
		}
	}
}

// Pure: the reaper's predicate is the award's: a reliability window is
// retained from the block of now - ClientReliabilityRetention on, and the one
// before it has expired.
func TestOperatorBlockReliabilityRetainedIsTheRetention(t *testing.T) {
	now := time.Date(2026, time.November, 8, 10, 30, 15, 0, time.UTC)
	cutoffBlock := clientReliabilityRetentionCutoffBlock(now)
	if cutoffBlock != reliabilityBlockNumber(now.Add(-21*24*time.Hour)) {
		t.Fatal("client_reliability retention is not 21 days")
	}
	if !operatorBlockReliabilityRetained(cutoffBlock, now) || !operatorBlockReliabilityRetained(cutoffBlock+1, now) {
		t.Fatal("a window inside retention counts as expired")
	}
	if operatorBlockReliabilityRetained(cutoffBlock-1, now) {
		t.Fatal("a window past retention counts as retained")
	}
}

// Pure: a transfer rollup writes from the first whole day the contract
// horizon reaper cannot reach, two days inside its 35-day floor.
func TestTransferAuditRollupFirstDay(t *testing.T) {
	for _, sample := range []struct {
		now  time.Time
		want time.Time
	}{
		{now: time.Date(2026, time.November, 8, 10, 0, 0, 0, time.UTC), want: time.Date(2026, time.October, 7, 0, 0, 0, 0, time.UTC)},
		{now: time.Date(2026, time.November, 8, 0, 0, 0, 0, time.UTC), want: time.Date(2026, time.October, 6, 0, 0, 0, 0, time.UTC)},
		{now: time.Date(2026, time.November, 8, 0, 0, 0, 1, time.UTC), want: time.Date(2026, time.October, 7, 0, 0, 0, 0, time.UTC)},
	} {
		if got := TransferAuditRollupFirstDay(sample.now); !got.Equal(sample.want) {
			t.Errorf("at %s: first day %s, want %s", sample.now, got, sample.want)
		}
	}
}
