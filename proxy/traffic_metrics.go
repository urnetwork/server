package proxy

// Bounded, identity-free ingress and WireGuard traffic metrics.

import (
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	proxylib "github.com/urnetwork/proxy"
)

const proxyMetricsMaxInterval = time.Minute

// socksIngressMetricsSource is the read-only bounded snapshot exposed by the
// sibling proxy library.
type socksIngressMetricsSource interface {
	Stats() proxylib.SocksStatsSnapshot
	ActiveCount() int
}

// httpIngressMetricsSource is the read-only bounded snapshot exposed by the
// sibling proxy library.
type httpIngressMetricsSource interface {
	Stats() proxylib.HttpStatsSnapshot
	ActiveCount() int
}

// proxySessionMaximum is one protocol maximum and observation timestamp.
type proxySessionMaximum struct {
	bucket     int64
	seconds    float64
	observedAt time.Time
}

// proxyTrafficMetrics owns direct counters plus coherent library snapshots
// and interval maximum/timestamp pairs.
type proxyTrafficMetrics struct {
	admissions     *prometheus.CounterVec
	sessions       *prometheus.CounterVec
	sessionSeconds *prometheus.HistogramVec
	sessionsActive *prometheus.GaugeVec
	bytes          *prometheus.CounterVec
	wgPackets      *prometheus.CounterVec
	wgBytes        *prometheus.CounterVec

	ingressEventsDesc      *prometheus.Desc
	ingressActiveDesc      *prometheus.Desc
	sessionMaximumDesc     *prometheus.Desc
	sessionMaximumTimeDesc *prometheus.Desc
	stateLock              sync.Mutex
	socksSource            socksIngressMetricsSource
	httpSource             httpIngressMetricsSource
	sessionMaximums        map[string]proxySessionMaximum
	now                    func() time.Time
}

// newProxyTrafficMetrics constructs one independently registerable family.
func newProxyTrafficMetrics(registerer prometheus.Registerer) *proxyTrafficMetrics {
	metrics := &proxyTrafficMetrics{
		admissions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "urnetwork", Subsystem: "proxy", Name: "ingress_admissions_total",
			Help: "Proxy ingress admissions by finite protocol and bounded result.",
		}, []string{"protocol", "outcome"}),
		sessions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "urnetwork", Subsystem: "proxy", Name: "sessions_total",
			Help: "Completed HTTP/SOCKS upstream sessions by finite protocol and bounded terminal outcome.",
		}, []string{"protocol", "outcome"}),
		sessionSeconds: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "urnetwork", Subsystem: "proxy", Name: "session_duration_seconds",
			Help:    "Completed HTTP/SOCKS upstream-session duration by finite protocol.",
			Buckets: []float64{0.001, 0.01, 0.05, 0.1, 0.5, 1, 5, 15, 30, 60, 300, 900, 3600},
		}, []string{"protocol"}),
		sessionsActive: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "urnetwork", Subsystem: "proxy", Name: "sessions_active",
			Help: "Successfully dialed HTTP/SOCKS upstream sessions not yet closed.",
		}, []string{"protocol"}),
		bytes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "urnetwork", Subsystem: "proxy", Name: "session_bytes_total",
			Help: "HTTP/SOCKS bytes at the client-facing relay boundary, by direction.",
		}, []string{"protocol", "direction"}),
		wgPackets: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "urnetwork", Subsystem: "proxy", Name: "wireguard_packets_total",
			Help: "Inner WireGuard packets offered at the provider-device boundary by direction and bounded outcome.",
		}, []string{"direction", "outcome"}),
		wgBytes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "urnetwork", Subsystem: "proxy", Name: "wireguard_bytes_total",
			Help: "Inner WireGuard packet bytes offered at the provider-device boundary by direction and bounded outcome.",
		}, []string{"direction", "outcome"}),
		ingressEventsDesc: prometheus.NewDesc(
			"urnetwork_proxy_ingress_events_total",
			"Cumulative process-generation events from bounded HTTP/SOCKS library snapshots.",
			[]string{"protocol", "reason"}, nil,
		),
		ingressActiveDesc: prometheus.NewDesc(
			"urnetwork_proxy_ingress_active",
			"Current in-flight requests, tunnels, connections, and UDP associations reported by each ingress library.",
			[]string{"protocol"}, nil,
		),
		sessionMaximumDesc: prometheus.NewDesc(
			"urnetwork_proxy_session_interval_max_seconds",
			"Maximum completed upstream-session duration in the latest one-minute interval with a close.",
			[]string{"protocol"}, nil,
		),
		sessionMaximumTimeDesc: prometheus.NewDesc(
			"urnetwork_proxy_session_interval_max_timestamp_seconds",
			"Unix time of the close observation backing the latest proxy session interval maximum.",
			[]string{"protocol"}, nil,
		),
		sessionMaximums: map[string]proxySessionMaximum{},
		now:             time.Now,
	}
	registerer.MustRegister(
		metrics.admissions,
		metrics.sessions,
		metrics.sessionSeconds,
		metrics.sessionsActive,
		metrics.bytes,
		metrics.wgPackets,
		metrics.wgBytes,
		metrics,
	)
	return metrics
}

