// DNS waves are joined to the same private tunnel's contemporaneous setup
// state. This process aggregate contains no provider labels or verdicts.
package work

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/urnetwork/server/v2026/qualityprobe/providertunnel"
)

// Observation is a fixed atomic increment; collection never blocks a probe.
type providerEgressDnsMetrics struct {
	observations                      providertunnel.DnsObservations
	desc                              *prometheus.Desc
	routeCountDesc                    *prometheus.Desc
	routeSecondsDesc                  *prometheus.Desc
	contractDesc, contractSecondsDesc *prometheus.Desc
}

// Every process exports all 45 fixed cells, including zeros, so an absent
// metric on an older executable cannot masquerade as a healthy observation.
func newProviderEgressDnsMetrics() *providerEgressDnsMetrics {
	return &providerEgressDnsMetrics{
		contractDesc: prometheus.NewDesc("urnetwork_egress_probe_dns_contract_waves_total",
			"DNS wave outcomes joined to the exact tunnel's end-of-wave no-provider-write local contract witness; not_proved is unknown cause",
			[]string{"result", "evidence"}, nil),
		contractSecondsDesc: prometheus.NewDesc("urnetwork_egress_probe_dns_contract_wave_seconds_total",
			"DNS wave wall residence by exact tunnel contract witness; not contract-acquisition duration",
			[]string{"result", "evidence"}, nil),
		routeCountDesc: prometheus.NewDesc("urnetwork_egress_probe_dns_route_waves_total",
			"Completed DNS waves classified by endpoint route snapshots and source-owned admission time; snapshots are not complete route history",
			[]string{"result", "route"}, nil),
		routeSecondsDesc: prometheus.NewDesc("urnetwork_egress_probe_dns_route_seconds_total",
			"DNS wave seconds before or after the currently observed route admission, or unattributed; before admission is not continuous setup time",
			[]string{"result", "route", "phase"}, nil),
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
	ch <- self.routeCountDesc
	ch <- self.routeSecondsDesc
	ch <- self.contractDesc
	ch <- self.contractSecondsDesc
}

// Reads a bounded snapshot without retaining any provider lifecycle owner.
func (self *providerEgressDnsMetrics) Collect(ch chan<- prometheus.Metric) {
	for _, value := range self.observations.ContractSnapshot() {
		ch <- prometheus.MustNewConstMetric(self.contractDesc, prometheus.CounterValue, float64(value.Count), value.Result, value.Evidence)
		ch <- prometheus.MustNewConstMetric(self.contractSecondsDesc, prometheus.CounterValue, value.WaveSeconds, value.Result, value.Evidence)
	}
	for _, value := range self.observations.RouteTimingSnapshot() {
		ch <- prometheus.MustNewConstMetric(self.routeCountDesc, prometheus.CounterValue, float64(value.Count), value.Result, value.Route)
		for phase, seconds := range map[string]float64{
			"before_current_admission": value.BeforeCurrentAdmissionSeconds,
			"after_current_admission":  value.AfterCurrentAdmissionSeconds,
			"unattributed":             value.UnattributedSeconds,
		} {
			ch <- prometheus.MustNewConstMetric(self.routeSecondsDesc, prometheus.CounterValue, seconds, value.Result, value.Route, phase)
		}
	}
	for _, value := range self.observations.Snapshot() {
		ch <- prometheus.MustNewConstMetric(self.desc, prometheus.CounterValue, float64(value.Count), value.Result, value.Path)
	}
}
