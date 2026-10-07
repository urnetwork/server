package controller

import (
	"math"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/urnetwork/server/model"
)

func testStatsContractHourValue(t testing.TB, metrics *statsContractHourMetrics, name string) float64 {
	t.Helper()
	registry := prometheus.NewRegistry()
	registry.MustRegister(metrics)
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() == name && len(family.Metric) == 1 {
			return family.Metric[0].GetGauge().GetValue()
		}
	}
	t.Fatalf("missing contract-hour metric %s", name)
	return 0
}

func TestStatsContractHourMetricsExpiryAndFailure(t *testing.T) {
	store := newStatsProviderEgressTestStore()
	metrics := newStatsContractHourMetrics(prometheus.NewRegistry(), store.now)
	now := store.now()
	snapshot := &statsContractHourSnapshot{model.ContractHourCounts{Contracts: 7, WithExtender: 3, Disputes: 1}, now, now.Add(time.Second)}
	metrics.publish(snapshot, true)
	if got := testStatsContractHourValue(t, metrics, "urnetwork_stats_contracts_24h"); got != 7 {
		t.Fatal("fresh counts unavailable")
	}
	metrics.publish(nil, false)
	if !math.IsNaN(testStatsContractHourValue(t, metrics, "urnetwork_stats_contracts_24h")) || testStatsContractHourValue(t, metrics, "urnetwork_stats_contract_hour_available") != 0 {
		t.Fatal("failure retained current counts")
	}
	if testStatsContractHourValue(t, metrics, "urnetwork_stats_contract_hour_window_end_seconds") != float64(now.Unix()) {
		t.Fatal("failed refresh renewed original source time")
	}
	metrics.publish(snapshot, true)
	store.advance(statsContractHourCacheTTL)
	if !math.IsNaN(testStatsContractHourValue(t, metrics, "urnetwork_stats_contracts_24h")) || testStatsContractHourValue(t, metrics, "urnetwork_stats_contract_hour_available") != 0 {
		t.Fatal("stalled refresh loop kept expired counts current")
	}
	now = store.now()
	metrics.publish(&statsContractHourSnapshot{model.ContractHourCounts{}, now, now}, true)
	if testStatsContractHourValue(t, metrics, "urnetwork_stats_contracts_24h") != 0 || testStatsContractHourValue(t, metrics, "urnetwork_stats_contract_hour_available") != 1 {
		t.Fatal("legitimate zero lost")
	}
}

func TestStatsContractHourMetricsScrapeOwnsGeneration(t *testing.T) {
	store := newStatsProviderEgressTestStore()
	metrics := newStatsContractHourMetrics(prometheus.NewRegistry(), store.now)
	now := store.now()
	first := &statsContractHourSnapshot{model.ContractHourCounts{Contracts: 7, WithExtender: 3, Disputes: 1}, now, now}
	metrics.publish(first, true)
	ch := make(chan prometheus.Metric)
	done := make(chan struct{})
	go func() { defer close(done); metrics.Collect(ch); close(ch) }()
	got := []float64{}
	for metric := range ch {
		if len(got) == 0 {
			metrics.publish(nil, false)
		}
		var wire dto.Metric
		if err := metric.Write(&wire); err != nil {
			t.Fatal(err)
		}
		got = append(got, wire.GetGauge().GetValue())
	}
	<-done
	want := []float64{7, 3, 1, float64(now.Unix()), float64(now.Unix()), 1}
	if len(got) != len(want) {
		t.Fatal("partial scrape")
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatal("scrape mixed publication generations")
		}
	}
}
