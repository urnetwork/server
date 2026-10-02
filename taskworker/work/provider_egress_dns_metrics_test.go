package work

import (
	"github.com/prometheus/client_golang/prometheus"
	"testing"
)

// Scraping exports only fixed diagnostic vocabularies, including zero cells.
func TestProviderEgressDnsMetricsHaveFixedCardinality(t *testing.T) {
	metrics := newProviderEgressDnsMetrics()
	registry := prometheus.NewPedanticRegistry()
	registry.MustRegister(metrics)
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	expected := map[string]int{"urnetwork_egress_probe_dns_waves_total": 45, "urnetwork_egress_probe_dns_route_waves_total": 35, "urnetwork_egress_probe_dns_route_seconds_total": 105, "urnetwork_egress_probe_dns_contract_waves_total": 15, "urnetwork_egress_probe_dns_contract_wave_seconds_total": 15}
	results := map[string]bool{"answer": true, "authoritative_empty": true, "timeout": true, "unanswered": true, "canceled": true}
	paths := map[string]bool{"unknown": true, "active": true, "forming": true, "platform_unreachable": true, "provider_unresponsive": true, "rate_limited": true, "auth_failing": true, "lost": true, "closed": true}
	routes := map[string]bool{"unknown": true, "stable_route": true, "current_route_admitted": true, "unready_endpoints": true, "changed_or_ambiguous": true, "lost": true, "closed": true}
	phases := map[string]bool{"before_current_admission": true, "after_current_admission": true, "unattributed": true}
	if len(families) != len(expected) {
		t.Fatal("unexpected metric family")
	}
	for _, family := range families {
		if expected[family.GetName()] != len(family.Metric) {
			t.Fatal("diagnostic cardinality changed")
		}
		labels := 2
		if family.GetName() == "urnetwork_egress_probe_dns_route_seconds_total" {
			labels = 3
		}
		for _, metric := range family.Metric {
			if len(metric.Label) != labels || metric.Counter == nil || metric.Counter.GetValue() != 0 {
				t.Fatal("diagnostic type or initial value changed")
			}
			for _, label := range metric.Label {
				allowed := false
				switch label.GetName() {
				case "result":
					allowed = results[label.GetValue()]
				case "path":
					allowed = paths[label.GetValue()]
				case "route":
					allowed = routes[label.GetValue()]
				case "phase":
					allowed = phases[label.GetValue()]
				case "evidence":
					allowed = label.GetValue() == "unobserved" || label.GetValue() == "local_contract_no_provider_write" || label.GetValue() == "not_proved"
				}
				if !allowed {
					t.Fatal("identity or unbounded label escaped diagnostic")
				}
			}
		}
	}
}
