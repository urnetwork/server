// Exports what the hosted devices' windows do with their window clients
// (connect ip_remote_multi_client_window_client_stats.go): every candidate an
// expansion builds by outcome and the pass state that built it, every pass
// that wants to grow by the state it found, warnings and removals by cause,
// and the live windows by the state their last resize pass ended in. The
// capability-aware placement (server FP2-PROTOCOL.md §8.2) adds the live
// window clients by window and the labels FindProviders2 gave them, the
// flows by the window of their first exit, ip version and protocol, and the
// flows no window held a compatible exit for.
//
// Every device of the process counts into one instance, read at scrape time.
// The labels are connect's fixed vocabularies; no identity can become a label.
package proxy

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/connect/v2026"
)

// The process's window client counters, shared by every device of every
// manager (see DefaultProxyDeviceManagerSettings).
var defaultWindowClientStats = connect.NewWindowClientStats()

var windowCandidatesDesc = prometheus.NewDesc(
	"urnetwork_proxy_window_candidates_total",
	"Window client candidates hosted windows built, by the state of the expansion pass that built them and how they ended",
	[]string{"window", "state", "outcome"}, nil,
)

var windowExpandPassesDesc = prometheus.NewDesc(
	"urnetwork_proxy_window_expand_passes_total",
	"Hosted window resize passes that wanted to grow, by the window state they found",
	[]string{"window", "state"}, nil,
)

var windowWarningsDesc = prometheus.NewDesc(
	"urnetwork_proxy_window_warnings_total",
	"Hosted window clients that entered the warned state, by cause",
	[]string{"window", "cause"}, nil,
)

var windowRemovalsDesc = prometheus.NewDesc(
	"urnetwork_proxy_window_removals_total",
	"Hosted window clients removed from their window, by cause",
	[]string{"window", "cause"}, nil,
)

var windowBalanceProbesDesc = prometheus.NewDesc(
	"urnetwork_proxy_window_balance_probes_total",
	"Balance probes hosted windows sent while the payer's balance refused their contracts, by what each learned",
	[]string{"window", "outcome"}, nil,
)

var windowsDesc = prometheus.NewDesc(
	"urnetwork_proxy_windows",
	"Live hosted windows by the state their last resize pass ended in",
	[]string{"window", "state"}, nil,
)

var windowClientsDesc = prometheus.NewDesc(
	"urnetwork_proxy_window_clients",
	"Live hosted window clients by window and the ip and protocol labels FindProviders2 gave them",
	[]string{"window", "ip", "protocol"}, nil,
)

var windowFlowsDesc = prometheus.NewDesc(
	"urnetwork_proxy_window_flows_total",
	"Hosted device flows by the window of the exit that first carried them, their ip version and protocol",
	[]string{"window", "ip_version", "protocol"}, nil,
)

var windowNoCompatibleDesc = prometheus.NewDesc(
	"urnetwork_proxy_window_no_compatible_total",
	"Hosted device flows answered unreachable because no window held an exit labelled to carry them, by ip version and protocol",
	[]string{"ip_version", "protocol"}, nil,
)

// The read side of connect.WindowClientStats.
type windowClientStatsSource interface {
	VisitCandidates(visit func(window string, state string, outcome string, count uint64))
	VisitExpandPasses(visit func(window string, state string, count uint64))
	VisitWarnings(visit func(window string, cause string, count uint64))
	VisitRemovals(visit func(window string, cause string, count uint64))
	VisitBalanceProbes(visit func(window string, outcome string, count uint64))
	VisitWindows(visit func(window string, state string, count int64))
	VisitWindowClients(visit func(window string, ip string, protocol string, count int64))
	VisitWindowFlows(visit func(window string, ipVersion string, protocol string, count uint64))
	VisitNoCompatible(visit func(ipVersion string, protocol string, count uint64))
}

// Reads one counters instance at scrape time.
type windowClientStatsCollector struct {
	stats windowClientStatsSource
}

func newWindowClientStatsCollector(stats windowClientStatsSource) *windowClientStatsCollector {
	return &windowClientStatsCollector{
		stats: stats,
	}
}

func (self *windowClientStatsCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- windowCandidatesDesc
	ch <- windowExpandPassesDesc
	ch <- windowWarningsDesc
	ch <- windowRemovalsDesc
	ch <- windowBalanceProbesDesc
	ch <- windowsDesc
	ch <- windowClientsDesc
	ch <- windowFlowsDesc
	ch <- windowNoCompatibleDesc
}

func (self *windowClientStatsCollector) Collect(ch chan<- prometheus.Metric) {
	self.stats.VisitCandidates(func(window string, state string, outcome string, count uint64) {
		ch <- prometheus.MustNewConstMetric(windowCandidatesDesc, prometheus.CounterValue, float64(count), window, state, outcome)
	})
	self.stats.VisitExpandPasses(func(window string, state string, count uint64) {
		ch <- prometheus.MustNewConstMetric(windowExpandPassesDesc, prometheus.CounterValue, float64(count), window, state)
	})
	self.stats.VisitWarnings(func(window string, cause string, count uint64) {
		ch <- prometheus.MustNewConstMetric(windowWarningsDesc, prometheus.CounterValue, float64(count), window, cause)
	})
	self.stats.VisitRemovals(func(window string, cause string, count uint64) {
		ch <- prometheus.MustNewConstMetric(windowRemovalsDesc, prometheus.CounterValue, float64(count), window, cause)
	})
	self.stats.VisitBalanceProbes(func(window string, outcome string, count uint64) {
		ch <- prometheus.MustNewConstMetric(windowBalanceProbesDesc, prometheus.CounterValue, float64(count), window, outcome)
	})
	self.stats.VisitWindows(func(window string, state string, count int64) {
		ch <- prometheus.MustNewConstMetric(windowsDesc, prometheus.GaugeValue, float64(count), window, state)
	})
	self.stats.VisitWindowClients(func(window string, ip string, protocol string, count int64) {
		ch <- prometheus.MustNewConstMetric(windowClientsDesc, prometheus.GaugeValue, float64(count), window, ip, protocol)
	})
	self.stats.VisitWindowFlows(func(window string, ipVersion string, protocol string, count uint64) {
		ch <- prometheus.MustNewConstMetric(windowFlowsDesc, prometheus.CounterValue, float64(count), window, ipVersion, protocol)
	})
	self.stats.VisitNoCompatible(func(ipVersion string, protocol string, count uint64) {
		ch <- prometheus.MustNewConstMetric(windowNoCompatibleDesc, prometheus.CounterValue, float64(count), ipVersion, protocol)
	})
}

func init() {
	prometheus.MustRegister(newWindowClientStatsCollector(defaultWindowClientStats))
}
