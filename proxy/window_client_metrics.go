// Exports what the hosted devices' windows do with their window clients
// (connect ip_remote_multi_client_window_client_stats.go): every candidate an
// expansion builds by outcome and the pass state that built it, every pass
// that wants to grow by the state it found, warnings and removals by cause,
// and the live windows by the state their last resize pass ended in.
//
// Every device of the process counts into one instance, read at scrape time.
// The labels are connect's fixed vocabularies; no identity can become a label.
package proxy

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/connect"
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

var windowsDesc = prometheus.NewDesc(
	"urnetwork_proxy_windows",
	"Live hosted windows by the state their last resize pass ended in",
	[]string{"window", "state"}, nil,
)

// The read side of connect.WindowClientStats.
type windowClientStatsSource interface {
	VisitCandidates(visit func(window string, state string, outcome string, count uint64))
	VisitExpandPasses(visit func(window string, state string, count uint64))
	VisitWarnings(visit func(window string, cause string, count uint64))
	VisitRemovals(visit func(window string, cause string, count uint64))
	VisitWindows(visit func(window string, state string, count int64))
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
	ch <- windowsDesc
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
	self.stats.VisitWindows(func(window string, state string, count int64) {
		ch <- prometheus.MustNewConstMetric(windowsDesc, prometheus.GaugeValue, float64(count), window, state)
	})
}

func init() {
	prometheus.MustRegister(newWindowClientStatsCollector(defaultWindowClientStats))
}
