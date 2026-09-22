package server

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/urnetwork/connect"
)

// NewH1PlusCollector exports a fixed numeric schema. Callers pass a compile-time
// subsystem and protocol; no request fields, credentials, origins or identities
// can create metric labels.
func NewH1PlusCollector(subsystem, protocol string, stats *connect.H1PlusStats) prometheus.Collector {
	names := []string{"attempts", "accepted", "fallbacks", "auth_failures", "handshake_nanoseconds", "flushes", "writes", "messages", "bytes", "read_errors", "write_errors", "fallback_rejected", "fallback_invalid", "fallback_io", "fallback_proxy", "websocket_selected", "handshake_failures"}
	c := &h1PlusCollector{stats: stats, descs: make([]*prometheus.Desc, len(names))}
	for i, name := range names {
		c.descs[i] = prometheus.NewDesc("urnetwork_"+subsystem+"_h1plus_"+name+"_total", "H1+ bounded carrier counter: "+name, nil, prometheus.Labels{"protocol": protocol})
	}
	return c
}

type h1PlusCollector struct {
	stats *connect.H1PlusStats
	descs []*prometheus.Desc
}

func (c *h1PlusCollector) Describe(out chan<- *prometheus.Desc) {
	for _, desc := range c.descs {
		out <- desc
	}
}
func (c *h1PlusCollector) Collect(out chan<- prometheus.Metric) {
	s := c.stats.Snapshot()
	values := [...]uint64{s.Attempts, s.Accepted, s.Fallbacks, s.AuthFailures, s.HandshakeNanos, s.Flushes, s.Writes, s.Messages, s.Bytes, s.ReadErrors, s.WriteErrors, s.FallbackRejected, s.FallbackInvalid, s.FallbackIO, s.FallbackProxy, s.WebSocketSelected, s.HandshakeFailures}
	for i, value := range values {
		out <- prometheus.MustNewConstMetric(c.descs[i], prometheus.CounterValue, float64(value))
	}
}
