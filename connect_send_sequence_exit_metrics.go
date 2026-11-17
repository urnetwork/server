package server

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/connect"
)

// sendSequenceExitCollector exposes connect's per-reason count of send
// sequence exits (connect transfer_sequence_exit.go) in every server executable
// that embeds the connect library: the connect service most, and the proxy's
// hosted devices and the taskworker's probes. Each exit's detail line is at
// V(1) since 2026-11-15 (CPUOPT6 A #2(b)); the count keeps its rate by reason
// with verbose logging off. One snapshot per scrape.
type sendSequenceExitCollector struct {
	exits *prometheus.Desc
}

func newSendSequenceExitCollector() *sendSequenceExitCollector {
	return &sendSequenceExitCollector{
		exits: prometheus.NewDesc(
			"urnetwork_connect_send_sequence_exits_total",
			"Send sequences that ended on a terminal owner decision, by the reason of each sequence's first decision (connect transfer_sequence_exit.go); idle ends are not counted, and each exit's detail line is logged at V(1)",
			[]string{"reason"},
			nil,
		),
	}
}

func (self *sendSequenceExitCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- self.exits
}

func (self *sendSequenceExitCollector) Collect(ch chan<- prometheus.Metric) {
	for reason, count := range connect.GetSendSequenceExitCounts() {
		ch <- prometheus.MustNewConstMetric(self.exits, prometheus.CounterValue, float64(count), reason)
	}
}

func init() {
	prometheus.MustRegister(newSendSequenceExitCollector())
}
