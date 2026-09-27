// The telemetry bridge exports an exact bounded capability, not identities.
package work

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

func TestProviderEgressDnsMetricsHaveFixedCardinality(t *testing.T) {
	metrics := newProviderEgressDnsMetrics()
	registry := prometheus.NewPedanticRegistry()
	registry.MustRegister(metrics)
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	if len(families) != 1 || families[0].GetName() != "urnetwork_egress_probe_dns_waves_total" || len(families[0].Metric) != 45 {
		t.Fatal("DNS observer did not export all and only its fixed cells")
	}
	results := map[string]bool{"answer": true, "authoritative_empty": true, "timeout": true, "unanswered": true, "canceled": true}
	paths := map[string]bool{"unknown": true, "active": true, "forming": true, "platform_unreachable": true, "provider_unresponsive": true, "rate_limited": true, "auth_failing": true, "lost": true, "closed": true}
	for _, metric := range families[0].Metric {
		if len(metric.Label) != 2 || metric.Counter == nil || metric.Counter.GetValue() != 0 {
			t.Fatal("DNS observation gained an identity label or wrong metric type")
		}
		for _, label := range metric.Label {
			if label.GetName() == "result" && results[label.GetValue()] || label.GetName() == "path" && paths[label.GetValue()] {
				continue
			}
			t.Fatal("DNS observation exported an unbounded label")
		}
	}
}
