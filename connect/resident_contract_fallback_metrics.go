// Fixed failure reasons separate deadline pressure from source and lease
// failures. No error text, client id, pair, or request label is exported.
package connect

import "github.com/prometheus/client_golang/prometheus"

type contractFallbackErrorReason uint8

const (
	contractFallbackDeadline contractFallbackErrorReason = iota
	contractFallbackCanceled
	contractFallbackPhysicalError
	contractFallbackUnknown
	contractFallbackExpiredLease
	contractFallbackErrorReasonCount
)

// Shared observations do not own or change any exchange admission budget.
type residentContractFallbackMetrics struct {
	errors        [contractFallbackErrorReasonCount]prometheus.Counter
	sourceSeconds prometheus.Histogram
}

// Every reason is registered at zero. The unlabelled histogram covers the
// admitted source call and its unwind, excluding Redis and admission refusal.
func newResidentContractFallbackMetrics(registerer prometheus.Registerer) *residentContractFallbackMetrics {
	counter := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "urnetwork_connect_contract_fallback_errors_total",
		Help: "Temporary contract source failures by a closed reason set; deadline and cancellation take precedence over the returned error.",
	}, []string{"reason"})
	metrics := &residentContractFallbackMetrics{
		sourceSeconds: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "urnetwork_connect_contract_fallback_source_seconds",
			Help:    "Time in an admitted temporary source call including cleanup, excluding Redis and refused admission.",
			Buckets: []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.2, 0.25, 0.5, 1},
		}),
	}
	for reason, label := range [...]string{"deadline", "canceled", "physical_error", "unknown", "expired_lease"} {
		metrics.errors[reason] = counter.WithLabelValues(label)
	}
	registerer.MustRegister(counter, metrics.sourceSeconds)
	return metrics
}

// Callers can only choose one of the predeclared reasons.
func (self *residentContractFallbackMetrics) add(reason contractFallbackErrorReason) {
	self.errors[reason].Inc()
}

var defaultResidentContractFallbackMetrics = newResidentContractFallbackMetrics(prometheus.DefaultRegisterer)
