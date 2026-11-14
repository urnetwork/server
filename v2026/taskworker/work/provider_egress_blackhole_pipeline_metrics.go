// Fixed process-local decisions explain admission without provider identities.
package work

import "github.com/prometheus/client_golang/prometheus"

// Entry/lookup/selection are events; a pipeline records its first stop decision
// before joining tails. Counters are not task counts, acknowledgements or rows.
var egressProbeBlackholePipelineDecisions = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "urnetwork_egress_probe_blackhole_pipeline_decisions_total",
	Help: "Fixed blackhole pipeline entry, lookup, selection and first admission-stop decisions; not task identities or durable verdicts",
}, []string{"decision"})

// Preinitialized children distinguish a quiet capable executable from absence.
func init() {
	for _, decision := range []string{
		"pipeline_started", "lookup_started", "successor_selected", "cutoff",
		"partial_due", "no_unseen_due", "error", "cohort_cap", "canceled",
		"full_error", "full_finished", "no_full_serial", "serial_geometry",
	} {
		egressProbeBlackholePipelineDecisions.WithLabelValues(decision)
	}
	prometheus.MustRegister(egressProbeBlackholePipelineDecisions)
}
