package server

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/connect"
)

func TestMessagePoolMetricsTrackOneLibrarySnapshot(t *testing.T) {
	wantNames := []string{
		"urnetwork_message_pool_taken_total",
		"urnetwork_message_pool_returned_total",
		"urnetwork_message_pool_created_total",
		"urnetwork_message_pool_unpooled_taken_total",
		"urnetwork_message_pool_unpooled_bytes_total",
		"urnetwork_message_pool_outstanding",
		"urnetwork_message_pool_retained",
		"urnetwork_message_pool_retained_bytes",
		"urnetwork_message_pool_capacity_bytes",
		"urnetwork_message_pool_packet_retained_bytes",
		"urnetwork_message_pool_large_object_retained_bytes",
		"urnetwork_message_pool_device_tun_egress_outstanding",
		"urnetwork_message_pool_device_tun_egress_outstanding_bytes",
	}

	buffer := connect.MessagePoolGet(1024)
	stats := connect.GetMessagePoolAggregateStats()
	unpooledTaken, unpooledBytes := connect.MessagePoolUnpooledCounts()
	registry := prometheus.NewPedanticRegistry()
	registry.MustRegister(newMessagePoolCollector())
	metrics := gatherMessagePoolMetrics(t, registry)

	want := map[string]float64{
		"urnetwork_message_pool_taken_total":                         float64(stats.Taken),
		"urnetwork_message_pool_returned_total":                      float64(stats.Returned),
		"urnetwork_message_pool_created_total":                       float64(stats.Created),
		"urnetwork_message_pool_unpooled_taken_total":                float64(unpooledTaken),
		"urnetwork_message_pool_unpooled_bytes_total":                float64(unpooledBytes),
		"urnetwork_message_pool_outstanding":                         float64(stats.Taken - stats.Returned),
		"urnetwork_message_pool_retained":                            float64(stats.RetainedCount),
		"urnetwork_message_pool_retained_bytes":                      float64(stats.RetainedByteCount),
		"urnetwork_message_pool_capacity_bytes":                      float64(stats.CapacityByteCount),
		"urnetwork_message_pool_packet_retained_bytes":               float64(stats.PacketRetainedByteCount),
		"urnetwork_message_pool_large_object_retained_bytes":         float64(stats.LargeObjectRetainedByteCount),
		"urnetwork_message_pool_device_tun_egress_outstanding":       float64(stats.DeviceTunEgressOutstandingCount),
		"urnetwork_message_pool_device_tun_egress_outstanding_bytes": float64(stats.DeviceTunEgressOutstandingByteCount),
	}
	for _, name := range wantNames {
		if got, ok := metrics[name]; !ok {
			t.Errorf("metric %s is not collected", name)
		} else if got != want[name] {
			t.Errorf("metric %s = %v, want %v", name, got, want[name])
		}
	}

	connect.MessagePoolReturn(buffer)
	registered := gatherMessagePoolMetrics(t, prometheus.DefaultGatherer)
	if _, ok := registered["urnetwork_message_pool_capacity_bytes"]; !ok {
		t.Fatal("root message-pool collector is not registered with the process registry")
	}
}

func gatherMessagePoolMetrics(t testing.TB, gatherer prometheus.Gatherer) map[string]float64 {
	t.Helper()
	families, err := gatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]float64{}
	for _, family := range families {
		if len(family.Metric) != 1 {
			continue
		}
		metric := family.Metric[0]
		if metric.Counter != nil {
			values[family.GetName()] = metric.Counter.GetValue()
		} else if metric.Gauge != nil {
			values[family.GetName()] = metric.Gauge.GetValue()
		}
	}
	return values
}
