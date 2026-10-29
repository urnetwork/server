package controller

import (
	"math"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The SN Top board's snapshot and agreement metrics (LEADERTOP.md §6.3),
// set by the taskworker that computes the snapshots. Counts and values only,
// no identities. A value whose source has no data yet is NaN.
var (
	snTopSnapshotUnixMilli atomic.Int64

	snTopSnapshotAgeSeconds = prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "sn_top", Name: "snapshot_age_seconds",
		Help: "Seconds since the newest SN Top snapshot this taskworker computed or read was computed; NaN before it saw one. A tempo is about 72 minutes, and the board is stale past three.",
	}, func() float64 {
		computedAt := snTopSnapshotUnixMilli.Load()
		if computedAt == 0 {
			return math.NaN()
		}
		return server.NowUtc().Sub(time.UnixMilli(computedAt)).Seconds()
	})
	snTopSnapshotUnits = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "sn_top", Name: "snapshot_units",
		Help: "What the newest SN Top snapshot counted, by kind: ranked_networks, eligible_providers, measured_providers (the verify cohort), claimed_fleets (UIDs), open_seats and trail_hops (non-seed hops of complete trails in the tempo).",
	}, []string{"kind"})
	snTopProjectedCutoff = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "sn_top", Name: "projected_cutoff",
		Help: "The newest snapshot's projected 200th score (the Top 200 line); 0 while fewer than 200 networks rank.",
	})
	snTopClaimedCutoff = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "sn_top", Name: "claimed_cutoff",
		Help: "The newest snapshot's floor among the claimed fleets the replica selects; NaN without a claimed fleet.",
	})
	snTopReplicaAgreement = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "sn_top", Name: "replica_agreement_uids",
		Help: "UIDs whose replica score equals (match) or differs from (mismatch) the validator's exact head summary score for the folded tempo. Any mismatch means the replica is wrong. Both are 0 without a head summary.",
	}, []string{"result"})
	snTopPrecisionAt200 = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "sn_top", Name: "precision_at_200",
		Help: "Of the claimed networks the previous snapshot projected inside the Top 200, the share the validator selected; NaN until fleets bind and a head summary arrives. The likely label turns on at 0.9.",
	})
	snTopSelectedIncentive = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "sn_top", Name: "selected_incentive_uids",
		Help: "UIDs the validator selected that earn (earning) or do not earn (not_earning) on-chain incentive in the latest read. Both are 0 without a head summary and an incentive read.",
	}, []string{"result"})
	snTopSnapshotDurationSeconds = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "sn_top", Name: "snapshot_duration_seconds",
		Help: "How long the newest SN Top snapshot took to compute and write.",
	})
	snTopSnapshotsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "urnetwork", Subsystem: "sn_top", Name: "snapshots_total",
		Help: "SN Top snapshots this taskworker wrote, one per folded tempo.",
	})
)

func init() {
	prometheus.MustRegister(
		snTopSnapshotAgeSeconds,
		snTopSnapshotUnits,
		snTopProjectedCutoff,
		snTopClaimedCutoff,
		snTopReplicaAgreement,
		snTopPrecisionAt200,
		snTopSelectedIncentive,
		snTopSnapshotDurationSeconds,
		snTopSnapshotsTotal,
	)
	snTopClaimedCutoff.Set(math.NaN())
	snTopPrecisionAt200.Set(math.NaN())
}

func observeSnTopSnapshotTime(computedAt time.Time) {
	for {
		previous := snTopSnapshotUnixMilli.Load()
		if computedAt.UnixMilli() <= previous || snTopSnapshotUnixMilli.CompareAndSwap(previous, computedAt.UnixMilli()) {
			return
		}
	}
}

func snTopNullableGauge(value *float64) float64 {
	if value == nil {
		return math.NaN()
	}
	return *value
}

func observeSnTopSnapshot(snapshot *model.SnTopSnapshot, projection *model.SnTopProjection, inputs *snTopInputs, duration time.Duration) {
	observeSnTopSnapshotTime(snapshot.ComputedAt)
	snTopSnapshotUnits.WithLabelValues("ranked_networks").Set(float64(projection.TotalRanked))
	snTopSnapshotUnits.WithLabelValues("eligible_providers").Set(float64(projection.EligibleProviders))
	snTopSnapshotUnits.WithLabelValues("measured_providers").Set(float64(inputs.measuredProviders))
	snTopSnapshotUnits.WithLabelValues("claimed_fleets").Set(float64(projection.ClaimedFleets))
	snTopSnapshotUnits.WithLabelValues("open_seats").Set(float64(projection.OpenSeats))
	snTopSnapshotUnits.WithLabelValues("trail_hops").Set(float64(inputs.trailHops))
	snTopProjectedCutoff.Set(model.SnTopFinite(projection.ProjectedCutoff))
	snTopClaimedCutoff.Set(snTopNullableGauge(projection.ClaimedCutoff))
	snTopReplicaAgreement.WithLabelValues("match").Set(float64(inputs.agreement.replicaMatches))
	snTopReplicaAgreement.WithLabelValues("mismatch").Set(float64(inputs.agreement.replicaMismatches))
	snTopPrecisionAt200.Set(snTopNullableGauge(inputs.agreement.precisionAt200))
	snTopSelectedIncentive.WithLabelValues("earning").Set(float64(inputs.agreement.selectedEarning))
	snTopSelectedIncentive.WithLabelValues("not_earning").Set(float64(inputs.agreement.selectedNotEarning))
	snTopSnapshotDurationSeconds.Set(duration.Seconds())
	snTopSnapshotsTotal.Inc()
}
