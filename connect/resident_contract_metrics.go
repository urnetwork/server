// Finite allowance and compatibility outcomes, without client, pair, error, or
// generation labels. All series exist at construction, including zero values.
package connect

import "github.com/prometheus/client_golang/prometheus"

type contractAllowanceOutcome uint8

const (
	contractAllowanceRedisPositive contractAllowanceOutcome = iota
	contractAllowanceRedisNegative
	contractAllowanceRedisUnknown
	contractAllowanceRedisError
	contractAllowanceFallbackDisabled
	contractAllowanceFallbackSaturated
	contractAllowanceFallbackStarted
	contractAllowanceFallbackPositive
	contractAllowanceFallbackNegative
	contractAllowanceFallbackError
	contractAllowanceCanceled
	contractAllowanceCheckRefused
	contractAllowanceCount
)

// Counters are shared observations only; admission is explicitly exchange-owned.
type residentContractAllowanceMetrics struct {
	outcomes [contractAllowanceCount]prometheus.Counter
}

// Register every allowed label once so traffic cannot increase cardinality.
func newResidentContractAllowanceMetrics(registerer prometheus.Registerer) *residentContractAllowanceMetrics {
	counter := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "urnetwork_connect_contract_allowance_total",
		Help: "Admitted Redis/source check outcomes and refused unchecked offers; temporary source fallback is separately bounded.",
	}, []string{"outcome"})
	metrics := &residentContractAllowanceMetrics{}
	for outcome, label := range [...]string{
		"redis_positive", "redis_negative", "redis_unknown", "redis_error",
		"fallback_disabled", "fallback_saturated", "fallback_started",
		"fallback_positive", "fallback_negative", "fallback_error", "canceled", "check_refused",
	} {
		metrics.outcomes[outcome] = counter.WithLabelValues(label)
	}
	registerer.MustRegister(counter)
	return metrics
}

// Internal callers use only the closed outcome enumeration above.
func (self *residentContractAllowanceMetrics) add(outcome contractAllowanceOutcome) {
	self.outcomes[outcome].Inc()
}

var defaultResidentContractAllowanceMetrics = newResidentContractAllowanceMetrics(prometheus.DefaultRegisterer)
