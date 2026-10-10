package server

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/connect/v2026"
)

// messagePoolCollector keeps process-wide message-buffer ownership observable
// in every server executable. In particular, proxy imports the root server
// package but not controller, where these counters previously lived and were
// therefore absent from the service whose memory ceiling they diagnose.
//
// One aggregate snapshot per scrape keeps capacity, retention, and ownership
// internally comparable while avoiding one pool-shard walk per metric.
type messagePoolCollector struct {
	taken                           *prometheus.Desc
	returned                        *prometheus.Desc
	created                         *prometheus.Desc
	unpooledTaken                   *prometheus.Desc
	unpooledBytes                   *prometheus.Desc
	outstanding                     *prometheus.Desc
	retained                        *prometheus.Desc
	retainedBytes                   *prometheus.Desc
	capacityBytes                   *prometheus.Desc
	packetRetainedBytes             *prometheus.Desc
	largeObjectRetainedBytes        *prometheus.Desc
	deviceTunEgressOutstanding      *prometheus.Desc
	deviceTunEgressOutstandingBytes *prometheus.Desc
}

func newMessagePoolCollector() *messagePoolCollector {
	metric := func(name, help string) *prometheus.Desc {
		return prometheus.NewDesc("urnetwork_message_pool_"+name, help, nil, nil)
	}
	return &messagePoolCollector{
		taken:                           metric("taken_total", "Pooled message buffers taken"),
		returned:                        metric("returned_total", "Pooled message buffers returned"),
		created:                         metric("created_total", "Pooled message buffers allocated"),
		unpooledTaken:                   metric("unpooled_taken_total", "Messages served outside the pools (oversize classes)"),
		unpooledBytes:                   metric("unpooled_bytes_total", "Bytes served outside the pools"),
		outstanding:                     metric("outstanding", "Pooled message buffers currently taken and not finally returned"),
		retained:                        metric("retained", "Free message buffers currently retained for reuse"),
		retainedBytes:                   metric("retained_bytes", "Bytes in free message buffers currently retained for reuse"),
		capacityBytes:                   metric("capacity_bytes", "Configured process-wide upper bound on retained message-pool bytes"),
		packetRetainedBytes:             metric("packet_retained_bytes", "Bytes currently retained by packet message-pool classes"),
		largeObjectRetainedBytes:        metric("large_object_retained_bytes", "Bytes currently retained by large-object message-pool classes"),
		deviceTunEgressOutstanding:      metric("device_tun_egress_outstanding", "Device TUN egress packet roots currently owned outside the free lists"),
		deviceTunEgressOutstandingBytes: metric("device_tun_egress_outstanding_bytes", "Bytes in device TUN egress packet roots currently owned outside the free lists"),
	}
}

func (c *messagePoolCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.taken
	ch <- c.returned
	ch <- c.created
	ch <- c.unpooledTaken
	ch <- c.unpooledBytes
	ch <- c.outstanding
	ch <- c.retained
	ch <- c.retainedBytes
	ch <- c.capacityBytes
	ch <- c.packetRetainedBytes
	ch <- c.largeObjectRetainedBytes
	ch <- c.deviceTunEgressOutstanding
	ch <- c.deviceTunEgressOutstandingBytes
}

func (c *messagePoolCollector) Collect(ch chan<- prometheus.Metric) {
	stats := connect.GetMessagePoolAggregateStats()
	unpooledTaken, unpooledBytes := connect.MessagePoolUnpooledCounts()
	outstanding := uint64(0)
	if stats.Returned < stats.Taken {
		outstanding = stats.Taken - stats.Returned
	}

	ch <- prometheus.MustNewConstMetric(c.taken, prometheus.CounterValue, float64(stats.Taken))
	ch <- prometheus.MustNewConstMetric(c.returned, prometheus.CounterValue, float64(stats.Returned))
	ch <- prometheus.MustNewConstMetric(c.created, prometheus.CounterValue, float64(stats.Created))
	ch <- prometheus.MustNewConstMetric(c.unpooledTaken, prometheus.CounterValue, float64(unpooledTaken))
	ch <- prometheus.MustNewConstMetric(c.unpooledBytes, prometheus.CounterValue, float64(unpooledBytes))
	ch <- prometheus.MustNewConstMetric(c.outstanding, prometheus.GaugeValue, float64(outstanding))
	ch <- prometheus.MustNewConstMetric(c.retained, prometheus.GaugeValue, float64(stats.RetainedCount))
	ch <- prometheus.MustNewConstMetric(c.retainedBytes, prometheus.GaugeValue, float64(stats.RetainedByteCount))
	ch <- prometheus.MustNewConstMetric(c.capacityBytes, prometheus.GaugeValue, float64(stats.CapacityByteCount))
	ch <- prometheus.MustNewConstMetric(c.packetRetainedBytes, prometheus.GaugeValue, float64(stats.PacketRetainedByteCount))
	ch <- prometheus.MustNewConstMetric(c.largeObjectRetainedBytes, prometheus.GaugeValue, float64(stats.LargeObjectRetainedByteCount))
	ch <- prometheus.MustNewConstMetric(c.deviceTunEgressOutstanding, prometheus.GaugeValue, float64(stats.DeviceTunEgressOutstandingCount))
	ch <- prometheus.MustNewConstMetric(c.deviceTunEgressOutstandingBytes, prometheus.GaugeValue, float64(stats.DeviceTunEgressOutstandingByteCount))
}

var processMessagePoolCollector = newMessagePoolCollector()

func init() {
	prometheus.MustRegister(processMessagePoolCollector)
}
