package controller

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/server/v2026/model"
)

// How approximate the latest points leaderboard snapshot is: the inputs of its
// Blocks and Streak that earn in payout weeks instead of stored block rows
// (model.PointsLeaderboardApproximation, owner decision 2026-10-28). The
// rebuild task sets them after each snapshot it publishes; counts only, no
// identities.
var (
	pointsLeaderboardPayoutWeekPayments = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "points_leaderboard", Name: "payout_week_payments",
		Help: "Payments whose points earn in their payout weeks in the latest snapshot, by reason: rollup_pending, the history rollup has not measured their paid sweeps yet and its rows replace the weeks as it does; sweeps_reaped, completed payments the rollup measured after contract reaping had deleted their sweeps, whose weeks stay.",
	}, []string{"reason"})
	pointsLeaderboardUnlinkedPoints = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "points_leaderboard", Name: "unlinked_points",
		Help: "Positive post-genesis point rows without a payment row in the latest snapshot; each earns in the last block that closed before it posted.",
	})
	pointsLeaderboardApproximateNetworks = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "points_leaderboard", Name: "approximate_networks",
		Help: "Networks with a positive point among the latest snapshot's payout-week or unlinked inputs: their Blocks and Streak are approximate, an upper bound of the measured values.",
	})
	pointsLeaderboardRollupPendingPayments = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "points_leaderboard", Name: "rollup_pending_payments",
		Help: "Payments with a positive post-genesis point that the history rollup has not finished, at the latest rebuild: the backfill's remaining work.",
	})
)

func init() {
	prometheus.MustRegister(
		pointsLeaderboardPayoutWeekPayments,
		pointsLeaderboardUnlinkedPoints,
		pointsLeaderboardApproximateNetworks,
		pointsLeaderboardRollupPendingPayments,
	)
}

func observePointsLeaderboardApproximation(approximation model.PointsLeaderboardApproximation) {
	pointsLeaderboardPayoutWeekPayments.WithLabelValues("rollup_pending").Set(float64(approximation.RollupPendingPayments))
	pointsLeaderboardPayoutWeekPayments.WithLabelValues("sweeps_reaped").Set(float64(approximation.ReapedPayments))
	pointsLeaderboardUnlinkedPoints.Set(float64(approximation.UnlinkedPoints))
	pointsLeaderboardApproximateNetworks.Set(float64(approximation.ApproximateNetworks))
	pointsLeaderboardRollupPendingPayments.Set(float64(approximation.BackfillPendingPayments))
}
