package connect

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestResidentForwardWorkerOwnershipGaugeReturnsToBaseline(t *testing.T) {
	baseline := testutil.ToFloat64(residentForwardWorkersGauge)
	resident := &Resident{cancel: func() {}}
	started := make(chan struct{})
	release := make(chan struct{})
	if !resident.startForwardWorker(nil, func() {
		close(started)
		<-release
	}) {
		t.Fatal("start resident forward worker")
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("resident forward worker did not start")
	}
	if got := testutil.ToFloat64(residentForwardWorkersGauge); got != baseline+1 {
		t.Fatalf("resident forward worker gauge=%v, want %v while worker is live", got, baseline+1)
	}
	close(release)
	resident.forwardWorkers.Wait()
	if got := testutil.ToFloat64(residentForwardWorkersGauge); got != baseline {
		t.Fatalf("resident forward worker gauge=%v, want baseline %v after join", got, baseline)
	}
}

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
	registry.MustRegister(
		residentClientsGauge,
		residentLazyForwardIngressEnabledGauge,
		residentCallbackWorkersGauge,
		residentForwardWorkersGauge,
		residentForwardIdleWatchersGauge,
	)
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	if len(families) != 5 {
		t.Fatalf("runtime diagnostic metric families=%d, want 5", len(families))
	}
	for _, family := range families {
		switch family.GetName() {
		case "urnetwork_connect_resident_clients",
			"urnetwork_connect_resident_lazy_forward_ingress_enabled",
			"urnetwork_connect_resident_callback_workers",
			"urnetwork_connect_resident_forward_workers",
			"urnetwork_connect_resident_forward_idle_watchers":
		default:
			t.Fatalf("unexpected runtime diagnostic metric %q", family.GetName())
		}
		if len(family.GetMetric()) != 1 || len(family.GetMetric()[0].GetLabel()) != 0 || family.GetMetric()[0].GetGauge() == nil {
			t.Fatalf("runtime diagnostic metric %q exposes identities or is not one gauge", family.GetName())
		}
	}
}
