package handlers

// Fixed-cardinality due-handler observations count actual selected-return
// events, not successful response delivery, executed probes, or durable writes.

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/urnetwork/server/v2026/model"
)

// Safe for concurrent handler calls; all label combinations exist even at idle.
type providerEgressDueCollectors struct {
	selected *prometheus.CounterVec
	requests prometheus.Counter
	enabled  prometheus.Gauge
}

var providerEgressDueLaneLabels = [model.ProviderEgressDueLaneCount]string{
	"no-location", "stale-location", "stale-health", "missing-health",
}

var providerEgressDueMetrics = newProviderEgressDueCollectors(prometheus.DefaultRegisterer)

// A separate capability and request count distinguish idle code from an absent
// exporter; neither is a substitute for selected-return events.
func newProviderEgressDueCollectors(registerer prometheus.Registerer) *providerEgressDueCollectors {
	collectors := &providerEgressDueCollectors{
		selected: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "urnetwork_egress_due_selected_total",
			Help: "Actual due-handler selected-return events by owning earliest-deadline lane and expiry at selection; not receipt or persistence acknowledgments",
		}, []string{"lane", "expired"}),
		requests: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "urnetwork_egress_due_requests_total",
			Help: "Due-handler requests whose model selection returned, including empty selections",
		}),
		enabled: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "urnetwork_egress_due_observation_enabled",
			Help: "Executable-owned capability for identity-free due selected-return observations",
		}),
	}
	for _, lane := range providerEgressDueLaneLabels {
		for _, expired := range []string{"false", "true"} {
			collectors.selected.WithLabelValues(lane, expired)
		}
	}
	collectors.enabled.Set(1)
	registerer.MustRegister(collectors.selected, collectors.requests, collectors.enabled)
	return collectors
}

// Counts only diagnostics from the completed selection, before response encoding.
func (self *providerEgressDueCollectors) observe(diagnostics model.ProviderEgressDueDiagnostics) {
	self.requests.Inc()
	for lane, counts := range diagnostics.Selected {
		self.selected.WithLabelValues(providerEgressDueLaneLabels[lane], "false").Add(float64(counts.Current))
		self.selected.WithLabelValues(providerEgressDueLaneLabels[lane], "true").Add(float64(counts.Expired))
	}
}
