package connect

// These tests pin the bounded-label Prometheus view of the shared H3 DATAGRAM
// carrier counters without starting a database-backed ConnectHandler.

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	clientconnect "github.com/urnetwork/connect/v2026"
)

// One production-shaped, single-DATAGRAM round trip must appear as monotonic
// message, fragment, and byte counters under the documented metric families.
// An oversized frame remains on the reliable hybrid stream and must not be
// misreported as a failed DATAGRAM attempt.
func TestConnectH3DatagramCollectorExportsCarrierStats(t *testing.T) {
	settings := clientconnect.DefaultH3DatagramSettings()
	stats := &clientconnect.H3DatagramStats{}
	fragmenter, err := clientconnect.NewH3DatagramFragmenter(settings, stats)
	if err != nil {
		t.Fatal(err)
	}
	message := bytes.Repeat([]byte("metric"), 70)
	var datagrams [][]byte
	_, err = fragmenter.Send(
		message,
		clientconnect.H3DatagramHeaderByteCount+512,
		func(datagram []byte) error {
			datagrams = append(datagrams, bytes.Clone(datagram))
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	useStream, _, err := fragmenter.SendHybrid(
		bytes.Repeat([]byte("stream"), 300),
		clientconnect.H3DatagramHeaderByteCount+512,
		func([]byte) error {
			t.Fatal("oversized hybrid frame attempted the DATAGRAM lane")
			return nil
		},
	)
	if err != nil || !useStream {
		t.Fatalf("oversized hybrid disposition = stream:%t err:%v", useStream, err)
	}
	budget := clientconnect.NewH3DatagramReassemblyBudget(settings.ProcessReassemblyByteCount)
	reassembler, err := clientconnect.NewH3DatagramReassembler(settings, budget, stats)
	if err != nil {
		t.Fatal(err)
	}
	var received []byte
	for _, datagram := range datagrams {
		if complete := reassembler.Accept(datagram, time.Unix(100, 0)); complete != nil {
			received = complete
		}
	}
	if !bytes.Equal(received, message) {
		t.Fatalf("received bytes=%d want=%d", len(received), len(message))
	}
	clientconnect.MessagePoolReturn(received)
	reassembler.Close()
	stats.RecordStreamSent(2048)
	stats.RecordStreamReceived(4096)
	streamBudget := clientconnect.NewH3HybridStreamSendBudget(1, 32, stats)
	if !streamBudget.Acquire(t.Context(), 24) {
		t.Fatal("hybrid stream queue metric reservation failed")
	}
	cancelledCtx, cancel := context.WithCancel(t.Context())
	cancel()
	if streamBudget.Acquire(cancelledCtx, 1) {
		t.Fatal("cancelled hybrid stream queue wait succeeded")
	}
	if streamBudget.Acquire(t.Context(), 33) {
		t.Fatal("oversized hybrid stream queue metric reservation succeeded")
	}
	defer streamBudget.Release(24)

	registry := prometheus.NewPedanticRegistry()
	if err := registry.Register(newConnectH3DatagramCollector(stats)); err != nil {
		t.Fatal(err)
	}
	metricFamilies, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	eventValues := map[string]float64{}
	byteValues := map[string]float64{}
	queueMessageValues := map[string]float64{}
	queueByteValues := map[string]float64{}
	queueWaitSeconds := -1.0
	for _, metricFamily := range metricFamilies {
		for _, metric := range metricFamily.Metric {
			switch metricFamily.GetName() {
			case "urnetwork_connect_h3_datagram_events_total":
				if len(metric.Label) != 1 || metric.Counter == nil {
					t.Fatalf("event metric has unexpected shape: %+v", metric)
				}
				label := metric.Label[0].GetValue()
				eventValues[label] = metric.Counter.GetValue()
			case "urnetwork_connect_h3_datagram_bytes_total":
				if len(metric.Label) != 1 || metric.Counter == nil {
					t.Fatalf("byte metric has unexpected shape: %+v", metric)
				}
				label := metric.Label[0].GetValue()
				byteValues[label] = metric.Counter.GetValue()
			case "urnetwork_connect_h3_hybrid_stream_queue_messages":
				if len(metric.Label) != 1 || metric.Gauge == nil {
					t.Fatalf("queue message metric has unexpected shape: %+v", metric)
				}
				queueMessageValues[metric.Label[0].GetValue()] = metric.Gauge.GetValue()
			case "urnetwork_connect_h3_hybrid_stream_queue_bytes":
				if len(metric.Label) != 1 || metric.Gauge == nil {
					t.Fatalf("queue byte metric has unexpected shape: %+v", metric)
				}
				queueByteValues[metric.Label[0].GetValue()] = metric.Gauge.GetValue()
			case "urnetwork_connect_h3_hybrid_stream_queue_wait_seconds_total":
				if len(metric.Label) != 0 || metric.Counter == nil {
					t.Fatalf("queue wait metric has unexpected shape: %+v", metric)
				}
				queueWaitSeconds = metric.Counter.GetValue()
			}
		}
	}
	if eventValues["sent_message"] != 1 || eventValues["received_message"] != 1 ||
		eventValues["sent_fragment"] != float64(len(datagrams)) ||
		eventValues["received_fragment"] != float64(len(datagrams)) {
		t.Fatalf("event metrics=%v", eventValues)
	}
	if byteValues["sent"] <= float64(len(message)) || byteValues["received"] != byteValues["sent"] {
		t.Fatalf("byte metrics=%v", byteValues)
	}
	if eventValues["stream_sent_message"] != 1 || eventValues["stream_received_message"] != 1 ||
		byteValues["stream_sent"] != 2048 || byteValues["stream_received"] != 4096 {
		t.Fatalf("hybrid stream metrics events=%v bytes=%v", eventValues, byteValues)
	}
	if eventValues["hybrid_stream_queue_wait"] != 1 ||
		eventValues["hybrid_stream_queue_oversize"] != 1 ||
		queueMessageValues["current"] != 1 || queueMessageValues["maximum"] != 1 ||
		queueByteValues["current"] != 24 || queueByteValues["maximum"] != 24 ||
		queueWaitSeconds < 0 {
		t.Fatalf(
			"hybrid stream queue metrics events=%v messages=%v bytes=%v wait=%f",
			eventValues,
			queueMessageValues,
			queueByteValues,
			queueWaitSeconds,
		)
	}
}
