// Private probe-tunnel auth diagnostics, not provider quality evidence.
package work

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/urnetwork/connect/v2026"
)

type providerEgressAuthMetrics struct {
	observations connect.AuthNetworkClientObservations
	desc         *prometheus.Desc
}

// All 36 cells are exported, including zeros, to distinguish an instrumented
// process from an older executable. Recording itself is atomic and does not
// wait for Prometheus collection.
func newProviderEgressAuthMetrics() *providerEgressAuthMetrics {
	return &providerEgressAuthMetrics{
		desc: prometheus.NewDesc("urnetwork_egress_probe_auth_requests_total",
			"Completed logical synchronous probe auth calls by last actual POST phase and outcome; excludes hello and individual route counts",
			[]string{"phase", "result"}, nil),
	}
}

var egressProbeAuth = newProviderEgressAuthMetrics()

func init() {
	prometheus.MustRegister(egressProbeAuth)
}

func (self *providerEgressAuthMetrics) Describe(ch chan<- *prometheus.Desc) {
	ch <- self.desc
}

func (self *providerEgressAuthMetrics) Collect(ch chan<- prometheus.Metric) {
	for _, value := range self.observations.Snapshot() {
		ch <- prometheus.MustNewConstMetric(self.desc, prometheus.CounterValue, float64(value.Count), value.Phase, value.Result)
	}
}
