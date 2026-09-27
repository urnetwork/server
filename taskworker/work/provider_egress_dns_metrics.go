// DNS waves are joined to the same private tunnel's contemporaneous setup
// state. This process aggregate contains no provider labels or verdicts.
package work

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/urnetwork/server/qualityprobe/providertunnel"
)

// Observation is a fixed atomic increment; collection never blocks a probe.
type providerEgressDnsMetrics struct {
	observations providertunnel.DnsObservations
	desc         *prometheus.Desc
}

// Every process exports all 45 fixed cells, including zeros, so an absent
// metric on an older executable cannot masquerade as a healthy observation.
func newProviderEgressDnsMetrics() *providerEgressDnsMetrics {
	return &providerEgressDnsMetrics{
		desc: prometheus.NewDesc("urnetwork_egress_probe_dns_waves_total",
			"Completed private-tunnel DNS waves by local outcome and same-tunnel setup state; neither provider verdicts nor individual DoH transport attempts",
			[]string{"result", "path"}, nil),
	}
}

var egressProbeDns = newProviderEgressDnsMetrics()

func init() {
	prometheus.MustRegister(egressProbeDns)
}

// The descriptor's vocabulary is owned by providertunnel's fixed snapshot.
func (self *providerEgressDnsMetrics) Describe(ch chan<- *prometheus.Desc) {
	ch <- self.desc
}

// Reads a bounded snapshot without retaining any provider lifecycle owner.
func (self *providerEgressDnsMetrics) Collect(ch chan<- prometheus.Metric) {
	for _, value := range self.observations.Snapshot() {
		ch <- prometheus.MustNewConstMetric(self.desc, prometheus.CounterValue, float64(value.Count), value.Result, value.Path)
	}
}
