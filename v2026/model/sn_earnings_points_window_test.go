package model

import (
	"slices"
	"testing"
	"time"
)

// Pure: a window holds the blocks whose end falls in [start, end). Block b
// ends at genesis + b weeks, so Main's epochs 1 to 5 (from 2026-10-09
// 18:26:28Z, a week each) hold blocks 15 to 19, one each; a block ending on
// a window's start counts there and not in the window before.
func TestOperatorBlocksEndingIn(t *testing.T) {
	epoch1Start := time.Date(2026, 10, 9, 18, 26, 28, 0, time.UTC)
	week := 7 * 24 * time.Hour
	for i := 0; i < 5; i += 1 {
		start := epoch1Start.Add(time.Duration(i) * week)
		if got, want := operatorBlocksEndingIn(start, start.Add(week)), []int64{int64(15 + i)}; !slices.Equal(got, want) {
			t.Fatalf("epoch %d: blocks %v, want %v", i+1, got, want)
		}
	}

	end16 := operatorBlockStart(17)
	if got := operatorBlocksEndingIn(end16, end16.Add(time.Second)); !slices.Equal(got, []int64{16}) {
		t.Fatalf("a window from block 16's end: %v", got)
	}
	if got := operatorBlocksEndingIn(end16.Add(-time.Second), end16); len(got) != 0 {
		t.Fatalf("a window to block 16's end: %v", got)
	}
	if got := operatorBlocksEndingIn(SubnetBlockGenesis.Add(-week), SubnetBlockGenesis.Add(3*week)); !slices.Equal(got, []int64{1, 2}) {
		t.Fatalf("a window over the genesis: %v", got)
	}
	if got := operatorBlocksEndingIn(end16, end16); len(got) != 0 {
		t.Fatalf("an empty window: %v", got)
	}
}
