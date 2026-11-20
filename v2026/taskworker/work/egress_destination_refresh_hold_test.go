// What the refresh reports when it holds a candidate back: the result's Held
// count, and the refresh stage of urnetwork_egress_site_catalog_refusals_total.
package work

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/controller"
)

// The value of one labeled counter series in the default registry, 0 when
// it is absent.
func testCounterValue(t testing.TB, name string, labels map[string]string) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.GetMetric() {
			matched := 0
			for _, label := range metric.GetLabel() {
				if value, ok := labels[label.GetName()]; ok && value == label.GetValue() {
					matched++
				}
			}
			if matched == len(labels) {
				return metric.GetCounter().GetValue()
			}
		}
	}
	return 0
}

func TestRefreshEgressDestinationsCountsAHeldCandidate(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		labels := map[string]string{"stage": controller.ProviderEgressRefusalStageRefresh, "reason": controller.ProviderEgressRefusalCountryConflict}
		before := testCounterValue(t, "urnetwork_egress_site_catalog_refusals_total", labels)
		result, _ := testRefreshWithACollidingCandidate(t, context.Background())
		if result.Held != 1 {
			t.Fatalf("refresh result = %+v, want one candidate held", result)
		}
		if got := testCounterValue(t, "urnetwork_egress_site_catalog_refusals_total", labels) - before; got != 1 {
			t.Fatalf("refresh-stage refusals counted %v, want 1", got)
		}
	})
}
