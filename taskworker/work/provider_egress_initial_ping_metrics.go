// Initial-ping dependencies are local diagnostic snapshots, not provider
// quality evidence or a partition of the completed URL-turn population.
package work

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/urnetwork/connect"
)

type providerEgressInitialPingMetrics struct {
	observations connect.InitialPingObservations
	count        *prometheus.Desc
	seconds      *prometheus.Desc
	started      *prometheus.Desc
}

// Export all 49 fixed scalar cells including zeros. Multiple windows and
// retries can evaluate the same tunnel; an acknowledged ping need not be
// admitted. No provider, client, destination, URL or error-string label exists.
func newProviderEgressInitialPingMetrics() *providerEgressInitialPingMetrics {
	return &providerEgressInitialPingMetrics{
		count: prometheus.NewDesc("urnetwork_egress_probe_initial_ping_evaluations_total",
			"Terminal initial-ping evaluations after successful construction by terminal outcome and dependency snapshot; acknowledged is not admission",
			[]string{"outcome", "dependency"}, nil),
		seconds: prometheus.NewDesc("urnetwork_egress_probe_initial_ping_seconds_total",
			"Sum of terminal initial-ping residence including contract and carrier work; endpoint dependency snapshots do not attribute the whole duration",
			[]string{"outcome", "dependency"}, nil),
		started: prometheus.NewDesc("urnetwork_egress_probe_initial_ping_started_total",
			"Initial-ping evaluations started after successful construction; independent of terminal cells and accepted measured URL outcomes",
			nil, nil),
	}
}

var egressProbeInitialPing = newProviderEgressInitialPingMetrics()

func init() {
	prometheus.MustRegister(egressProbeInitialPing)
}

func (self *providerEgressInitialPingMetrics) Describe(ch chan<- *prometheus.Desc) {
	ch <- self.count
	ch <- self.seconds
	ch <- self.started
}

func (self *providerEgressInitialPingMetrics) Collect(ch chan<- prometheus.Metric) {
	for _, value := range self.observations.Snapshot() {
		ch <- prometheus.MustNewConstMetric(self.count, prometheus.CounterValue, float64(value.Count), value.Outcome, value.Dependency)
		ch <- prometheus.MustNewConstMetric(self.seconds, prometheus.CounterValue, value.Seconds, value.Outcome, value.Dependency)
	}
	ch <- prometheus.MustNewConstMetric(self.started, prometheus.CounterValue, float64(self.observations.Started()))
}
