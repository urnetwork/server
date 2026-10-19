// Metric evidence must retain exact/capped/unavailable boundaries across
// refreshes and concurrent scrapes without using process-global test hooks.
package controller

import (
	"math"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/urnetwork/server/v2026/model"
)

// A separate registry inspects any owned collector without mutating it.
func testStatsOpenContractValue(t testing.TB, metrics *statsOpenContractMetrics, name string, labels map[string]string) float64 {
	t.Helper()
	registry := prometheus.NewRegistry()
	registry.MustRegister(metrics)
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.Metric {
			if len(metric.Label) != len(labels) {
				continue
			}
			match := true
			for _, label := range metric.Label {
				match = match && labels[label.GetName()] == label.GetValue()
			}
			if match {
				return metric.GetGauge().GetValue()
			}
		}
	}
	t.Fatalf("missing bounded contract metric %s labels=%v", name, labels)
	return 0
}

// Replacing a good sample by a cap or failed read must not keep pushing its
// old exact value. A capped extender zero remains unknown, not exact zero.
func TestStatsOpenContractsPublishesHonestTransitions(t *testing.T) {
	metrics := newStatsOpenContractMetrics(prometheus.NewRegistry())
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	exact := model.OpenContractStatsSnapshot{OpenContracts: 3, OpenContractsWithExtender: 2, OpenDisputes: 1,
		OpenContractsExact: true, OpenDisputesExact: true, ObservedAt: at}
	metrics.publish(exact, true)
	if value := testStatsOpenContractValue(t, metrics, "urnetwork_stats_open_contracts", nil); value != 3 {
		t.Fatalf("exact=%v", value)
	}
	capped := model.OpenContractStatsSnapshot{OpenContracts: 1001, OpenContractsWithExtender: 0, OpenDisputes: 0,
		OpenDisputesExact: true, ObservedAt: at.Add(time.Minute)}
	metrics.publish(capped, true)
	for _, name := range []string{"urnetwork_stats_open_contracts", "urnetwork_stats_open_contracts_with_extender"} {
		if !math.IsNaN(testStatsOpenContractValue(t, metrics, name, nil)) {
			t.Fatalf("%s published a capped count as exact", name)
		}
	}
	if testStatsOpenContractValue(t, metrics, "urnetwork_stats_contract_open_lower_bound", map[string]string{"kind": "open"}) != 1001 ||
		testStatsOpenContractValue(t, metrics, "urnetwork_stats_contract_open_lower_bound", map[string]string{"kind": "with_extender"}) != 0 ||
		testStatsOpenContractValue(t, metrics, "urnetwork_stats_contract_open_status", map[string]string{"kind": "with_extender", "status": "capped"}) != 1 {
		t.Fatal("capped lower bound lost its independent status")
	}
	if testStatsOpenContractValue(t, metrics, "urnetwork_stats_open_disputes", nil) != 0 {
		t.Fatal("exact dispute zero was hidden by an unrelated open-set cap")
	}
	metrics.publish(model.OpenContractStatsSnapshot{}, false)
	for _, name := range []string{"urnetwork_stats_open_contracts", "urnetwork_stats_open_contracts_with_extender", "urnetwork_stats_open_disputes"} {
		if !math.IsNaN(testStatsOpenContractValue(t, metrics, name, nil)) {
			t.Fatalf("unavailable exact gauge %s retained a value", name)
		}
	}
	for _, kind := range []string{"open", "with_extender", "dispute"} {
		if !math.IsNaN(testStatsOpenContractValue(t, metrics, "urnetwork_stats_contract_open_lower_bound", map[string]string{"kind": kind})) ||
			testStatsOpenContractValue(t, metrics, "urnetwork_stats_contract_open_observed_at_seconds", map[string]string{"kind": kind}) != 0 ||
			testStatsOpenContractValue(t, metrics, "urnetwork_stats_contract_open_status", map[string]string{"kind": kind, "status": "unavailable"}) != 1 {
			t.Fatalf("unavailable %s retained a healthy value or timestamp", kind)
		}
	}
	metrics.publish(exact, true)
	if testStatsOpenContractValue(t, metrics, "urnetwork_stats_open_contracts", nil) != 3 ||
		testStatsOpenContractValue(t, metrics, "urnetwork_stats_contract_open_observed_at_seconds", map[string]string{"kind": "open"}) != float64(at.Unix()) {
		t.Fatal("exact recovery lost its source time")
	}
}

// The first emitted metric is the barrier: a replacement before the rest of
// that scrape arrives must not mix generations in its count/status fields.
func TestStatsOpenContractsScrapeOwnsOneSnapshot(t *testing.T) {
	metrics := newStatsOpenContractMetrics(prometheus.NewRegistry())
	metrics.publish(model.OpenContractStatsSnapshot{OpenContracts: 3, OpenContractsWithExtender: 2, OpenDisputes: 1,
		OpenContractsExact: true, OpenDisputesExact: true, ObservedAt: time.Unix(1000, 0)}, true)
	values := make(chan prometheus.Metric)
	go func() { metrics.Collect(values); close(values) }()
	first := <-values
	metrics.publish(model.OpenContractStatsSnapshot{}, false)
	all := []prometheus.Metric{first}
	for value := range values {
		all = append(all, value)
	}
	if len(all) != 18 {
		t.Fatalf("snapshot metric count=%d", len(all))
	}
	for _, value := range all {
		metric := &dto.Metric{}
		if err := value.Write(metric); err != nil {
			t.Fatal(err)
		}
		if math.IsNaN(metric.GetGauge().GetValue()) {
			t.Fatal("a concurrent unavailable snapshot contaminated an exact scrape")
		}
		for _, label := range metric.Label {
			if label.GetName() == "status" && label.GetValue() == "exact" && metric.GetGauge().GetValue() != 1 {
				t.Fatal("exact status changed within one scrape")
			}
		}
	}
}
