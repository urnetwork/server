package server

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/connect/v2026"
)

// tlsVerifiedChainCacheCollector exposes connect's process verified-chain
// cache (connect net_tls_chain_cache.go) in every server executable: the
// taskworker's egress probe and the proxy's hosted devices dial the platform
// through it, and the api and connect services make a few such dials of their
// own. One snapshot per scrape keeps the counts consistent with each other.
type tlsVerifiedChainCacheCollector struct {
	lookups   *prometheus.Desc
	stores    *prometheus.Desc
	evictions *prometheus.Desc
	entries   *prometheus.Desc
}

func newTlsVerifiedChainCacheCollector() *tlsVerifiedChainCacheCollector {
	return &tlsVerifiedChainCacheCollector{
		lookups: prometheus.NewDesc(
			"urnetwork_tls_verified_chain_cache_lookups_total",
			"Pinned-root tls handshakes by whether a remembered verification of the same chain, server name and root pool answered (hit) or the chain was verified in full (miss)",
			[]string{"result"},
			nil,
		),
		stores: prometheus.NewDesc(
			"urnetwork_tls_verified_chain_cache_stores_total",
			"Chain verifications remembered",
			nil,
			nil,
		),
		evictions: prometheus.NewDesc(
			"urnetwork_tls_verified_chain_cache_evictions_total",
			"Remembered chain verifications dropped to stay within the entry bound",
			nil,
			nil,
		),
		entries: prometheus.NewDesc(
			"urnetwork_tls_verified_chain_cache_entries",
			"Remembered chain verifications held now",
			nil,
			nil,
		),
	}
}

func (self *tlsVerifiedChainCacheCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- self.lookups
	ch <- self.stores
	ch <- self.evictions
	ch <- self.entries
}

func (self *tlsVerifiedChainCacheCollector) Collect(ch chan<- prometheus.Metric) {
	stats := connect.GetTlsVerifiedChainCacheStats()
	ch <- prometheus.MustNewConstMetric(self.lookups, prometheus.CounterValue, float64(stats.HitCount), "hit")
	ch <- prometheus.MustNewConstMetric(self.lookups, prometheus.CounterValue, float64(stats.MissCount), "miss")
	ch <- prometheus.MustNewConstMetric(self.stores, prometheus.CounterValue, float64(stats.StoreCount))
	ch <- prometheus.MustNewConstMetric(self.evictions, prometheus.CounterValue, float64(stats.EvictCount))
	ch <- prometheus.MustNewConstMetric(self.entries, prometheus.GaugeValue, float64(stats.EntryCount))
}

func init() {
	prometheus.MustRegister(newTlsVerifiedChainCacheCollector())
}
