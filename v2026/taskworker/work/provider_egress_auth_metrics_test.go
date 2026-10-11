package work

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

func TestProviderEgressAuthMetricsHaveFixedCardinality(t *testing.T) {
	metrics := newProviderEgressAuthMetrics()
	registry := prometheus.NewPedanticRegistry()
	registry.MustRegister(metrics)
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	if len(families) != 1 || families[0].GetName() != "urnetwork_egress_probe_auth_requests_total" || len(families[0].Metric) != 36 {
		t.Fatal("auth observer did not export all and only its fixed cells")
	}
	phases := map[string]bool{"no_post": true, "pre_write": true, "response_wait": true, "body_read": true}
	results := map[string]bool{"ok": true, "timeout": true, "canceled": true, "http_auth": true, "http_rate": true, "http_error": true, "api_error": true, "decode_error": true, "error": true}
	for _, metric := range families[0].Metric {
		if len(metric.Label) != 2 || metric.Counter == nil || metric.Counter.GetValue() != 0 {
			t.Fatal("auth observation gained an identity label or wrong metric type")
		}
		for _, label := range metric.Label {
			if label.GetName() == "phase" && phases[label.GetValue()] || label.GetName() == "result" && results[label.GetValue()] {
				continue
			}
			t.Fatal("auth observation exported an unbounded label")
		}
	}
}
