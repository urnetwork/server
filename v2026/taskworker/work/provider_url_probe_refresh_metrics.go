package work

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

type urlProbeFleetRefreshResult uint8

const (
	urlProbeFleetRefreshSuccess urlProbeFleetRefreshResult = iota
	urlProbeFleetRefreshDeadline
	urlProbeFleetRefreshCanceled
	urlProbeFleetRefreshReadError
	urlProbeFleetRefreshResultCount
)

var urlProbeFleetRefreshResultLabels = [urlProbeFleetRefreshResultCount]string{
	"success", "context_deadline", "context_canceled", "read_error",
}

// Refresh observations are separate from the atomic fifteen-series census.
// An idle capable producer exposes zero counts without inventing a census.
type providerUrlProbeFleetRefreshCollectors struct {
	results *prometheus.CounterVec
	seconds *prometheus.CounterVec
	enabled prometheus.Gauge
}

func newProviderUrlProbeFleetRefreshCollectors(registerer prometheus.Registerer) *providerUrlProbeFleetRefreshCollectors {
	metrics := &providerUrlProbeFleetRefreshCollectors{
		results: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "urnetwork_url_probe_fleet_refresh_total",
			Help: "Actual URL fleet census refresh attempts by fixed result; guard skips are not attempts and failures retain the prior census generation",
		}, []string{"result"}),
		seconds: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "urnetwork_url_probe_fleet_refresh_seconds_total",
			Help: "Cumulative monotonic wall time of actual URL fleet census refresh attempts by fixed result",
		}, []string{"result"}),
		enabled: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "urnetwork_url_probe_fleet_refresh_observation_enabled",
			Help: "Executable-owned capability for identity-free URL fleet refresh result and duration observations",
		}),
	}
	for _, result := range urlProbeFleetRefreshResultLabels {
		metrics.results.WithLabelValues(result)
		metrics.seconds.WithLabelValues(result)
	}
	metrics.enabled.Set(1)
	registerer.MustRegister(metrics.results, metrics.seconds, metrics.enabled)
	return metrics
}

var urlProbeFleetRefreshMetrics = newProviderUrlProbeFleetRefreshCollectors(prometheus.DefaultRegisterer)

// Classify only the refresh's typed context state after a failed result. A
// successful published generation remains success if cancellation races return.
// Arbitrary read/panic/error text never becomes a metric label.
func (self *providerUrlProbeFleetRefreshCollectors) observe(ctx context.Context, refreshErr error, elapsed time.Duration) {
	if self == nil {
		return
	}
	result := urlProbeFleetRefreshSuccess
	if refreshErr != nil {
		result = urlProbeFleetRefreshReadError
		switch ctx.Err() {
		case context.DeadlineExceeded:
			result = urlProbeFleetRefreshDeadline
		case context.Canceled:
			result = urlProbeFleetRefreshCanceled
		}
	}
	label := urlProbeFleetRefreshResultLabels[result]
	self.results.WithLabelValues(label).Inc()
	self.seconds.WithLabelValues(label).Add(elapsed.Seconds())
}
