package work

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

// A cold process must publish the complete finite vocabulary, never an empty
// vector that a reader could mistake for an older uninstrumented executable.
func TestProviderEgressInitialPingFiniteMetrics(t *testing.T) {
	registry := prometheus.NewPedanticRegistry()
	registry.MustRegister(newProviderEgressInitialPingMetrics())
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	if len(families) != 5 {
		t.Fatalf("metric family count=%d", len(families))
	}
	series := 0
	for _, family := range families {
		if !strings.HasPrefix(family.GetName(), "urnetwork_egress_probe_initial_ping_") {
			t.Fatal("unexpected metric family")
		}
		started := strings.HasSuffix(family.GetName(), "_started_total")
		path := strings.Contains(family.GetName(), "_path_")
		want := 24
		if started {
			want = 1
		}
		if len(family.Metric) != want {
			t.Fatalf("family=%s cells=%d want=%d", family.GetName(), len(family.Metric), want)
		}
		seen := map[string]bool{}
		for _, metric := range family.Metric {
			if metric.Counter == nil || metric.Counter.GetValue() != 0 {
				t.Fatal("cold diagnostic counter is not zero")
			}
			labels := map[string]string{}
			for _, label := range metric.Label {
				labels[label.GetName()] = label.GetValue()
			}
			if started {
				if len(labels) != 0 {
					t.Fatal("started counter has identity labels")
				}
			} else if path {
				if len(labels) != 3 || !validInitialPingMetricValue(labels["outcome"], "acknowledged", "expired", "error", "canceled_or_ended") ||
					!validInitialPingMetricValue(labels["route_write"], "not_observed", "accepted") || !validInitialPingMetricValue(labels["ack_callback"], "pending", "success", "error") {
					t.Fatal("unexpected path diagnostic label schema/vocabulary")
				}
				key := labels["outcome"] + "/" + labels["route_write"] + "/" + labels["ack_callback"]
				if seen[key] {
					t.Fatal("duplicate path diagnostic cell")
				}
				seen[key] = true
			} else {
				if len(labels) != 2 || labels["outcome"] == "" || labels["dependency"] == "" {
					t.Fatal("unexpected diagnostic label schema")
				}
				key := labels["outcome"] + "/" + labels["dependency"]
				if seen[key] {
					t.Fatal("duplicate diagnostic cell")
				}
				seen[key] = true
			}
			series++
		}
	}
	if series != 97 {
		t.Fatalf("series cardinality=%d", series)
	}
}

func validInitialPingMetricValue(value string, allowed ...string) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}
