package controller

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/server/model"
)

// How much of the latest points leaderboard snapshot earns in payout weeks
// instead of measured block rows (model.PointsLeaderboardApproximation). The
// rebuild task sets them after each snapshot; counts only, no identities.
var (
	pointsLeaderboardPayoutWeekPayments = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "points_leaderboard", Name: "payout_week_payments",
		Help: "Payments whose points earned in their payout weeks in the latest snapshot, because the history rollup had not measured their paid sweeps.",
	})
	pointsLeaderboardUnmeasuredPayments = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "points_leaderboard", Name: "unmeasured_payments",
		Help: "Of the payout-week payments, completed payments the rollup measured after contract reaping had deleted their sweeps; the rollup cannot replace their payout weeks.",
	})
	pointsLeaderboardPayoutWeekPoints = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "points_leaderboard", Name: "payout_week_points",
		Help: "Positive point rows of the payout-week payments in the latest snapshot.",
	})
	pointsLeaderboardUnlinkedPoints = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "points_leaderboard", Name: "unlinked_points",
		Help: "Positive post-genesis point rows without a payment row; each earned in the last block that closed before it posted.",
	})
	pointsLeaderboardApproximateNetworks = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "points_leaderboard", Name: "approximate_networks",
		Help: "Networks with any payout-week or unlinked point in the latest snapshot.",
	})
	pointsLeaderboardBackfillPendingPayments = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "points_leaderboard", Name: "backfill_pending_payments",
		Help: "Payments with positive post-genesis points that the history rollup has not finished: the backfill's remaining work.",
	})
)

func init() {
	prometheus.MustRegister(
		pointsLeaderboardPayoutWeekPayments,
		pointsLeaderboardUnmeasuredPayments,
		pointsLeaderboardPayoutWeekPoints,
		pointsLeaderboardUnlinkedPoints,
		pointsLeaderboardApproximateNetworks,
		pointsLeaderboardBackfillPendingPayments,
	)
}

func observePointsLeaderboardApproximation(approximation model.PointsLeaderboardApproximation) {
	pointsLeaderboardPayoutWeekPayments.Set(float64(approximation.PayoutWeekPayments))
	pointsLeaderboardUnmeasuredPayments.Set(float64(approximation.UnmeasuredPayments))
	pointsLeaderboardPayoutWeekPoints.Set(float64(approximation.PayoutWeekPoints))
	pointsLeaderboardUnlinkedPoints.Set(float64(approximation.UnlinkedPoints))
	pointsLeaderboardApproximateNetworks.Set(float64(approximation.ApproximateNetworks))
	pointsLeaderboardBackfillPendingPayments.Set(float64(approximation.BackfillPendingPayments))
}
