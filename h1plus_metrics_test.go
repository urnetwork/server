package server

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/urnetwork/connect"
)

func TestH1PlusCollectorUsesFixedLabelsAndCounters(t *testing.T) {
	stats := &connect.H1PlusStats{}
	connect.RecordH1PlusSelection(stats, time.Millisecond, nil)
	connect.RecordH1PlusSelection(stats, time.Millisecond, &connect.HTTPUpgradeError{StatusCode: 401, Reason: "authorization", Terminal: true})
	connect.RecordH1PlusSelection(stats, time.Millisecond, &connect.HTTPUpgradeError{StatusCode: 426, Reason: "rejected"})
	registry := prometheus.NewRegistry()
	registry.MustRegister(NewH1PlusCollector("test", connect.H1FramerXlProtocol, stats))
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]float64{}
	for _, family := range families {
		if len(family.Metric) != 1 {
			t.Fatalf("unbounded metric cardinality: %s", family.GetName())
		}
		metric := family.Metric[0]
		if len(metric.Label) != 1 || metric.Label[0].GetName() != "protocol" || metric.Label[0].GetValue() != connect.H1FramerXlProtocol {
			t.Fatalf("unexpected labels: %v", metric.Label)
		}
		values[family.GetName()] = metric.GetCounter().GetValue()
	}
	for name, want := range map[string]float64{"attempts": 3, "accepted": 1, "fallback_rejected": 1, "auth_failures": 1, "handshake_nanoseconds": 3000000} {
		if got := values["urnetwork_test_h1plus_"+name+"_total"]; got != want {
			t.Errorf("%s=%v want%v", name, got, want)
		}
	}
}
