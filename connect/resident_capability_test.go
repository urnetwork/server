package connect

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// The executable-owned capability lets production monitoring distinguish an
// intentional modified build containing the lazy resident fix from an older
// artifact when a base revision cannot establish the exact executable contents.
func TestResidentLazyForwardIngressCapabilityMetric(t *testing.T) {
	if got := testutil.ToFloat64(residentLazyForwardIngressEnabledGauge); got != 1 {
		t.Fatalf("resident lazy-forward-ingress capability = %v, want 1", got)
	}
}

func TestResidentRuntimeDiagnosticGaugesAreIdentityFree(t *testing.T) {
	registry := prometheus.NewRegistry()
	registry.MustRegister(residentClientsGauge, residentLazyForwardIngressEnabledGauge)
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	if len(families) != 2 {
		t.Fatalf("runtime diagnostic metric families=%d, want 2", len(families))
	}
	for _, family := range families {
		if family.GetName() != "urnetwork_connect_resident_clients" && family.GetName() != "urnetwork_connect_resident_lazy_forward_ingress_enabled" {
			t.Fatalf("unexpected runtime diagnostic metric %q", family.GetName())
		}
		if len(family.GetMetric()) != 1 || len(family.GetMetric()[0].GetLabel()) != 0 || family.GetMetric()[0].GetGauge() == nil {
			t.Fatalf("runtime diagnostic metric %q exposes identities or is not one gauge", family.GetName())
		}
	}
}
