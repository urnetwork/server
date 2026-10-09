// Readiness counters retain the existing probe's finite observation at its
// caller boundary. They perform no read and do not attest financial progress.
package model

import "github.com/prometheus/client_golang/prometheus"

func newLegacyPayerReadinessCounter() *prometheus.CounterVec {
	counter := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "urnetwork_legacy_payer_index_readiness_total",
		Help: "Existing due-index readiness observations by fixed caller, outcome and cache use; not catalog faults or financial commits.",
	}, []string{"caller", "outcome", "cached"})
	for _, caller := range []string{"dispatcher", "payer"} {
		for _, outcome := range []string{"ready", "catalog_invalid", "deadline", "canceled", "read_error", "unknown"} {
			counter.WithLabelValues(caller, outcome, "false")
		}
		counter.WithLabelValues(caller, "ready", "true")
	}
	return counter
}

var legacyPayerReadinessCounter = newLegacyPayerReadinessCounter()

func init() {
	prometheus.MustRegister(legacyPayerReadinessCounter)
}

// Caller and outcome are closed vocabularies even if a future producer passes
// an unexpected value. Cached proof is positive authority only for ready.
func recordLegacyPayerReadiness(counter *prometheus.CounterVec, caller string, observation LegacySettlementPayerIndexReadiness) {
	switch caller {
	case "dispatcher", "payer":
	default:
		caller = "unknown"
	}
	outcome := observation.Outcome
	switch outcome {
	case "ready", "catalog_invalid", "deadline", "canceled", "read_error":
	default:
		outcome = "unknown"
	}
	cached := "false"
	if outcome == "ready" && observation.Cached {
		cached = "true"
	}
	counter.WithLabelValues(caller, outcome, cached).Inc()
}
