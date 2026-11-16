// One collector-owned snapshot publishes exact counts, lower bounds and
// visibility together. Scrapers never combine fields from different refreshes.
package controller

import (
	"context"
	"math"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/urnetwork/server/v2026/model"
)

// Publication belongs to the stats goroutine; Collect is concurrent-safe.
// The registerer is injected so tests own their metrics without global hooks.
type statsOpenContractMetrics struct {
	registerer   prometheus.Registerer
	registered   bool
	stateLock    sync.Mutex
	snapshot     model.OpenContractStatsSnapshot
	available    bool
	exactDescs   [3]*prometheus.Desc
	lowerDesc    *prometheus.Desc
	statusDesc   *prometheus.Desc
	observedDesc *prometheus.Desc
}

// Registration stays lazy: API processes importing controller do not export
// Taskworker collector gauges before this owner has attempted a refresh.
func newStatsOpenContractMetrics(registerer prometheus.Registerer) *statsOpenContractMetrics {
	return &statsOpenContractMetrics{
		registerer: registerer,
		exactDescs: [3]*prometheus.Desc{
			prometheus.NewDesc("urnetwork_stats_open_contracts", "Exact open contracts; NaN when capped or unavailable", nil, nil),
			prometheus.NewDesc("urnetwork_stats_open_contracts_with_extender", "Exact open contracts with extender parties; NaN when capped or unavailable", nil, nil),
			prometheus.NewDesc("urnetwork_stats_open_disputes", "Exact undecided disputes; NaN when capped or unavailable", nil, nil),
		},
		lowerDesc:    prometheus.NewDesc("urnetwork_stats_contract_open_lower_bound", "Observed lower bound; NaN when unavailable", []string{"kind"}, nil),
		statusDesc:   prometheus.NewDesc("urnetwork_stats_contract_open_status", "One-hot exact, capped or unavailable snapshot status", []string{"kind", "status"}, nil),
		observedDesc: prometheus.NewDesc("urnetwork_stats_contract_open_observed_at_seconds", "PostgreSQL statement snapshot Unix time; zero when unavailable", []string{"kind"}, nil),
	}
}

// Describe never depends on a sampled population or caller identity.
func (self *statsOpenContractMetrics) Describe(ch chan<- *prometheus.Desc) {
	for _, desc := range self.exactDescs {
		ch <- desc
	}
	ch <- self.lowerDesc
	ch <- self.statusDesc
	ch <- self.observedDesc
}

// Copy under the lock, then emit outside it so a slow scrape cannot retain the
// publication lock. All fields in this scrape share the copied generation.
func (self *statsOpenContractMetrics) Collect(ch chan<- prometheus.Metric) {
	var snapshot model.OpenContractStatsSnapshot
	var available bool
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		snapshot, available = self.snapshot, self.available
	}()
	for i, row := range []struct {
		kind  string
		count int64
		exact bool
	}{
		{kind: "open", count: snapshot.OpenContracts, exact: snapshot.OpenContractsExact},
		{kind: "with_extender", count: snapshot.OpenContractsWithExtender, exact: snapshot.OpenContractsExact},
		{kind: "dispute", count: snapshot.OpenDisputes, exact: snapshot.OpenDisputesExact},
	} {
		exact, lower, observed, status := math.NaN(), math.NaN(), float64(0), "unavailable"
		if available {
			lower, observed, status = float64(row.count), float64(snapshot.ObservedAt.UnixMicro())/1000000, "capped"
			if row.exact {
				exact, status = lower, "exact"
			}
		}
		ch <- prometheus.MustNewConstMetric(self.exactDescs[i], prometheus.GaugeValue, exact)
		ch <- prometheus.MustNewConstMetric(self.lowerDesc, prometheus.GaugeValue, lower, row.kind)
		ch <- prometheus.MustNewConstMetric(self.observedDesc, prometheus.GaugeValue, observed, row.kind)
		for _, candidate := range []string{"exact", "capped", "unavailable"} {
			value := float64(0)
			if candidate == status {
				value = 1
			}
			ch <- prometheus.MustNewConstMetric(self.statusDesc, prometheus.GaugeValue, value, row.kind, candidate)
		}
	}
}

// The only writer is the stats owner; readers synchronize through stateLock.
func (self *statsOpenContractMetrics) publish(snapshot model.OpenContractStatsSnapshot, available bool) {
	if !self.registered {
		self.registerer.MustRegister(self)
		self.registered = true
	}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.snapshot, self.available = snapshot, available && !snapshot.ObservedAt.IsZero()
	}()
}

var statsOpenContractsMetrics = newStatsOpenContractMetrics(prometheus.DefaultRegisterer)

// A timeout affects only these gauges, not the following hourly/user stats.
// Earlier refresh failures leave a timestamp that dashboards must age out.
func statsRefreshOpenContracts(ctx context.Context) {
	snapshot, err := model.ReadOpenContractStats(ctx)
	statsOpenContractsMetrics.publish(snapshot, err == nil)
}
