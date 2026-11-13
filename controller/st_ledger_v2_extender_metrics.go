package controller

// st_ledger_v2_extender_metrics.go — the SN25 extender pool's metrics (sn
// WHITEPAPER.md §8.6.7: Z and its parts, M, N and the extenders' share of the
// pool). Bytes, counts and epochs only; no client, coldkey, unit or address
// appears in a label. Each gauge describes the last epoch of its phase:
//   - census: the census captured at an epoch's end (N, M, rows);
//   - build: an epoch the amendment covers, as a root first builds it;
//   - shadow: an epoch before E_x, as its shadow report builds it, and the
//     timed-out attempts of the last shadow given up.

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/urfoundation/sn/ledger"

	"github.com/urnetwork/server/model"
)

var (
	stExtenderPoolEpoch = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "st_extender_pool", Name: "epoch",
		Help: "The epoch the extender pool gauges describe, by phase: census (captured at the epoch's end), build (an epoch the amendment covers, first built) and shadow (a shadow report).",
	}, []string{"phase"})
	stExtenderPoolBytes = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "st_extender_pool", Name: "bytes",
		Help: "The extender pool's bytes in the epoch of its phase (build, shadow), by part: intra_platform and intra_network (Z's parts), pool (Z), weight (Z', after the cap), eligible_provider (W_k, the eligible provider usage that caps Z), and unfunded and measurement (pooled shares Z leaves out).",
	}, []string{"phase", "part"})
	stExtenderPoolCounts = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "st_extender_pool", Name: "count",
		Help: "Counts in the epoch of the phase (census, build, shadow), by count: census_rows, eligible (N), units (M), contracts and payers (behind Z), unfunded_contracts and measurement_contracts (left out of Z), platform_endpoints (registered when the epoch was read), and timed_out_attempts (shadow: the attempts whose read ran past its bound when an epoch's shadow was last given up).",
	}, []string{"phase", "count"})
	stExtenderPoolWeightShare = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "st_extender_pool", Name: "weight_share",
		Help: "The extenders' share of the epoch's pool weight, Z' / (W_k + Z'), at most 0.5 under the cap (zeta 1), by phase (build, shadow).",
	}, []string{"phase"})
)

// Registers the extender pool's metrics.
func init() {
	prometheus.MustRegister(stExtenderPoolEpoch, stExtenderPoolBytes, stExtenderPoolCounts, stExtenderPoolWeightShare)
}

// Records an epoch's captured census.
func stExtenderCensusMetrics(epoch uint64, rows []ledger.CensusRow) {
	eligible, units := stExtenderCensusCounts(rows)
	stExtenderPoolEpoch.WithLabelValues("census").Set(float64(epoch))
	stExtenderPoolCounts.WithLabelValues("census", "census_rows").Set(float64(len(rows)))
	stExtenderPoolCounts.WithLabelValues("census", "eligible").Set(float64(eligible))
	stExtenderPoolCounts.WithLabelValues("census", "units").Set(float64(units))
}

// Records an epoch's extender pool as a phase built it: the epoch reader's
// pool, the split and the eligible provider usage.
func stExtenderPoolMetrics(phase string, epoch uint64, pool *model.StEpochExtenderPool, split *ledger.ExtenderSplit, eligibleProvider uint64, platformEndpoints int) {
	stExtenderPoolEpoch.WithLabelValues(phase).Set(float64(epoch))
	partBytes := map[string]uint64{
		"intra_platform": uint64(pool.IntraPlatform), "intra_network": uint64(pool.IntraNetwork), "pool": pool.Bytes(),
		"weight": split.Weight, "eligible_provider": eligibleProvider,
		"unfunded": uint64(pool.UnfundedBytes), "measurement": uint64(pool.MeasurementBytes),
	}
	for part, bytes := range partBytes {
		stExtenderPoolBytes.WithLabelValues(phase, part).Set(float64(bytes))
	}
	kindCounts := map[string]uint64{
		"eligible": split.Eligible, "units": split.Units, "contracts": pool.Contracts, "payers": pool.Payers,
		"unfunded_contracts": pool.UnfundedContracts, "measurement_contracts": pool.MeasurementContracts,
		"platform_endpoints": uint64(platformEndpoints),
	}
	for kind, value := range kindCounts {
		stExtenderPoolCounts.WithLabelValues(phase, kind).Set(float64(value))
	}
	stExtenderPoolWeightShare.WithLabelValues(phase).Set(stExtenderWeightShare(split.Weight, eligibleProvider))
}

// Z' / (W_k + Z'), 0 for an epoch with no weight.
func stExtenderWeightShare(weight uint64, eligibleProvider uint64) float64 {
	total := float64(weight) + float64(eligibleProvider)
	if total == 0 {
		return 0
	}
	return float64(weight) / total
}