var defaultProxyTrafficMetrics = newProxyTrafficMetrics(prometheus.DefaultRegisterer)

// Cache the four hot-path WireGuard counter pairs so packet forwarding never
// performs a label lookup or allocates a temporary batch solely for metrics.
var proxyWireGuardClientDeliveredPackets = defaultProxyTrafficMetrics.wgPackets.WithLabelValues("client_to_destination", "delivered")
var proxyWireGuardClientDeliveredBytes = defaultProxyTrafficMetrics.wgBytes.WithLabelValues("client_to_destination", "delivered")
var proxyWireGuardClientDroppedPackets = defaultProxyTrafficMetrics.wgPackets.WithLabelValues("client_to_destination", "dropped")
var proxyWireGuardClientDroppedBytes = defaultProxyTrafficMetrics.wgBytes.WithLabelValues("client_to_destination", "dropped")
var proxyWireGuardDestinationDeliveredPackets = defaultProxyTrafficMetrics.wgPackets.WithLabelValues("destination_to_client", "delivered")
var proxyWireGuardDestinationDeliveredBytes = defaultProxyTrafficMetrics.wgBytes.WithLabelValues("destination_to_client", "delivered")
var proxyWireGuardDestinationDroppedPackets = defaultProxyTrafficMetrics.wgPackets.WithLabelValues("destination_to_client", "dropped")
var proxyWireGuardDestinationDroppedBytes = defaultProxyTrafficMetrics.wgBytes.WithLabelValues("destination_to_client", "dropped")

// StartIngressMetrics attaches the two already-constructed ingress snapshots.
// Before this call their series are absent rather than false zeroes.
func StartIngressMetrics(socks socksIngressMetricsSource, httpSource httpIngressMetricsSource) {
	defaultProxyTrafficMetrics.setIngressSources(socks, httpSource)
}

// setIngressSources atomically installs both ingress sources.
func (self *proxyTrafficMetrics) setIngressSources(socks socksIngressMetricsSource, httpSource httpIngressMetricsSource) {
	self.stateLock.Lock()
	self.socksSource = socks
	self.httpSource = httpSource
	self.stateLock.Unlock()
}

// Describe implements prometheus.Collector.
func (self *proxyTrafficMetrics) Describe(descriptions chan<- *prometheus.Desc) {
	descriptions <- self.ingressEventsDesc
	descriptions <- self.ingressActiveDesc
	descriptions <- self.sessionMaximumDesc
	descriptions <- self.sessionMaximumTimeDesc
}

