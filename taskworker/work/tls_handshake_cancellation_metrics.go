// Taskworker process counters for current-epoch TLS handshake cancellation.
package work

import (
	"strconv"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/urnetwork/connect"
)

// Connect owns the counters. This reader gathers a fixed vocabulary without
// retaining clients or adding work to their handshake callbacks.
type tlsHandshakeCancellationMetrics struct {
	count    *prometheus.Desc
	snapshot func() []connect.TlsHandshakeCancellationCount
}

// Pin the labels to the local session role, companion flag and cancellation source.
func newTlsHandshakeCancellationMetrics(snapshot func() []connect.TlsHandshakeCancellationCount) *tlsHandshakeCancellationMetrics {
	return &tlsHandshakeCancellationMetrics{
		count: prometheus.NewDesc(
			"urnetwork_taskworker_tls_handshake_cancellations_total",
			"Coalesced current-epoch TLS handshake context cancellations by first observed cancellation owner, local role and companion flag",
			[]string{"source", "role", "companion"}, nil,
		),
		snapshot: snapshot,
	}
}

// The Taskworker stats pusher gathers its default registry after readiness.
func init() {
	prometheus.MustRegister(newTlsHandshakeCancellationMetrics(connect.GetTlsHandshakeCancellationCounts))
}

// Advertise the counter even before the first cancellation.
func (self *tlsHandshakeCancellationMetrics) Describe(ch chan<- *prometheus.Desc) {
	ch <- self.count
}

// Emit every cell, including process-lifetime zero counts.
func (self *tlsHandshakeCancellationMetrics) Collect(ch chan<- prometheus.Metric) {
	for _, cell := range self.snapshot() {
		ch <- prometheus.MustNewConstMetric(self.count, prometheus.CounterValue, float64(cell.Count),
			cell.Source, cell.Role, strconv.FormatBool(cell.Companion))
	}
}
