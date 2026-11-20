package nginx

// Regression test: the front's and the forwarder's metrics push under the
// stats push's grouping (server.StatsPushReservedLabelNames). With service
// and block labels, the front's request counter made the push client refuse
// every push of the connect process from its first requests on, so all of the
// process's metrics stopped.

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/push"

	"github.com/urnetwork/server"
)

// Every front and forwarder metric family, each with a series, pushes as a
// pushing process pushes them. Before upstream_service and upstream_block,
// the push client refused this push: "already contains grouping label".
func TestFrontMetricsPushUnderStatsPushGrouping(t *testing.T) {
	registry := prometheus.NewRegistry()

	frontMetrics := newFrontMetrics(registry)
	frontMetrics.requests.WithLabelValues("api", "h2", "2xx", "g1").Inc()
	frontMetrics.responseHeaderSeconds.WithLabelValues("api", "h2", "2xx", "g1").Observe(0.01)
	frontMetrics.limitRefusals.WithLabelValues("request", limitDefaultZone).Inc()
	frontMetrics.certificateExpiry.WithLabelValues().Set(1)
	frontMetrics.quicClientHellos.WithLabelValues(tlsQuicPortWhodis, tlsQuicNameClassAlt, "true").Inc()

	// the forwarder's metrics are the process's, in the default registry. This
	// registry gathers them too, with one synthetic series each, removed after
	registry.MustRegister(
		upstreamAttemptsCounter,
		upstreamRetriesCounter,
		forwardErrorsCounter,
		forwardBodyRefusalsCounter,
		forwardTunnelsGauge,
		bodyMemoryBytesGauge,
		bodySpillBytesGauge,
		bodySpillFilesGauge,
	)
	serviceName := "push-labels-test"
	upstreamAttemptsCounter.WithLabelValues(serviceName, "g1", upstreamAttemptResponse).Inc()
	upstreamRetriesCounter.WithLabelValues(serviceName, upstreamAttemptHttp503).Inc()
	forwardErrorsCounter.WithLabelValues(serviceName, "502").Inc()
	forwardBodyRefusalsCounter.WithLabelValues(serviceName, "canceled").Inc()
	forwardTunnelsGauge.WithLabelValues(serviceName).Inc()
	defer func() {
		upstreamAttemptsCounter.DeleteLabelValues(serviceName, "g1", upstreamAttemptResponse)
		upstreamRetriesCounter.DeleteLabelValues(serviceName, upstreamAttemptHttp503)
		forwardErrorsCounter.DeleteLabelValues(serviceName, "502")
		forwardBodyRefusalsCounter.DeleteLabelValues(serviceName, "canceled")
		forwardTunnelsGauge.DeleteLabelValues(serviceName)
	}()

	pushServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))
	defer pushServer.Close()

	// the job is the push client's own label; every other reserved name groups
	pusher := push.New(pushServer.URL, "connect").Gatherer(registry)
	for _, labelName := range server.StatsPushReservedLabelNames() {
		if labelName != "job" {
			pusher = pusher.Grouping(labelName, "test")
		}
	}
	if err := pusher.Push(); err != nil {
		t.Fatal(err)
	}
}
