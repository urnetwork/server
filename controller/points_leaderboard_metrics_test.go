package controller

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/server/model"
)

// Block metrics no longer wait on the history rollup (owner decision
// 2026-10-28), so the rebuild's one-minute cadence follows the rollup's own
// progress: an available snapshot keeps it while payments remain, instead of
// dropping the backfill to one batch run an hour.
func TestPointsLeaderboardRebuildDelayFollowsTheBackfill(t *testing.T) {
	for _, c := range []struct {
		name   string
		result *RebuildPointsLeaderboardResult
		want   time.Duration
	}{
		{name: "no result", result: nil, want: pointsLeaderboardRebuildInterval},
		{name: "available, rollup advanced, payments remain",
			result: &RebuildPointsLeaderboardResult{EpochMetricsAvailable: true, RollupBatches: 12, BackfillPendingPayments: 7584},
			want:   time.Minute},
		{name: "unavailable before the first block, rollup advanced, payments remain",
			result: &RebuildPointsLeaderboardResult{RollupBatches: 1, BackfillPendingPayments: 1},
			want:   time.Minute},
		{name: "the run finished the rollup",
			result: &RebuildPointsLeaderboardResult{EpochMetricsAvailable: true, RollupBatches: 3},
			want:   pointsLeaderboardRebuildInterval},
		{name: "nothing the rollup can advance",
			result: &RebuildPointsLeaderboardResult{EpochMetricsAvailable: true, BackfillPendingPayments: 2},
			want:   pointsLeaderboardRebuildInterval},
	} {
		if got := pointsLeaderboardRebuildDelay(c.result); got != c.want {
			t.Fatalf("%s: delay %s, want %s", c.name, got, c.want)
		}
	}
}

// The gauges record how approximate the latest snapshot is, which the snapshot
// row does not store.
func TestPointsLeaderboardApproximationGauges(t *testing.T) {
	observePointsLeaderboardApproximation(model.PointsLeaderboardApproximation{
		RollupPendingPayments:   7574,
		ReapedPayments:          235,
		UnlinkedPoints:          2,
		ApproximateNetworks:     7545,
		BackfillPendingPayments: 7584,
	})
	for _, c := range []struct {
		name string
		got  float64
		want float64
	}{
		{"rollup pending payout-week payments", testutil.ToFloat64(pointsLeaderboardPayoutWeekPayments.WithLabelValues("rollup_pending")), 7574},
		{"reaped payout-week payments", testutil.ToFloat64(pointsLeaderboardPayoutWeekPayments.WithLabelValues("sweeps_reaped")), 235},
		{"unlinked points", testutil.ToFloat64(pointsLeaderboardUnlinkedPoints), 2},
		{"approximate networks", testutil.ToFloat64(pointsLeaderboardApproximateNetworks), 7545},
		{"rollup pending payments", testutil.ToFloat64(pointsLeaderboardRollupPendingPayments), 7584},
	} {
		if c.got != c.want {
			t.Fatalf("%s gauge %v, want %v", c.name, c.got, c.want)
		}
	}
	// a snapshot with no completed block clears them
	observePointsLeaderboardApproximation(model.PointsLeaderboardApproximation{})
	if testutil.ToFloat64(pointsLeaderboardPayoutWeekPayments.WithLabelValues("rollup_pending")) != 0 ||
		testutil.ToFloat64(pointsLeaderboardApproximateNetworks) != 0 {
		t.Fatal("an exact snapshot kept the previous approximation")
	}
}