// Collect implements prometheus.Collector using only finite snapshot fields.
func (self *proxyTrafficMetrics) Collect(metrics chan<- prometheus.Metric) {
	self.stateLock.Lock()
	socksSource := self.socksSource
	httpSource := self.httpSource
	maximums := make(map[string]proxySessionMaximum, len(self.sessionMaximums))
	for protocol, maximum := range self.sessionMaximums {
		maximums[protocol] = maximum
	}
	self.stateLock.Unlock()

	if socksSource != nil {
		stats := socksSource.Stats()
		emitProxyIngressEvents(metrics, self.ingressEventsDesc, "socks", []proxyIngressEvent{
			{reason: "connect_dial_error", value: stats.ConnectDialErrors},
			{reason: "associate_oversize_datagram", value: stats.AssociateOversizeDatagrams},
			{reason: "associate_malformed_datagram", value: stats.AssociateMalformedDatagrams},
			{reason: "associate_foreign_datagram", value: stats.AssociateForeignDatagrams},
			{reason: "associate_dial_error", value: stats.AssociateDialErrors},
			{reason: "associate_send_error", value: stats.AssociateSendErrors},
			{reason: "associate_oversize_reply", value: stats.AssociateOversizeReplies},
			{reason: "associate_reply_error", value: stats.AssociateReplyErrors},
			{reason: "associate_flow_opened", value: stats.AssociateFlowsOpened},
			{reason: "associate_flow_evicted", value: stats.AssociateFlowsEvicted},
		})
		metrics <- prometheus.MustNewConstMetric(self.ingressActiveDesc, prometheus.GaugeValue, float64(socksSource.ActiveCount()), "socks")
	}
	if httpSource != nil {
		stats := httpSource.Stats()
		emitProxyIngressEvents(metrics, self.ingressEventsDesc, "http", []proxyIngressEvent{
			{reason: "connect_dial_error", value: stats.ConnectDialErrors},
			{reason: "connect_client_gone", value: stats.ConnectClientsGone},
			{reason: "connect_hijack_error", value: stats.ConnectHijackErrors},
			{reason: "request_dial_error", value: stats.RequestDialErrors},
			{reason: "request_not_replayed", value: stats.RequestsNotReplayed},
			{reason: "response_aborted", value: stats.ResponsesAborted},
			{reason: "body_too_large", value: stats.BodiesTooLarge},
			{reason: "upgrade_error", value: stats.UpgradeErrors},
		})
		metrics <- prometheus.MustNewConstMetric(self.ingressActiveDesc, prometheus.GaugeValue, float64(httpSource.ActiveCount()), "http")
	}
	for protocol, maximum := range maximums {
		metrics <- prometheus.MustNewConstMetric(self.sessionMaximumDesc, prometheus.GaugeValue, maximum.seconds, protocol)
		metrics <- prometheus.MustNewConstMetric(
			self.sessionMaximumTimeDesc,
			prometheus.GaugeValue,
			float64(maximum.observedAt.UnixNano())/float64(time.Second),
			protocol,
		)
	}
}

// proxyIngressEvent is one finite library snapshot field.
type proxyIngressEvent struct {
	reason string
	value  uint64
}

// emitProxyIngressEvents renders a fixed snapshot mapping.
func emitProxyIngressEvents(metrics chan<- prometheus.Metric, description *prometheus.Desc, protocol string, events []proxyIngressEvent) {
	for _, event := range events {
		metrics <- prometheus.MustNewConstMetric(description, prometheus.CounterValue, float64(event.value), protocol, event.reason)
	}
}

// recordAdmission increments one finite admission result.
func (self *proxyTrafficMetrics) recordAdmission(protocol string, outcome string) {
	self.admissions.WithLabelValues(protocol, outcome).Inc()
}

// observeSessionClose records one terminal session sample.
func (self *proxyTrafficMetrics) observeSessionClose(protocol string, outcome string, duration time.Duration) {
	self.sessions.WithLabelValues(protocol, outcome).Inc()
	self.sessionSeconds.WithLabelValues(protocol).Observe(duration.Seconds())
	now := self.now()
	bucket := now.UnixNano() / proxyMetricsMaxInterval.Nanoseconds()
	self.stateLock.Lock()
	maximum, ok := self.sessionMaximums[protocol]
	if !ok || maximum.bucket != bucket || maximum.seconds < duration.Seconds() {
		self.sessionMaximums[protocol] = proxySessionMaximum{bucket: bucket, seconds: duration.Seconds(), observedAt: now}
	}
	self.stateLock.Unlock()
}

// proxyMetricsConn counts traffic at the upstream-connection side of the
// ingress relay. Write is client-to-destination; Read is destination-to-client.
type proxyMetricsConn struct {
	net.Conn
	metrics      *proxyTrafficMetrics
	protocol     string
	start        time.Time
	readBytes    prometheus.Counter
	writeBytes   prometheus.Counter
	active       prometheus.Gauge
	closeOnce    sync.Once
	readFailure  atomic.Bool
	writeFailure atomic.Bool
}

// Read implements net.Conn.
func (self *proxyMetricsConn) Read(buffer []byte) (int, error) {
	count, err := self.Conn.Read(buffer)
	if 0 < count {
		self.readBytes.Add(float64(count))
	}
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
		self.readFailure.Store(true)
	}
	return count, err
}

