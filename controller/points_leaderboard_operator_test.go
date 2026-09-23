package controller

import (
	"testing"
	"time"

	"github.com/urnetwork/server/model"
)

func TestPointsLeaderboardOperatorWindowsUseCompletedSundayBlocks(t *testing.T) {
	genesis := model.SubnetBlockGenesis
	week := model.SubnetBlockDuration
	for _, tc := range []struct {
		name string
		now  time.Time
		want int
	}{
		{name: "before genesis", now: genesis.Add(-time.Second), want: 0},
		{name: "genesis", now: genesis, want: 0},
		{name: "before first close", now: genesis.Add(week - time.Nanosecond), want: 0},
		{name: "first close", now: genesis.Add(week), want: 1},
		{name: "Sunday payout still pending", now: genesis.Add(2*week + 9*time.Hour), want: 2},
		{name: "after payout hour", now: genesis.Add(2*week + 11*time.Hour), want: 2},
		{name: "third close", now: genesis.Add(3 * week), want: 3},
	} {
		windows := pointsLeaderboardOperatorWindows(tc.now)
		if len(windows) != tc.want {
			t.Fatalf("%s: got %d windows, want %d", tc.name, len(windows), tc.want)
		}
		for i, window := range windows {
			wantStart := genesis.Add(time.Duration(i) * week)
			if window.Epoch != uint64(i+1) || !window.Start.Equal(wantStart) || !window.End.Equal(wantStart.Add(week)) {
				t.Fatalf("%s: window %d = %+v", tc.name, i, window)
			}
		}
	}
}
