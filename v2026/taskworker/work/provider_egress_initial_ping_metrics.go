// Initial-ping dependencies are local diagnostic snapshots, not provider
// quality evidence or a partition of the completed URL-turn population.
package work

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/urnetwork/connect/v2026"
)

type providerEgressInitialPingMetrics struct {
	observations connect.InitialPingObservations
	count        *prometheus.Desc
	seconds      *prometheus.Desc
	started      *prometheus.Desc
	pathCount    *prometheus.Desc
	pathSeconds  *prometheus.Desc
}

// Export all 97 fixed scalar cells including zeros (49 original + 48 path). Multiple windows and
// retries can evaluate the same tunnel; an acknowledged ping need not be
// admitted. No provider, client, destination, URL or error-string label exists.
func newProviderEgressInitialPingMetrics() *providerEgressInitialPingMetrics {
	return &providerEgressInitialPingMetrics{
		pathCount: prometheus.NewDesc("urnetwork_egress_probe_initial_ping_path_evaluations_total",
			"Terminal initial-ping evaluations by exact ping local route-write and ACK callback witnesses; accepted is not remote receipt and callback success is not admission",
			[]string{"outcome", "route_write", "ack_callback"}, nil),
		pathSeconds: prometheus.NewDesc("urnetwork_egress_probe_initial_ping_path_seconds_total",
			"Whole terminal initial-ping residence bucketed by exact ping path witnesses; these are terminal snapshots, not phase durations",
			[]string{"outcome", "route_write", "ack_callback"}, nil),
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
	ch <- self.pathCount
	ch <- self.pathSeconds
}

func (self *providerEgressInitialPingMetrics) Collect(ch chan<- prometheus.Metric) {
	for _, value := range self.observations.Snapshot() {
		ch <- prometheus.MustNewConstMetric(self.count, prometheus.CounterValue, float64(value.Count), value.Outcome, value.Dependency)
		ch <- prometheus.MustNewConstMetric(self.seconds, prometheus.CounterValue, value.Seconds, value.Outcome, value.Dependency)
	}
	for _, value := range self.observations.PathSnapshot() {
		ch <- prometheus.MustNewConstMetric(self.pathCount, prometheus.CounterValue, float64(value.Count), value.Outcome, value.RouteWrite, value.AckCallback)
		ch <- prometheus.MustNewConstMetric(self.pathSeconds, prometheus.CounterValue, value.Seconds, value.Outcome, value.RouteWrite, value.AckCallback)
	}
	ch <- prometheus.MustNewConstMetric(self.started, prometheus.CounterValue, float64(self.observations.Started()))
}