// Write implements net.Conn.
func (self *proxyMetricsConn) Write(buffer []byte) (int, error) {
	count, err := self.Conn.Write(buffer)
	if 0 < count {
		self.writeBytes.Add(float64(count))
	}
	if err != nil && !errors.Is(err, net.ErrClosed) {
		self.writeFailure.Store(true)
	}
	return count, err
}

// Close implements net.Conn and records exactly one terminal sample.
func (self *proxyMetricsConn) Close() error {
	err := self.Conn.Close()
	self.closeOnce.Do(func() {
		outcome := "completed"
		if self.readFailure.Load() && self.writeFailure.Load() {
			outcome = "read_write_error"
		} else if self.readFailure.Load() {
			outcome = "read_error"
		} else if self.writeFailure.Load() {
			outcome = "write_error"
		}
		self.active.Dec()
		self.metrics.observeSessionClose(self.protocol, outcome, time.Since(self.start))
	})
	return err
}

// instrumentProxyConnection wraps a successful finite-protocol dial.
func instrumentProxyConnection(protocol string, conn net.Conn) net.Conn {
	return defaultProxyTrafficMetrics.instrumentConnection(protocol, conn)
}

// instrumentConnection wraps one successful dial with this metric family.
func (self *proxyTrafficMetrics) instrumentConnection(protocol string, conn net.Conn) net.Conn {
	protocol = boundedProxyProtocol(protocol)
	self.recordAdmission(protocol, "admitted")
	active := self.sessionsActive.WithLabelValues(protocol)
	active.Inc()
	return &proxyMetricsConn{
		Conn:       conn,
		metrics:    self,
		protocol:   protocol,
		start:      time.Now(),
		readBytes:  self.bytes.WithLabelValues(protocol, "destination_to_client"),
		writeBytes: self.bytes.WithLabelValues(protocol, "client_to_destination"),
		active:     active,
	}
}

// recordProxyAdmissionFailure maps internal detail to one finite outcome.
func recordProxyAdmissionFailure(protocol string, unauthorized bool) {
	protocol = boundedProxyProtocol(protocol)
	outcome := "dial_failed"
	if unauthorized {
		outcome = "unauthorized"
	}
	defaultProxyTrafficMetrics.recordAdmission(protocol, outcome)
}

// observeWireGuardPackets records an aggregate inner-packet batch without
// identity or destination labels. offset is the non-payload prefix stripped
// before the provider device consumes each packet.
func observeWireGuardPackets(direction string, outcome string, packets [][]byte, offset int) {
	if len(packets) == 0 {
		return
	}
	bytes := 0
	for _, packet := range packets {
		bytes += max(0, len(packet)-offset)
	}
	packetCounter, byteCounter := proxyWireGuardCounters(direction, outcome)
	packetCounter.Add(float64(len(packets)))
	byteCounter.Add(float64(bytes))
}

// observeWireGuardPacket avoids constructing a one-element slice on the hot
// packet path.
func observeWireGuardPacket(direction string, outcome string, packetBytes int) {
	packetCounter, byteCounter := proxyWireGuardCounters(direction, outcome)
	packetCounter.Inc()
	byteCounter.Add(float64(packetBytes))
}

// proxyWireGuardCounters returns one of four pre-bound finite metric pairs.
func proxyWireGuardCounters(direction string, outcome string) (prometheus.Counter, prometheus.Counter) {
	if direction == "client_to_destination" {
		if outcome == "delivered" {
			return proxyWireGuardClientDeliveredPackets, proxyWireGuardClientDeliveredBytes
		}
		return proxyWireGuardClientDroppedPackets, proxyWireGuardClientDroppedBytes
	}
	if outcome == "delivered" {
		return proxyWireGuardDestinationDeliveredPackets, proxyWireGuardDestinationDeliveredBytes
	}
	return proxyWireGuardDestinationDroppedPackets, proxyWireGuardDestinationDroppedBytes
}

// boundedProxyProtocol rejects any future caller-controlled protocol string.
func boundedProxyProtocol(protocol string) string {
	switch strings.ToLower(protocol) {
	case "http", "socks":
		return strings.ToLower(protocol)
	default:
		return "other"
	}
}
