package controller

import (
	"context"
	"math"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Source age is checked at scrape time, including when the refresh loop stalls.
// Each scrape owns one generation of counts, source times and availability.
type statsContractHourMetrics struct {
	registerer                         prometheus.Registerer
	registerOnce                       sync.Once
	now                                func() time.Time
	mu                                 sync.Mutex
	snapshot                           statsContractHourSnapshot
	available                          bool
	counts                             [3]*prometheus.Desc
	windowEnd, completed, availability *prometheus.Desc
}

func newStatsContractHourMetrics(registerer prometheus.Registerer, now func() time.Time) *statsContractHourMetrics {
	return &statsContractHourMetrics{
		registerer: registerer, now: now,
		counts: [3]*prometheus.Desc{
			prometheus.NewDesc("urnetwork_stats_contracts_24h", "Transfer contracts in the shared hourly window; NaN when stale or unavailable", nil, nil),
			prometheus.NewDesc("urnetwork_stats_contracts_with_extender_24h", "Transfer contracts with an extender in the shared hourly window; NaN when stale or unavailable", nil, nil),
			prometheus.NewDesc("urnetwork_stats_disputes_24h", "Disputed transfer contracts in the shared hourly window; NaN when stale or unavailable", nil, nil),
		},
		windowEnd:    prometheus.NewDesc("urnetwork_stats_contract_hour_window_end_seconds", "Original hourly-window end and source-read start; cache hits never advance it", nil, nil),
		completed:    prometheus.NewDesc("urnetwork_stats_contract_hour_source_completed_seconds", "Original hourly-window source-read completion", nil, nil),
		availability: prometheus.NewDesc("urnetwork_stats_contract_hour_available", "One when the shared hourly-window counts are available and younger than five minutes", nil, nil),
	}
}

func (metrics *statsContractHourMetrics) Describe(ch chan<- *prometheus.Desc) {
	for _, desc := range metrics.counts {
		ch <- desc
	}
	ch <- metrics.windowEnd
	ch <- metrics.completed
	ch <- metrics.availability
}

func (metrics *statsContractHourMetrics) Collect(ch chan<- prometheus.Metric) {
	metrics.mu.Lock()
	snapshot, available := metrics.snapshot, metrics.available
	metrics.mu.Unlock()
	available = available && statsContractHourSnapshotFresh(&snapshot, metrics.now())
	for i, count := range []int64{snapshot.Counts.Contracts, snapshot.Counts.WithExtender, snapshot.Counts.Disputes} {
		value := math.NaN()
		if available {
			value = float64(count)
		}
		ch <- prometheus.MustNewConstMetric(metrics.counts[i], prometheus.GaugeValue, value)
	}
	windowEnd, completed, status := float64(0), float64(0), float64(0)
	if !snapshot.WindowEnd.IsZero() {
		windowEnd = statsSourceTimestampSeconds(snapshot.WindowEnd)
		completed = statsSourceTimestampSeconds(snapshot.CompletedAt)
	}
	if available {
		status = 1
	}
	ch <- prometheus.MustNewConstMetric(metrics.windowEnd, prometheus.GaugeValue, windowEnd)
	ch <- prometheus.MustNewConstMetric(metrics.completed, prometheus.GaugeValue, completed)
	ch <- prometheus.MustNewConstMetric(metrics.availability, prometheus.GaugeValue, status)
}

func (metrics *statsContractHourMetrics) publish(snapshot *statsContractHourSnapshot, available bool) {
	metrics.registerOnce.Do(func() { metrics.registerer.MustRegister(metrics) })
	metrics.mu.Lock()
	defer metrics.mu.Unlock()
	metrics.available = available && snapshot != nil
	if metrics.available {
		metrics.snapshot = *snapshot
	}
}

var statsContractHoursMetrics = newStatsContractHourMetrics(prometheus.DefaultRegisterer, time.Now)

func statsRefreshContractHourWindow(ctx context.Context) {
	snapshot, err := getStatsContractHourSnapshot(ctx)
	statsContractHoursMetrics.publish(snapshot, err == nil)
}
