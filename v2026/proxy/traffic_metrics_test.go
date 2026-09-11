package proxy

import (
	"io"
	"net"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
	proxylib "github.com/urnetwork/proxy/v2026"
)

type proxyMetricsSocksSource struct {
	stats  proxylib.SocksStatsSnapshot
	active int
}

func (self proxyMetricsSocksSource) Stats() proxylib.SocksStatsSnapshot { return self.stats }
func (self proxyMetricsSocksSource) ActiveCount() int                   { return self.active }

type proxyMetricsHttpSource struct {
	stats  proxylib.HttpStatsSnapshot
	active int
}

func (self proxyMetricsHttpSource) Stats() proxylib.HttpStatsSnapshot { return self.stats }
func (self proxyMetricsHttpSource) ActiveCount() int                  { return self.active }

func TestProxyIngressCollectorExportsCompleteBoundedLibrarySnapshots(t *testing.T) {
	registry := prometheus.NewPedanticRegistry()
	metrics := newProxyTrafficMetrics(registry)
	metrics.setIngressSources(
		proxyMetricsSocksSource{
			stats:  proxylib.SocksStatsSnapshot{ConnectDialErrors: 3, AssociateFlowsOpened: 5},
			active: 2,
		},
		proxyMetricsHttpSource{
			stats:  proxylib.HttpStatsSnapshot{ConnectClientsGone: 7, ResponsesAborted: 11},
			active: 4,
		},
	)
	families := gatherProxyMetricFamilies(t, registry)
	if got := len(families["urnetwork_proxy_ingress_events_total"].Metric); got != 18 {
		t.Fatalf("bounded ingress event series = %d, want 18", got)
	}
	if got := proxyMetricValue(t, families, "urnetwork_proxy_ingress_events_total", map[string]string{"protocol": "socks", "reason": "connect_dial_error"}); got != 3 {
		t.Fatalf("SOCKS connect dial errors = %v, want 3", got)
	}
	if got := proxyMetricValue(t, families, "urnetwork_proxy_ingress_events_total", map[string]string{"protocol": "http", "reason": "response_aborted"}); got != 11 {
		t.Fatalf("HTTP response aborts = %v, want 11", got)
	}
	if got := proxyMetricValue(t, families, "urnetwork_proxy_ingress_active", map[string]string{"protocol": "http"}); got != 4 {
		t.Fatalf("HTTP active = %v, want 4", got)
	}
	for _, metric := range families["urnetwork_proxy_ingress_events_total"].Metric {
		if len(metric.Label) != 2 {
			t.Fatalf("ingress event has %d labels, want only protocol/reason", len(metric.Label))
		}
	}
}

func TestProxyConnectionMetricsPreserveDirectionalBytesAndCloseOnce(t *testing.T) {
	registry := prometheus.NewPedanticRegistry()
	metrics := newProxyTrafficMetrics(registry)
	relayConn, destinationConn := net.Pipe()
	wrapped := metrics.instrumentConnection("http", relayConn)

	writeDone := make(chan error, 1)
	go func() {
		_, err := wrapped.Write([]byte("abc"))
		writeDone <- err
	}()
	clientPayload := make([]byte, 3)
	if _, err := io.ReadFull(destinationConn, clientPayload); err != nil {
		t.Fatal(err)
	}
	if err := <-writeDone; err != nil {
		t.Fatal(err)
	}

	readDone := make(chan error, 1)
	go func() {
		_, err := destinationConn.Write([]byte("reply"))
		readDone <- err
	}()
	destinationPayload := make([]byte, 5)
	if _, err := io.ReadFull(wrapped, destinationPayload); err != nil {
		t.Fatal(err)
	}
	if err := <-readDone; err != nil {
		t.Fatal(err)
	}

	if err := wrapped.Close(); err != nil {
		t.Fatal(err)
	}
	_ = wrapped.Close()
	_ = destinationConn.Close()

	if got := testutil.ToFloat64(metrics.bytes.WithLabelValues("http", "client_to_destination")); got != 3 {
		t.Fatalf("client-to-destination bytes = %v, want 3", got)
	}
	if got := testutil.ToFloat64(metrics.bytes.WithLabelValues("http", "destination_to_client")); got != 5 {
		t.Fatalf("destination-to-client bytes = %v, want 5", got)
	}
	if got := testutil.ToFloat64(metrics.sessions.WithLabelValues("http", "completed")); got != 1 {
		t.Fatalf("completed sessions = %v, want 1 after repeated Close", got)
	}
	if got := testutil.ToFloat64(metrics.sessionsActive.WithLabelValues("http")); got != 0 {
		t.Fatalf("active sessions after close = %v, want 0", got)
	}
}

func TestProxySessionMaximumResetsAtIntervalBoundary(t *testing.T) {
	metrics := newProxyTrafficMetrics(prometheus.NewPedanticRegistry())
	now := time.Unix(1_800_000_000, 0)
	metrics.now = func() time.Time { return now }
	metrics.observeSessionClose("socks", "completed", 4*time.Second)
	metrics.observeSessionClose("socks", "completed", time.Second)
	if got := metrics.sessionMaximums["socks"].seconds; got != 4 {
		t.Fatalf("same-interval maximum = %v, want 4", got)
	}
	now = now.Add(proxyMetricsMaxInterval)
	metrics.observeSessionClose("socks", "completed", 500*time.Millisecond)
	if got := metrics.sessionMaximums["socks"].seconds; got != 0.5 {
		t.Fatalf("next-interval maximum = %v, want 0.5", got)
	}
}

func TestWireGuardPacketMetricsCountPayloadAfterOffset(t *testing.T) {
	packetsBefore := testutil.ToFloat64(proxyWireGuardClientDeliveredPackets)
	bytesBefore := testutil.ToFloat64(proxyWireGuardClientDeliveredBytes)
	observeWireGuardPackets("client_to_destination", "delivered", [][]byte{{0, 0, 1, 2, 3}, {0, 0, 4}}, 2)
	if got := testutil.ToFloat64(proxyWireGuardClientDeliveredPackets) - packetsBefore; got != 2 {
		t.Fatalf("WireGuard packet delta = %v, want 2", got)
	}
	if got := testutil.ToFloat64(proxyWireGuardClientDeliveredBytes) - bytesBefore; got != 4 {
		t.Fatalf("WireGuard payload byte delta = %v, want 4", got)
	}
}

// gatherProxyMetricFamilies gathers one private registry by metric name.
func gatherProxyMetricFamilies(t *testing.T, registry prometheus.Gatherer) map[string]*dto.MetricFamily {
	t.Helper()
	familyList, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	families := map[string]*dto.MetricFamily{}
	for _, family := range familyList {
		families[family.GetName()] = family
	}
	return families
}

// proxyMetricValue returns one exact gauge or counter series.
func proxyMetricValue(t *testing.T, families map[string]*dto.MetricFamily, name string, labels map[string]string) float64 {
	t.Helper()
	family := families[name]
	if family == nil {
		t.Fatalf("metric family %q is absent", name)
	}
	for _, metric := range family.Metric {
		matched := true
		for name, value := range labels {
			found := false
			for _, label := range metric.Label {
				if label.GetName() == name && label.GetValue() == value {
					found = true
					break
				}
			}
			matched = matched && found
		}
		if matched {
			if metric.Gauge != nil {
				return metric.GetGauge().GetValue()
			}
			return metric.GetCounter().GetValue()
		}
	}
	t.Fatalf("metric family %q omitted labels %#v", name, labels)
	return 0
}
