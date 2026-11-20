package connect

// Regression guard: connect's front metrics push under the stats push's
// grouping (server.StatsPushReservedLabelNames), as the nginx front's must
// (nginx/metrics_push_labels_test.go). One metric of the connect process
// with a label the push reserves stops every metric of the process.

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/push"

	"github.com/urnetwork/server"
)

// Every connect front metric family, each with a synthetic series, pushes as
// the connect process pushes it.
func TestFrontMetricsPushUnderStatsPushGrouping(t *testing.T) {
	registry := prometheus.NewRegistry()
	// the metrics are the process's, in the default registry. This registry
	// gathers them too, with one synthetic series each, removed after
	registry.MustRegister(
		frontSocketsUpGauge,
		frontSocketRestartsCounter,
		frontSocketBindFailuresCounter,
		frontQuicConnectionsCounter,
		frontQuicHandshakesCounter,
		frontHttpServerErrorsCounter,
		frontTlsHandshakesCounter,
		frontTlsHandshakeOffersCounter,
		frontQuicConnectionsByFamilyCounter,
	)
	// a synthetic value in every series, so the deletes after can't touch a
	// series another test counts in
	interfaceName := "push-labels-test"
	frontSocketsUpGauge.WithLabelValues(interfaceName, "ipv4", "tcp", "443").Set(4)
	frontSocketRestartsCounter.WithLabelValues(interfaceName, "ipv4", "udp", "443").Inc()
	frontSocketBindFailuresCounter.WithLabelValues(interfaceName, "ipv4", "udp", "443").Inc()
	frontQuicConnectionsCounter.WithLabelValues(interfaceName, frontQuicResultHandshakeFailed, frontQuicHandshakeReasonIdleTimeout).Inc()
	frontQuicHandshakesCounter.WithLabelValues(interfaceName, frontQuicVersionV1, "true").Inc()
	frontQuicConnectionsByFamilyCounter.WithLabelValues(interfaceName, frontFamilyIpv6, frontQuicResultConnect).Inc()
	frontHttpServerErrorsCounter.WithLabelValues(interfaceName, frontHttpErrorTlsHandshake, frontTlsHandshakeReasonClosed).Inc()
	frontTlsHandshakesCounter.WithLabelValues(frontTlsVersionTls13, "true", interfaceName).Inc()
	frontTlsHandshakeOffersCounter.WithLabelValues(frontTlsAlpnNone, "false", interfaceName, "none", "legacy").Inc()
	defer func() {
		frontSocketsUpGauge.DeleteLabelValues(interfaceName, "ipv4", "tcp", "443")
		frontSocketRestartsCounter.DeleteLabelValues(interfaceName, "ipv4", "udp", "443")
		frontSocketBindFailuresCounter.DeleteLabelValues(interfaceName, "ipv4", "udp", "443")
		frontQuicConnectionsCounter.DeleteLabelValues(interfaceName, frontQuicResultHandshakeFailed, frontQuicHandshakeReasonIdleTimeout)
		frontQuicHandshakesCounter.DeleteLabelValues(interfaceName, frontQuicVersionV1, "true")
		frontQuicConnectionsByFamilyCounter.DeleteLabelValues(interfaceName, frontFamilyIpv6, frontQuicResultConnect)
		frontHttpServerErrorsCounter.DeleteLabelValues(interfaceName, frontHttpErrorTlsHandshake, frontTlsHandshakeReasonClosed)
		frontTlsHandshakesCounter.DeleteLabelValues(frontTlsVersionTls13, "true", interfaceName)
		frontTlsHandshakeOffersCounter.DeleteLabelValues(frontTlsAlpnNone, "false", interfaceName, "none", "legacy")
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
