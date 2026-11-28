package server

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/connect/v2026"
)

// The collector reports every reason connect counts, under the name the connect
// dashboard reads, and passes a pedantic registry. The exit's detail line went
// to V(1) (CPUOPT6 A #2(b)), so this count is what remains at the default
// verbosity.
func TestSendSequenceExitCollectorReportsConnectCounts(t *testing.T) {
	registry := prometheus.NewPedanticRegistry()
	if err := registry.Register(newSendSequenceExitCollector()); err != nil {
		t.Fatal(err)
	}
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	if len(families) != 1 || families[0].GetName() != "urnetwork_connect_send_sequence_exits_total" {
		t.Fatalf("families = %v", families)
	}
	counts := connect.GetSendSequenceExitCounts()
	reasons := map[string]bool{}
	for _, metric := range families[0].GetMetric() {
		labels := metric.GetLabel()
		if len(labels) != 1 || labels[0].GetName() != "reason" {
			t.Fatalf("labels = %v", labels)
		}
		reasons[labels[0].GetValue()] = true
	}
	if len(reasons) != len(counts) {
		t.Fatalf("reasons = %v, want every counted reason %v", reasons, counts)
	}
	for _, reason := range []string{"ack_lifetime", "context", "contract_creation", "other"} {
		if !reasons[reason] {
			t.Fatalf("reason %s is not reported: %v", reason, reasons)
		}
	}
	if reasons["idle"] {
		t.Fatal("idle ends are not counted")
	}
}
