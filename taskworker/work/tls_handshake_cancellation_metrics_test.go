// Scrape contracts for per-process handshake cancellation evidence.
package work

import (
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
)

// The production collector must be reachable from the registry used by the
// Taskworker stats pusher, including on a process with no cancellation events.
func TestTlsHandshakeCancellationDefaultRegistry(t *testing.T) {
	candidate := newTlsHandshakeCancellationMetrics(connect.GetTlsHandshakeCancellationCounts)
	var registered prometheus.AlreadyRegisteredError
	if err := prometheus.Register(candidate); !errors.As(err, &registered) {
		t.Fatal("missing Taskworker default-registry collector")
	}
	if _, ok := registered.ExistingCollector.(*tlsHandshakeCancellationMetrics); !ok {
		t.Fatal("another default collector owns the cancellation counter")
	}
}

// Gather from a fresh registry with a private snapshot so two scrapes exercise
// cumulative changes and reserved zeros independent of other in-process tests.
func TestTlsHandshakeCancellationCounterCohorts(t *testing.T) {
	snapshot := connect.GetTlsHandshakeCancellationCounts()
	for i := range snapshot {
		snapshot[i].Count = 0
	}
	registry := prometheus.NewPedanticRegistry()
	registry.MustRegister(newTlsHandshakeCancellationMetrics(func() []connect.TlsHandshakeCancellationCount { return snapshot }))
	sources := []string{"parent_lifetime", "manager_retired", "session_retired", "control_enqueue_rejected", "epoch_retired", "worker_recovery", "unknown"}
	for _, value := range []uint64{0, 3, 7} {
		for i, cell := range snapshot {
			if cell.Source == "control_enqueue_rejected" && cell.Role == "server" && cell.Companion {
				snapshot[i].Count = value
			}
		}
		families, err := registry.Gather()
		if err != nil || len(families) != 1 || families[0].GetType() != dto.MetricType_COUNTER ||
			families[0].GetName() != "urnetwork_taskworker_tls_handshake_cancellations_total" || len(families[0].Metric) != 28 {
			t.Fatal("incomplete handshake cancellation counter family")
		}
		values := map[[3]string]float64{}
		for _, metric := range families[0].Metric {
			labels := map[string]string{}
			for _, pair := range metric.Label {
				labels[pair.GetName()] = pair.GetValue()
			}
			if len(labels) != 3 {
				t.Fatal("unexpected label appeared on handshake cancellation counter")
			}
			for _, reserved := range server.StatsPushReservedLabelNames() {
				if _, present := labels[reserved]; present {
					t.Fatal("cancellation counter conflicts with stats push grouping")
				}
			}
			values[[3]string{labels["source"], labels["role"], labels["companion"]}] = metric.GetCounter().GetValue()
		}
		for _, source := range sources {
			for _, role := range []string{"client", "server"} {
				for _, companion := range []string{"false", "true"} {
					want := float64(0)
					if source == "control_enqueue_rejected" && role == "server" && companion == "true" {
						want = float64(value)
					}
					if got, present := values[[3]string{source, role, companion}]; !present || got != want {
						t.Fatalf("wrong count for source %s role %s companion %s", source, role, companion)
					}
				}
			}
		}
	}
}
