// Connect transports terminate client H1 WebSocket and H3 QUIC connections,
// then expose each connection as one route to its resident client.
package connect

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	// "os"
	"strings"
	"time"
	// "runtime/debug"
	"encoding/binary"
	mathrand "math/rand"
	"strconv"
	"sync"

	"github.com/gorilla/websocket"
	quic "github.com/quic-go/quic-go"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
	"github.com/urnetwork/server/v2026"
	// "github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/jwt"
	"github.com/urnetwork/server/v2026/model"
)

// each client connection is a transport for the resident client
// there can be multiple simultaneous client connections from the same client instance
// all connections from the same client will eventually terminate at the same resident,
// where each connection will be a `connect.Transport` and traffic will be distributed across the transports

var connectedGauge = prometheus.NewGauge(
	prometheus.GaugeOpts{
		Namespace: "urnetwork",
		Subsystem: "connect",
		Name:      "connected_clients",
		Help:      "Number of connected clients",
	},
)

var defaultConnectH3DatagramStats = &connect.H3DatagramStats{}

// connectH3DatagramCollector exports the shared candidate-carrier counters
// without putting Prometheus label lookup or locking on either packet pump.
type connectH3DatagramCollector struct {
	stats                 *connect.H3DatagramStats
	eventDesc             *prometheus.Desc
	byteDesc              *prometheus.Desc
	queueMessageDesc      *prometheus.Desc
	queueByteDesc         *prometheus.Desc
	queueWaitDurationDesc *prometheus.Desc
}

// Creates the process-wide collector used by default ConnectHandler settings.
func newConnectH3DatagramCollector(stats *connect.H3DatagramStats) *connectH3DatagramCollector {
	return &connectH3DatagramCollector{
		stats: stats,
		eventDesc: prometheus.NewDesc(
			"urnetwork_connect_h3_datagram_events_total",
			"H3 DATAGRAM carrier events after authenticated capability negotiation",
			[]string{"event"},
			nil,
		),
		byteDesc: prometheus.NewDesc(
			"urnetwork_connect_h3_datagram_bytes_total",
			"H3 DATAGRAM envelope bytes passed to or received from quic-go",
			[]string{"direction"},
			nil,
		),
		queueMessageDesc: prometheus.NewDesc(
			"urnetwork_connect_h3_hybrid_stream_queue_messages",
			"Current and lifetime-maximum messages retained by bounded H3 hybrid stream handoffs",
			[]string{"state"},
			nil,
		),
		queueByteDesc: prometheus.NewDesc(
			"urnetwork_connect_h3_hybrid_stream_queue_bytes",
			"Current and lifetime-maximum backing bytes retained by bounded H3 hybrid stream handoffs",
			[]string{"state"},
			nil,
		),
		queueWaitDurationDesc: prometheus.NewDesc(
			"urnetwork_connect_h3_hybrid_stream_queue_wait_seconds_total",
			"Total time H3 lane dispatchers waited for bounded hybrid stream handoff capacity",
			nil,
			nil,
		),
	}
}

// Describe publishes the two bounded-label metric families.
func (self *connectH3DatagramCollector) Describe(descriptions chan<- *prometheus.Desc) {
	descriptions <- self.eventDesc
	descriptions <- self.byteDesc
	descriptions <- self.queueMessageDesc
	descriptions <- self.queueByteDesc
	descriptions <- self.queueWaitDurationDesc
}

// Collect snapshots atomics once and emits every closed-set event label.
func (self *connectH3DatagramCollector) Collect(metrics chan<- prometheus.Metric) {
	snapshot := self.stats.Snapshot()
	events := []struct {
		label string
		value uint64
	}{
		{label: "sent_message", value: snapshot.SentMessageCount},
		{label: "sent_fragment", value: snapshot.SentFragmentCount},
		{label: "send_error", value: snapshot.SendErrorCount},
		{label: "received_message", value: snapshot.ReceivedMessageCount},
		{label: "received_fragment", value: snapshot.ReceivedFragmentCount},
		{label: "duplicate_fragment", value: snapshot.DuplicateFragmentCount},
		{label: "malformed_fragment", value: snapshot.MalformedFragmentCount},
		{label: "checksum_failure", value: snapshot.ChecksumFailureCount},
		{label: "reassembly_timeout", value: snapshot.ReassemblyTimeoutCount},
		{label: "reassembly_limit", value: snapshot.ReassemblyLimitCount},
		{label: "stream_sent_message", value: snapshot.StreamSentMessageCount},
		{label: "stream_received_message", value: snapshot.StreamReceivedMessageCount},
		{label: "hybrid_stream_queue_wait", value: snapshot.HybridStreamQueueWaitCount},
		{label: "hybrid_stream_queue_oversize", value: snapshot.HybridStreamQueueOversizeCount},
	}
	for _, event := range events {
		metrics <- prometheus.MustNewConstMetric(
			self.eventDesc,
			prometheus.CounterValue,
			float64(event.value),
			event.label,
		)
	}
	metrics <- prometheus.MustNewConstMetric(
		self.byteDesc,
		prometheus.CounterValue,
		float64(snapshot.SentByteCount),
		"sent",
	)
	metrics <- prometheus.MustNewConstMetric(
		self.byteDesc,
		prometheus.CounterValue,
		float64(snapshot.ReceivedByteCount),
		"received",
	)
	metrics <- prometheus.MustNewConstMetric(
		self.byteDesc,
		prometheus.CounterValue,
		float64(snapshot.StreamSentMessageByteCount),
		"stream_sent",
	)
	metrics <- prometheus.MustNewConstMetric(
		self.byteDesc,
		prometheus.CounterValue,
		float64(snapshot.StreamReceivedMessageByteCount),
		"stream_received",
	)
	for _, queue := range []struct {
		description *prometheus.Desc
		current     uint64
		maximum     uint64
	}{
		{
			description: self.queueMessageDesc,
			current:     snapshot.HybridStreamQueueCurrentMessageCount,
			maximum:     snapshot.HybridStreamQueueMaximumMessageCount,
		},
		{
			description: self.queueByteDesc,
			current:     snapshot.HybridStreamQueueCurrentByteCount,
			maximum:     snapshot.HybridStreamQueueMaximumByteCount,
		},
	} {
		metrics <- prometheus.MustNewConstMetric(
			queue.description,
			prometheus.GaugeValue,
			float64(queue.current),
			"current",
		)
		metrics <- prometheus.MustNewConstMetric(
			queue.description,
			prometheus.GaugeValue,
			float64(queue.maximum),
			"maximum",
		)
	}
	metrics <- prometheus.MustNewConstMetric(
		self.queueWaitDurationDesc,
		prometheus.CounterValue,
		snapshot.HybridStreamQueueWaitDuration.Seconds(),
	)
}

// FIXME without egress verification, we rely on the ingress address to match the egress address
// FIXME turn this on to solve ipv6 aliasing abuse on the network
// currently the network only supports v4 egress
// egress verification and v6 support both need to be addressed in the future
const AllowOnlyIpv4 = false

const (
	connectH1WriteBatchMaxMessageCount = 8
	connectH3WriteBatchMaxMessageCount = 16
	connectH3WriteBatchMaxByteCount    = 64 * 1024
)

// Mirrors the client-side pre-publication query. quic-go v0.61.0 cannot queue
// a 2,048-byte DATAGRAM because its packet buffer is capped at 1,452 bytes, so
// the returned DatagramTooLargeError safely exposes the current path ceiling
// without emitting a probe message.
func initialConnectH3DatagramPathByteCount(
	configuredMaximum int,
	send func([]byte) error,
) int {
	initialMaximum := min(configuredMaximum, connect.H3InitialDatagramByteCount)
	var probe [2048]byte
	err := send(probe[:])
	var tooLargeErr *quic.DatagramTooLargeError
	if !errors.As(err, &tooLargeErr) ||
		int(tooLargeErr.MaxDatagramPayloadSize) <= connect.H3DatagramHeaderByteCount {
		return initialMaximum
	}
	return min(configuredMaximum, int(tooLargeErr.MaxDatagramPayloadSize))
}

// Narrows Gorilla's writer to the operations shared by production and the
// deterministic ready-batch ownership tests.
type connectH1WebSocketWriter interface {
	SetWriteDeadline(deadline time.Time) error
	WriteMessage(messageType int, data []byte) error
}

// Brackets one ready-only byte batch above the connection's TLS boundary.
type connectH1WriteBatch interface {
	BeginWriteBatch()
	AbortWriteBatch()
	FlushWriteBatch() error
}

// Returns the batching boundary only when Gorilla retained the connection
// installed at hijack. The explicit nil check prevents a failed assertion's
// typed nil pointer from becoming a non-nil interface.
func connectH1WriteBatchForConn(conn net.Conn) connectH1WriteBatch {
	writeBatchConn, ok := conn.(*connect.WebSocketWriteBatchConn)
	if !ok || writeBatchConn == nil {
		return nil
	}
	return writeBatchConn
}

// Preserves the original HTTP response behavior while replacing only the
// connection returned to Gorilla after the WebSocket hijack.
type connectH1BatchResponseWriter struct {
	http.ResponseWriter
}

// Delegates the hijack and inserts the shared pass-through batching wrapper.
func (self *connectH1BatchResponseWriter) Hijack() (
	net.Conn,
	*bufio.ReadWriter,
	error,
) {
	hijacker, ok := self.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("connect response writer does not support hijacking")
	}
	conn, readWriter, err := hijacker.Hijack()
	if err != nil {
		return nil, nil, err
	}
	return connect.NewWebSocketWriteBatchConn(conn), readWriter, nil
}

// Writes one user frame immediately, plus at most seven more frames already
// queued at the same instant. Every dequeued pooled buffer is returned after
// the terminal flush; successful accounting is published only after that
// flush reaches the delegated connection.
func writeConnectH1UserReadyBatch(
	ctx context.Context,
	writer connectH1WebSocketWriter,
	writeBatch connectH1WriteBatch,
	receive <-chan []byte,
	firstMessage []byte,
	firstOpen bool,
	writeTimeout time.Duration,
	onSent func(ByteCount),
) (open bool, err error) {
	if !firstOpen {
		return false, nil
	}

	var messageStorage [connectH1WriteBatchMaxMessageCount][]byte
	messageStorage[0] = firstMessage
	messageCount := 1
	open = true
	if writeBatch != nil {
	drainReady:
		for messageCount < len(messageStorage) {
			select {
			case <-ctx.Done():
				open = false
				break drainReady
			case message, nextOpen := <-receive:
				if !nextOpen {
					open = false
					break drainReady
				}
				messageStorage[messageCount] = message
				messageCount += 1
			default:
				break drainReady
			}
		}
	}
	defer func() {
		for _, message := range messageStorage[:messageCount] {
			connect.MessagePoolReturn(message)
		}
	}()

	select {
	case <-ctx.Done():
		return false, nil
	default:
	}

	if err = writer.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
		return open, err
	}
	if writeBatch != nil {
		writeBatch.BeginWriteBatch()
	}

	var sentByteCounts [connectH1WriteBatchMaxMessageCount]ByteCount
	sentCount := 0
	for _, message := range messageStorage[:messageCount] {
		if len(message) <= 16 {
			glog.Infof("[rts]send message must be >16 bytes (%d)\n", len(message))
			continue
		}
		if err = writer.WriteMessage(websocket.BinaryMessage, message); err != nil {
			if writeBatch != nil {
				writeBatch.AbortWriteBatch()
			}
			return open, err
		}
		sentByteCounts[sentCount] = ByteCount(len(message))
		sentCount += 1
	}
	if writeBatch != nil {
		if err = writeBatch.FlushWriteBatch(); err != nil {
			writeBatch.AbortWriteBatch()
			return open, err
		}
	}
	if onSent != nil {
		for _, sentByteCount := range sentByteCounts[:sentCount] {
			onSent(sentByteCount)
		}
	}
	return open, nil
}

// var serviceTransitionTime = time.Now().Add(30 * time.Second)

func init() {
	prometheus.MustRegister(connectedGauge)
	prometheus.MustRegister(newConnectH3DatagramCollector(defaultConnectH3DatagramStats))
}

func DefaultConnectHandlerSettings() *ConnectHandlerSettings {
	// platformTransportSettings := connect.DefaultPlatformTransportSettings()
	return &ConnectHandlerSettings{
		// use the min value from older version of the client
		// `platformTransportSettings.PingTimeout`
		MinPingTimeout:   1 * time.Second,
		MaxPingTimeout:   15 * time.Second,
		PingTrackerCount: 4,
		WriteTimeout:     15 * time.Second,
		ReadTimeout:      30 * time.Second,

		// a single exchange message size is encoded as an `int32`
		// because message must be serialized/deserialized from memory,
		// there is a global limit on the size per message
		// messages above this size will be ignored from clients and the exchange
		// MaximumExchangeMessageByteCount: ByteCount(4096),

		QuicConnectTimeout:   15 * time.Second,
		QuicHandshakeTimeout: 15 * time.Second,

		ListenH3Port: 443,
		// Clients continue to use public UDP/53. IPv4 ingress routers DNAT it to
		// edge UDP/8053, and nginx forwards PPv2 to this unprivileged listener.
		ListenDnsPort:       8053,
		EnableProxyProtocol: true,
		// Floor the framer at the connect runtime minimum message length: every
		// framer on the resident exchange flow must admit the handshake's TLS
		// server flight (one ~2.2 KiB pack). Also backs the websocket read limit.
		FramerSettings:       connect.DefaultFramerSettings(int(connect.DefaultClientSettings().MinimumMessageLenLimit())),
		TransportTlsSettings: server.DefaultTransportTlsSettings(),
		EnableH3Datagrams:    true,
		H3DatagramSettings:   connect.DefaultH3DatagramSettings(),
		H3DatagramStats:      defaultConnectH3DatagramStats,

		ConnectionAnnounceTimeout:   5 * time.Second,
		ConnectionAnnounceSettings:  *DefaultConnectionAnnounceSettings(),
		ConnectionRateLimitSettings: *DefaultConnectionRateLimitSettings(),
	}
}

type ConnectHandlerSettings struct {
	MinPingTimeout   time.Duration
	MaxPingTimeout   time.Duration
	PingTrackerCount int
	WriteTimeout     time.Duration
	ReadTimeout      time.Duration
	// MaximumExchangeMessageByteCount ByteCount
	QuicConnectTimeout   time.Duration
	QuicHandshakeTimeout time.Duration
	ListenH3Port         int
	ListenDnsPort        int
	EnableProxyProtocol  bool
	FramerSettings       *connect.FramerSettings
	TransportTlsSettings *server.TransportTlsSettings
	EnableH3Datagrams    bool
	H3DatagramSettings   *connect.H3DatagramSettings
	H3DatagramStats      *connect.H3DatagramStats
	// H3QuicPacketStats enables opt-in packet/frame diagnostics without
	// retaining qlog events or payloads. Nil keeps tracing disabled.
	H3QuicPacketStats         *connect.H3QuicPacketStats
	ConnectionAnnounceTimeout time.Duration
	// per-connection latency/speed test schedule.
	// nil selects a default based on the transport version.
	ConnectionTestConfig *TestConfig
	ConnectionAnnounceSettings
	ConnectionRateLimitSettings
}

// Keeps server-resident Transfer recovery symmetric with the client H3 path.
func connectH3TransferCarrierProperties(useH3Datagrams bool) connect.TransferCarrierProperties {
	return connect.TransferCarrierProperties{
		Unreliable:              useH3Datagrams,
		UnreliableFlowIsolation: useH3Datagrams,
		UnreliableFlowReserve:   useH3Datagrams,
	}
}

// Joins all per-connection workers before their handler releases shared state.
type connectHandlerWorkers struct {
	workers sync.WaitGroup
}

// Starts one owned per-connection worker.
func (self *connectHandlerWorkers) start(run func()) {
	self.workers.Add(1)
	go server.HandleError(func() {
		defer self.workers.Done()
		run()
	})
}

// Waits until every started worker has returned its local ownership.
func (self *connectHandlerWorkers) wait() {
	self.workers.Wait()
}

// Stops H1 connection resources before joining every worker that can retain a
// dequeued resident message.
func finishH1ConnectHandlerWorkers(workers *connectHandlerWorkers, stop func()) {
	stop()
	workers.wait()
}

// Stops H3 stream resources before joining the writer's pending batch owner
// and every other per-stream worker.
func finishH3ConnectHandlerWorkers(workers *connectHandlerWorkers, stop func()) {
	stop()
	workers.wait()
}

// Joins final connection registration cleanup before handler idle can expose
// the model and database as safe to tear down.
func finishConnectionAnnounce(announce *ConnectionAnnounce) {
	announce.CloseAndWait()
}

// newConnectQuicConfig keeps the server half of H3 aligned with the client's
// conservative startup packet and enabled DPLPMTUD behavior.
func newConnectQuicConfig(settings *ConnectHandlerSettings) *quic.Config {
	config := &quic.Config{
		HandshakeIdleTimeout: settings.QuicConnectTimeout + settings.QuicHandshakeTimeout,
		MaxIdleTimeout:       settings.MaxPingTimeout * 4,
		// Keep hybrid liveness independent from the application writer. That
		// writer can block behind quic-go's bounded DATAGRAM queue and must not
		// starve the only connection-level probe on a constrained uplink.
		KeepAlivePeriod:   settings.MaxPingTimeout,
		Allow0RTT:         true,
		InitialPacketSize: connect.H3InitialPacketByteCount,
		EnableDatagrams:   settings.EnableH3Datagrams,
	}
	if settings.H3QuicPacketStats != nil {
		config.Tracer = settings.H3QuicPacketStats.Tracer
	}
	return config
}

type ConnectHandler struct {
	ctx       context.Context
	cancel    context.CancelFunc
	handlerId server.Id
	exchange  *Exchange
	settings  *ConnectHandlerSettings

	transportTls               *server.TransportTls
	serviceTransitionTime      time.Time
	h3PacketConn               net.PacketConn
	dnsPacketConn              net.PacketConn
	h3DatagramReassemblyBudget *connect.H3DatagramReassemblyBudget

	activeLock  sync.Mutex
	activeCount int
	activeZero  chan struct{}
	closing     bool
}

// ConnectHandlerPacketConns contains already-bound packet sockets for the QUIC
// transports. The handler owns and closes every non-nil socket. Tests and
// embedded servers use these to retain an OS-assigned port continuously
// through startup instead of releasing a probe socket before rebinding it.
type ConnectHandlerPacketConns struct {
	H3  net.PacketConn
	Dns net.PacketConn
}

func NewConnectHandlerWithDefaults(ctx context.Context, handlerId server.Id, exchange *Exchange) *ConnectHandler {
	return NewConnectHandler(ctx, handlerId, exchange, DefaultConnectHandlerSettings())
}

func NewConnectHandler(ctx context.Context, handlerId server.Id, exchange *Exchange, settings *ConnectHandlerSettings) *ConnectHandler {
	return NewConnectHandlerWithPacketConns(
		ctx,
		handlerId,
		exchange,
		settings,
		ConnectHandlerPacketConns{},
	)
}

func NewConnectHandlerWithPacketConns(
	ctx context.Context,
	handlerId server.Id,
	exchange *Exchange,
	settings *ConnectHandlerSettings,
	packetConns ConnectHandlerPacketConns,
) *ConnectHandler {
	cancelCtx, cancel := context.WithCancel(ctx)
	activeZero := make(chan struct{})
	activeCount := 0
	if connectHandlerPacketEndpointEnabled(settings.ListenH3Port, packetConns.H3) {
		activeCount += 1
	}
	if connectHandlerPacketEndpointEnabled(settings.ListenDnsPort, packetConns.Dns) {
		activeCount += 1
	}
	if activeCount == 0 {
		close(activeZero)
	}

	transportTls, err := server.NewTransportTlsFromConfig(settings.TransportTlsSettings)
	if err != nil {
		glog.Errorf("[c]Could not initialize tls config. Disabling transport. = %s\n", err)
		transportTls = server.NewTransportTls(map[string]bool{}, server.DefaultTransportTlsSettings())
	}
	h3DatagramSettings := settings.H3DatagramSettings
	if h3DatagramSettings == nil {
		h3DatagramSettings = connect.DefaultH3DatagramSettings()
	}
	if settingsErr := h3DatagramSettings.Validate(); settingsErr != nil {
		panic(settingsErr)
	}
	settings.H3DatagramSettings = h3DatagramSettings
	if settings.H3DatagramStats == nil {
		settings.H3DatagramStats = &connect.H3DatagramStats{}
	}

	// the announce registers the peer with the SAME ttl the resident
	// heartbeat refreshes it (ExchangeResidentTtl); disconnect detection
	// relies on the entry expiring at that cadence once heartbeats stop, so a
	// larger registration ttl would delay disconnect by its full duration.
	// Derive it from the exchange so the two can never drift.
	settings.ConnectionAnnounceSettings.PeerRegisterTtl = exchange.settings.ExchangeResidentTtl
	// The exchange flag is the single network-peers switch: propagate it into the
	// announce settings so enabling peers in one place gates both the announce-time
	// registration and the exchange-side listener/heartbeat/teardown.
	settings.ConnectionAnnounceSettings.EnableNetworkPeers = exchange.settings.EnableNetworkPeers
	// The exchange flag is the single drain-excuse switch: the exchange side
	// writes the markers on drain, the announce side consumes them.
	settings.ConnectionAnnounceSettings.EnableDrainExcuse = exchange.settings.EnableDrainExcuse

	h := &ConnectHandler{
		ctx:                   cancelCtx,
		cancel:                cancel,
		handlerId:             handlerId,
		exchange:              exchange,
		settings:              settings,
		transportTls:          transportTls,
		serviceTransitionTime: time.Now().Add(2 * exchange.settings.DrainAllTimeout),
		h3PacketConn:          packetConns.H3,
		dnsPacketConn:         packetConns.Dns,
		h3DatagramReassemblyBudget: connect.NewH3DatagramReassemblyBudget(
			h3DatagramSettings.ProcessReassemblyByteCount,
		),
		activeCount: activeCount,
		activeZero:  activeZero,
	}

	go server.HandleError(h.run, cancel)

	return h
}

func (self *ConnectHandler) run() {
	defer self.cancel()
	defer self.markClosing()

	if connectHandlerPacketEndpointEnabled(self.settings.ListenH3Port, self.h3PacketConn) {
		go func() {
			defer self.endHandle()
			server.HandleError(self.runH3, self.cancel)
		}()
	}
	if connectHandlerPacketEndpointEnabled(self.settings.ListenDnsPort, self.dnsPacketConn) {
		go func() {
			defer self.endHandle()
			server.HandleError(self.runH3Dns, self.cancel)
		}()
	}

	select {
	case <-self.ctx.Done():
	}
}

func connectHandlerPortEnabled(port int) bool {
	return 0 < port && server.HasPort(port)
}

func connectHandlerPacketEndpointEnabled(port int, packetConn net.PacketConn) bool {
	return packetConn != nil || connectHandlerPortEnabled(port)
}

func (self *ConnectHandler) beginHandle() bool {
	self.activeLock.Lock()
	defer self.activeLock.Unlock()

	if self.closing || self.ctx.Err() != nil {
		return false
	}
	if self.activeCount == 0 {
		self.activeZero = make(chan struct{})
	}
	self.activeCount += 1
	return true
}

func (self *ConnectHandler) endHandle() {
	self.activeLock.Lock()
	defer self.activeLock.Unlock()

	self.activeCount -= 1
	if self.activeCount < 0 {
		panic("connect handler active count became negative")
	}
	if self.activeCount == 0 {
		close(self.activeZero)
	}
}

func (self *ConnectHandler) startHandle(do func()) bool {
	if !self.beginHandle() {
		return false
	}
	go func() {
		defer self.endHandle()
		server.HandleError(do)
	}()
	return true
}

func (self *ConnectHandler) markClosing() {
	self.activeLock.Lock()
	defer self.activeLock.Unlock()
	self.closing = true
}

func (self *ConnectHandler) Close() {
	self.markClosing()
	self.cancel()
}

func (self *ConnectHandler) WaitForIdle(ctx context.Context) bool {
	self.activeLock.Lock()
	activeZero := self.activeZero
	self.activeLock.Unlock()

	select {
	case <-ctx.Done():
		return false
	case <-activeZero:
		return true
	}
}

func (self *ConnectHandler) Connect(w http.ResponseWriter, r *http.Request) {
	if !self.beginHandle() {
		http.Error(w, "connect handler closed", http.StatusServiceUnavailable)
		return
	}
	defer self.endHandle()

	// a draining service refuses new connections, so a redialing client fails
	// fast and lands on a sibling service via the lb (CONNECTDRAIN2.md §3.3)
	if self.exchange.settings.EnableDrainCoordination && self.exchange.IsDraining() {
		http.Error(w, "draining", http.StatusServiceUnavailable)
		return
	}

	handleCtx, handleCancel := context.WithCancel(self.ctx)
	// handleCancel := func() {
	// 	defer handleCancel_()
	// 	var first bool
	// 	select {
	// 	case <- handleCtx.Done():
	// 		first = false
	// 	default:
	// 		first = true
	// 	}
	// 	if first {
	// 		glog.Infof("[t]handle cancel: %s\n", server.ErrorJson(r, debug.Stack()))
	// 	}
	// }
	var requestWorkers connectHandlerWorkers
	defer func() {
		handleCancel()
		requestWorkers.wait()
	}()

	requestWorkers.start(func() {
		defer handleCancel()
		select {
		case <-r.Context().Done():
		case <-handleCtx.Done():
		}
	})

	connectedGauge.Add(1)
	defer connectedGauge.Sub(1)

	// find the client ip:port from the request header
	// `X-Forwarded-For` is added by the warp lb
	clientAddress := r.Header.Get("X-UR-Forwarded-For")
	if clientAddress == "" {
		clientIpStr := r.Header.Get("X-Forwarded-For")
		clientPortStr := r.Header.Get("X-Forwarded-Source-Port")
		if clientIpStr != "" && clientPortStr != "" {
			clientAddress = fmt.Sprintf("%s:%s", clientIpStr, clientPortStr)
		}
	}
	if clientAddress == "" {
		// use the raw connection remote address
		clientAddress = r.RemoteAddr
	}

	if addrPort, err := server.ParseClientAddress(clientAddress); err != nil {
		return
	} else if AllowOnlyIpv4 && !addrPort.Addr().Is4() {
		return
	}

	rateLimit, err := NewConnectionRateLimit(
		handleCtx,
		clientAddress,
		self.handlerId,
		&self.settings.ConnectionRateLimitSettings,
	)
	if err != nil {
		glog.Infof("[t]rate limit init err = %s\n", err)
		return
	}
	err, disconnect := rateLimit.Connect()
	defer disconnect()
	if err != nil {
		if glog.V(1) {
			glog.Infof("[t]rate limit err = %s\n", err)
		}
		return
	}

	// attemp to parse the auth message from the header
	// if that fails, expect the auth message as the first message
	auth, transportVersion := func() (*protocol.Auth, int) {
		if glog.V(2) {
			glog.Infof("[c]header metadata: %v\n", server.SafeHttpHeadersForLog(r.Header))
		}

		headerAuth := r.Header.Get("Authorization")
		headerAppVersion := r.Header.Get("X-UR-AppVersion")
		headerInstanceId := r.Header.Get("X-UR-InstanceId")
		headerTransportVersion := r.Header.Get("X-UR-TransportVersion")

		transportVersion := 0
		if i, err := strconv.Atoi(headerTransportVersion); err == nil {
			transportVersion = i
		}

		bearerPrefix := "bearer "

		if len(bearerPrefix) < len(headerAuth) && strings.ToLower(headerAuth[:len(bearerPrefix)]) == bearerPrefix {
			jwt := headerAuth[len(bearerPrefix):]

			instanceId, err := server.ParseId(headerInstanceId)
			if err == nil {
				return &protocol.Auth{
					ByJwt:      jwt,
					InstanceId: instanceId.Bytes(),
					AppVersion: headerAppVersion,
				}, transportVersion
			} else {
				glog.Infof("[c]Bad header X-UR-InstanceId: %s\n", headerInstanceId)
			}
		}

		return nil, transportVersion
	}()

	upgrader := websocket.Upgrader{
		ReadBufferSize:  4 * 1024,
		WriteBufferSize: 4 * 1024,
	}

	batchResponseWriter := &connectH1BatchResponseWriter{ResponseWriter: w}
	ws, err := upgrader.Upgrade(batchResponseWriter, r, nil)
	if err != nil {
		return
	}
	defer ws.Close()

	// enforce the message size limit on messages in
	// +4 for the framer's length header (the websocket carries the framed message).
	ws.SetReadLimit(int64(self.settings.FramerSettings.MaxMessageLen + 4))

	if auth == nil {
		ws.SetReadDeadline(time.Now().Add(self.settings.ReadTimeout))
		messageType, authFrameBytes, err := ws.ReadMessage()
		if err != nil {
			// server.Logger("TIMEOUT HA\n")
			return
		}
		if messageType != websocket.BinaryMessage {
			return
		}

		message, err := connect.DecodeFrame(authFrameBytes)
		if err != nil {
			return
		}
		var ok bool
		auth, ok = message.(*protocol.Auth)
		if !ok {
			return
		}

		// echo the auth message on successful auth
		ws.SetWriteDeadline(time.Now().Add(self.settings.WriteTimeout))
		err = ws.WriteMessage(websocket.BinaryMessage, authFrameBytes)
		if err != nil {
			// server.Logger("TIMEOUT HC\n")
			return
		}
	}

	// auth failures are client-driven and unbounded in rate, so they are
	// counted in the jwt package (urnetwork_auth_jwt_rejections_total) rather
	// than logged per occurrence; the detail is at V(1)
	byJwt, err := jwt.ParseByJwtForAudience(handleCtx, auth.ByJwt, jwt.ByJwtAudienceConnect)
	if err != nil {
		if glog.V(1) {
			glog.Infof("[t]auth jwt err = %s\n", err)
		}
		return
	}

	if byJwt.ClientId == nil {
		return
	}
	if err := jwt.ValidateByJwtState(handleCtx, byJwt, true); err != nil {
		if glog.V(1) {
			glog.Infof("[t]inactive auth jwt: %s\n", err)
		}
		return
	}

	clientId := *byJwt.ClientId

	instanceId, err := server.IdFromBytes(auth.InstanceId)
	if err != nil {
		return
	}

	// verify the client is still part of the network
	// this will fail for example if the client has been removed
	networkId := model.GetNetworkClientNetwork(handleCtx, clientId)
	if networkId == nil || *networkId != byJwt.NetworkId {
		// server.Logger("ERROR HB\n")
		return
	}

	connectionId := server.NewId()
	self.exchange.registerConnection(clientId, connectionId, handleCancel)
	defer self.exchange.unregisterConnection(clientId, connectionId)

	c := func() {
		announceTimeout := time.Duration(0)
		if self.serviceTransitionTime.Before(time.Now()) {
			// the service has transitioned all the connections from the old to new
			// now we delay the announcement to make sure the transport is stable
			announceTimeout = self.settings.ConnectionAnnounceTimeout
		}
		var testConfig *TestConfig
		if self.settings.ConnectionTestConfig != nil {
			testConfig = self.settings.ConnectionTestConfig
		} else if transportVersion < 2 {
			testConfig = V0TestConfig()
		} else {
			testConfig = DefaultTestConfig()
		}
		announce := NewConnectionAnnounce(
			handleCtx,
			handleCancel,
			byJwt.NetworkId,
			clientId,
			clientAddress,
			self.handlerId,
			announceTimeout,
			testConfig,
			&self.settings.ConnectionAnnounceSettings,
		)
		defer finishConnectionAnnounce(announce)

		residentTransport := NewResidentTransport(
			handleCtx,
			self.exchange,
			clientId,
			instanceId,
		)
		var workers connectHandlerWorkers
		defer finishH1ConnectHandlerWorkers(&workers, func() {
			handleCancel()
			residentTransport.Close()
			ws.Close()
		})
		workers.start(func() {
			defer handleCancel()
			residentTransport.Run()
		})

		pingTracker := NewPingTracker(self.settings.PingTrackerCount)

		workers.start(func() {
			defer handleCancel()

			var speedTest *SpeedTest

			for {

				ws.SetReadDeadline(time.Now().Add(self.settings.ReadTimeout))
				messageType, r, err := ws.NextReader()
				if err != nil {
					// glog.Errorf("[t]read err = %s\n", err)
					if connectionId := announce.ConnectionId(); connectionId != nil {
						model.ClientError(handleCtx, *networkId, clientId, *connectionId, "read", err)
					}
					return
				}

				switch messageType {
				case websocket.BinaryMessage:

					message, err := connect.MessagePoolReadAll(r)
					if err != nil {
						if connectionId := announce.ConnectionId(); connectionId != nil {
							model.ClientError(handleCtx, *networkId, clientId, *connectionId, "read", err)
						}
						return
					}

					// reliability tracking
					announce.ReceiveMessage(ByteCount(len(message)))

					if len(message) <= 16 {
						if len(message) == 0 {
							// ping
							pingTracker.ReceivePing()
						} else if len(message) == 5 {
							switch message[0] {
							case connect.TransportControlSpeedStart:
								speedTest = &SpeedTest{
									TestId: binary.BigEndian.Uint32(message[1:5]),
								}
							case connect.TransportControlSpeedStop:
								announce.ReceiveSpeed(speedTest)
								speedTest = nil
							}
						} else if len(message) == 16 {
							// latency response
							if testId, err := server.IdFromBytes(message); err == nil {
								announce.ReceiveLatency(&LatencyTest{
									TestId: testId,
								})
							}
						}
						connect.MessagePoolReturn(message)
						continue
					}
					// during a speed test, count all incoming bytes (both
					// the client's echoed chunks and any user traffic the
					// client is concurrently sending) toward the throughput
					// total. user traffic continues to flow rather than being
					// dropped — speed test runs in parallel.
					if speedTest != nil {
						speedTest.TotalByteCount += model.ByteCount(len(message))
					}

					pingTracker.Receive()

					messageByteCount := len(message)
					sendResult := residentTransport.trySendMessage(
						handleCtx.Done(),
						message,
					)
					if sendResult == pooledMessageSendDone {
						return
					}
					if sendResult == pooledMessageSendDropped {
						recordReceiveQueueDrop(receiveQueueBoundaryConnectH1, messageByteCount)
					}
					if sendResult == pooledMessageSendDelivered {
						if glog.V(2) {
							glog.Infof("[rtr] <-%s\n", clientId)
						}
					}
				}
			}
		})

		workers.start(func() {
			defer handleCancel()

			recordWriteError := func(err error) {
				// A WebSocket deadline or partial write is terminal; the Transfer
				// sequence retries each logical message over a replacement route.
				if connectionId := announce.ConnectionId(); connectionId != nil {
					model.ClientError(handleCtx, *networkId, clientId, *connectionId, "write", err)
				}
			}
			write := func(message []byte, returnToPool bool) error {
				if returnToPool {
					defer connect.MessagePoolReturn(message)
				}
				err := ws.SetWriteDeadline(time.Now().Add(self.settings.WriteTimeout))
				if err == nil {
					err = ws.WriteMessage(websocket.BinaryMessage, message)
				}
				if err != nil {
					recordWriteError(err)
					return err
				}
				announce.SendMessage(ByteCount(len(message)))
				if glog.V(2) {
					glog.Infof("[ts] ->%s\n", clientId)
				}
				return nil
			}

			writeBatchConn := connectH1WriteBatchForConn(ws.UnderlyingConn())
			writeUser := func(message []byte, ok bool) bool {
				open, err := writeConnectH1UserReadyBatch(
					handleCtx,
					ws,
					writeBatchConn,
					residentTransport.receive,
					message,
					ok,
					self.settings.WriteTimeout,
					func(sentByteCount ByteCount) {
						announce.SendMessage(sentByteCount)
						if glog.V(2) {
							glog.Infof("[ts] ->%s\n", clientId)
						}
					},
				)
				if err != nil {
					recordWriteError(err)
					return false
				}
				return open
			}

			// speed test state: when non-zero, the writer is driving a speed
			// test and should interleave chunk writes with user traffic so
			// user packets aren't blocked. mirrors the client-side behavior.
			var speedTestId uint32
			speedTestChunksRemaining := 0
			chunk := make([]byte, 1024)

			// reusable ping timer (hot-path timer reuse): the slow select arms a
			// timer each iteration user traffic is briefly idle between bursts.
			pingTimer := time.NewTimer(0)
			defer pingTimer.Stop()

			for {
				if 0 < speedTestChunksRemaining {
					// drive the speed test in parallel with user traffic.
					// each iteration writes exactly one chunk (guaranteed
					// forward progress on the test even under heavy user
					// traffic), then opportunistically drains any pending
					// user messages before returning to the top.
					select {
					case <-handleCtx.Done():
						return
					case <-residentTransport.Done():
						return
					default:
					}
					mathrand.Read(chunk)
					chunkCopy := connect.MessagePoolCopy(chunk)
					if write(chunkCopy, true) != nil {
						return
					}
					speedTestChunksRemaining -= 1
					if speedTestChunksRemaining == 0 {
						stopMessage := connect.MessagePoolGet(5)
						stopMessage[0] = connect.TransportControlSpeedStop
						binary.BigEndian.PutUint32(stopMessage[1:5], speedTestId)
						if write(stopMessage, true) != nil {
							return
						}
					}
					// One bounded ready batch lets user traffic progress without
					// postponing the next speed chunk under a continuous backlog.
					select {
					case <-handleCtx.Done():
						return
					case <-residentTransport.Done():
						return
					case message, ok := <-residentTransport.receive:
						if !writeUser(message, ok) {
							return
						}
					default:
					}
					continue
				}

				// fast path without arming the ping timer
				select {
				case message, ok := <-residentTransport.receive:
					if !writeUser(message, ok) {
						return
					}
					continue
				default:
				}

				pingTimer.Reset(max(self.settings.MinPingTimeout, pingTracker.MinPingTimeout()))
				select {
				case <-handleCtx.Done():
					return
				case <-residentTransport.Done():
					return
				case message, ok := <-residentTransport.receive:
					if !writeUser(message, ok) {
						return
					}

				case <-pingTimer.C:
					if write(make([]byte, 0), false) != nil {
						return
					}
				case latencyTest := <-announce.PendingLatencyTest:
					if announce.SendLatency(latencyTest) {
						message := latencyTest.TestId.Bytes()
						if write(message, false) != nil {
							return
						}
					}
				case speedTest := <-announce.PendingSpeedTest:
					// client should echo control values and packets in speed test mode.
					// after starting, the writer interleaves chunk writes with
					// user traffic above so user packets aren't blocked. on the
					// reader side, all bytes received during the test (chunks +
					// user) count toward the throughput total.

					if announce.SendSpeed(speedTest) {
						startMessage := connect.MessagePoolGet(5)
						startMessage[0] = connect.TransportControlSpeedStart
						binary.BigEndian.PutUint32(startMessage[1:5], speedTest.TestId)
						if write(startMessage, true) != nil {
							return
						}
						speedTestId = speedTest.TestId
						speedTestChunksRemaining = int((speedTest.TotalByteCount + model.ByteCount(len(chunk)-1)) / model.ByteCount(len(chunk)))
					}
				}
			}
		})

		select {
		case <-handleCtx.Done():
			return
		}
	}
	if glog.V(2) {
		server.Trace(
			fmt.Sprintf("[t]connect %s", clientId),
			c,
		)
	} else {
		c()
	}
}

// note warp currently does not load balance h3 at nginx
// it passes the quic stream to exactly one service (connect)
// all tls is handled by this server

func (self *ConnectHandler) runH3() {
	self.listenQuic(
		self.settings.ListenH3Port,
		self.h3PacketConn,
		func(packetConn net.PacketConn) (net.PacketConn, error) {
			if self.settings.EnableProxyProtocol {
				packetConn = NewPpPacketConn(packetConn, DefaultWarpPpSettings())
			}
			return packetConn, nil
		},
	)
}

func (self *ConnectHandler) runH3Dns() {
	self.listenQuic(
		self.settings.ListenDnsPort,
		self.dnsPacketConn,
		func(packetConn net.PacketConn) (net.PacketConn, error) {
			if self.settings.EnableProxyProtocol {
				packetConn = NewPpPacketConn(packetConn, DefaultWarpPpSettings())
			}
			ptSettings := connect.DefaultPacketTranslationSettings()
			// FIXME read from config
			ptSettings.DnsTlds = [][]byte{[]byte("ur.xyz.")}
			return connect.NewPacketTranslation(
				self.ctx,
				connect.PacketTranslationModeDecode53,
				packetConn,
				ptSettings,
			)
		},
	)
}

func (self *ConnectHandler) listenQuic(
	port int,
	preboundPacketConn net.PacketConn,
	connTransform func(net.PacketConn) (net.PacketConn, error),
) {
	handleCtx, handleCancel := context.WithCancel(self.ctx)

	defer handleCancel()

	quicConfig := newConnectQuicConfig(self.settings)

	// type clientConfig struct {
	// 	tlsConfig *tls.Config
	// 	err error
	// }
	// clientConfigs := map[string]*clientConfig{}

	tlsConfig := &tls.Config{
		GetConfigForClient: self.transportTls.GetTlsConfigForClient,
	}

	serverConn := preboundPacketConn
	listenAddress := ""
	if serverConn == nil {
		listenIpv4, _, listenPort := server.RequireListenIpPort(port)
		listenAddress = net.JoinHostPort(listenIpv4, strconv.Itoa(listenPort))

		reusePort := false
		listenConfig := net.ListenConfig{}
		if reusePort {
			listenConfig.Control = server.SoReusePort
		}

		var err error
		serverConn, err = listenConfig.ListenPacket(
			handleCtx,
			"udp",
			listenAddress,
		)
		if err != nil {
			return
		}
	} else {
		listenAddress = serverConn.LocalAddr().String()
	}
	if glog.V(2) {
		glog.Infof("[c]h3 listen %s\n", listenAddress)
	}
	defer serverConn.Close()
	packetConn, err := connTransform(serverConn)
	if err != nil {
		return
	}
	defer packetConn.Close()
	quicTransport := &quic.Transport{
		Conn: packetConn,
		// createdConn: true,
		// isSingleUse: true,
	}
	listener, err := quicTransport.ListenEarly(tlsConfig, quicConfig)
	if err != nil {
		glog.Infof("[c]h3 listen %s err = %s\n", listenAddress, err)
		return
	}
	defer listener.Close()

	for {
		if glog.V(2) {
			glog.Infof("[c]h3 wait to accept connection %s\n", listenAddress)
		}
		conn, err := listener.Accept(handleCtx)
		if err != nil {
			glog.Infof("[c]h3 accept connection %s err = %s\n", listenAddress, err)
			return
		}

		glog.Infof("[c]h3 accept connection %s\n", listenAddress)
		if !self.startHandle(func() {
			defer conn.CloseWithError(0, "")

			err := self.connectQuic(conn)
			if err != nil {
				glog.Infof("[c]h3 connection exited %s err = %s\n", listenAddress, err)
			} else {
				glog.Infof("[c]h3 connection exited %s\n", listenAddress)
			}
		}) {
			conn.CloseWithError(0, "")
			return
		}
	}
}

// Reads one pooled H3 authentication frame and lends its exact wire bytes to
// the callback. The frame is returned on every decode and callback result.
func withConnectQuicAuthFrame(
	framer *connect.Framer,
	reader io.Reader,
	use func(auth *protocol.Auth, authFrameBytes []byte) error,
) error {
	return withObservedConnectQuicAuthFrame(framer, reader, nil, use)
}

// Reads one pooled H3 authentication frame and exposes its borrowed bytes to
// an optional deterministic ownership observer before protocol decoding.
func withObservedConnectQuicAuthFrame(
	framer *connect.Framer,
	reader io.Reader,
	observe func(authFrameBytes []byte),
	use func(auth *protocol.Auth, authFrameBytes []byte) error,
) error {
	authFrameBytes, err := framer.Read(reader)
	if err != nil {
		return err
	}
	defer connect.MessagePoolReturn(authFrameBytes)
	if observe != nil {
		observe(authFrameBytes)
	}

	message, err := connect.DecodeFrame(authFrameBytes)
	if err != nil {
		return err
	}
	auth, ok := message.(*protocol.Auth)
	if !ok {
		return fmt.Errorf("expected auth frame, got %T", message)
	}
	return use(auth, authFrameBytes)
}

func (self *ConnectHandler) connectQuic(conn *quic.Conn) error {
	handleCtx, handleCancel := context.WithCancel(self.ctx)
	var connectionWorkers connectHandlerWorkers
	defer func() {
		handleCancel()
		conn.CloseWithError(0, "")
		connectionWorkers.wait()
	}()

	connectionWorkers.start(func() {
		defer handleCancel()
		select {
		case <-conn.Context().Done():
		case <-handleCtx.Done():
		}
	})

	// find the client ip:port from the addr
	clientAddress := conn.RemoteAddr().String()

	if addrPort, err := server.ParseClientAddress(clientAddress); err != nil {
		return err
	} else if AllowOnlyIpv4 && !addrPort.Addr().Is4() {
		return fmt.Errorf("Only IPv4 is supported.")
	}

	rateLimit, err := NewConnectionRateLimit(
		handleCtx,
		clientAddress,
		self.handlerId,
		&self.settings.ConnectionRateLimitSettings,
	)
	if err != nil {
		glog.Infof("[t]rate limit init err = %s\n", err)
		return err
	}
	err, disconnect := rateLimit.Connect()
	defer disconnect()
	if err != nil {
		glog.Infof("[t]rate limit err = %s\n", err)
		return err
	}

	stream, err := conn.AcceptStream(handleCtx)
	if err != nil {
		return err
	}

	// FIXME
	/*
		if self.apiHostNames[earlyConn.ConnectionState.TLS.ServerName] {
			// pass off the stream to the internal api server
			return self.apiServer.OfferAccept(stream)
		}
	*/

	framer := connect.NewFramer(self.settings.FramerSettings)

	var byJwt *jwt.ByJwt
	var clientId server.Id
	var instanceId server.Id
	var connectionId server.Id
	useH3Datagrams := false
	connectionRegistered := false
	defer func() {
		if connectionRegistered {
			self.exchange.unregisterConnection(clientId, connectionId)
		}
	}()
	stream.SetReadDeadline(time.Now().Add(self.settings.ReadTimeout))
	err = withConnectQuicAuthFrame(
		framer,
		stream,
		func(auth *protocol.Auth, authFrameBytes []byte) error {
			var authErr error
			byJwt, authErr = jwt.ParseByJwtForAudience(
				handleCtx,
				auth.ByJwt,
				jwt.ByJwtAudienceConnect,
			)
			if authErr != nil {
				return authErr
			}
			if byJwt.ClientId == nil {
				return fmt.Errorf("Missing client id.")
			}
			if authErr = jwt.ValidateByJwtState(handleCtx, byJwt, true); authErr != nil {
				return authErr
			}

			clientId = *byJwt.ClientId
			instanceId, authErr = server.IdFromBytes(auth.InstanceId)
			if authErr != nil {
				return authErr
			}

			// Verify the client is still part of the network.
			networkId := model.GetNetworkClientNetwork(handleCtx, clientId)
			if networkId == nil || *networkId != byJwt.NetworkId {
				return fmt.Errorf("Client id is not part of network.")
			}

			connectionId = server.NewId()
			self.exchange.registerConnection(clientId, connectionId, handleCancel)
			connectionRegistered = true

			connectionState := conn.ConnectionState()
			authResponse, accepted := connect.AcceptH3DatagramAuthOffer(
				auth,
				self.settings.EnableH3Datagrams,
				connectionState.SupportsDatagrams.Local,
				connectionState.SupportsDatagrams.Remote,
			)
			useH3Datagrams = accepted

			stream.SetWriteDeadline(time.Now().Add(self.settings.WriteTimeout))
			if !useH3Datagrams {
				// Byte-for-byte echo preserves old-client behavior. A new client
				// talking to an old server sees accepted_version=0 and falls back.
				return framer.Write(stream, authFrameBytes)
			}
			responseBytes, responseErr := connect.EncodeFrame(
				authResponse,
				connect.DefaultProtocolVersion,
			)
			if responseErr != nil {
				return responseErr
			}
			defer connect.MessagePoolReturn(responseBytes)
			return framer.Write(stream, responseBytes)
		},
	)
	if err != nil {
		return err
	}

	c := func() {
		announceTimeout := time.Duration(0)
		if self.serviceTransitionTime.Before(time.Now()) {
			// the service has transitioned all the connections from the old to new
			// now we delay the announcement to make sure the transport is stable
			announceTimeout = self.settings.ConnectionAnnounceTimeout
		}
		announce := NewConnectionAnnounce(
			handleCtx,
			handleCancel,
			byJwt.NetworkId,
			clientId,
			clientAddress,
			self.handlerId,
			announceTimeout,
			V0TestConfig(),
			&self.settings.ConnectionAnnounceSettings,
		)
		defer finishConnectionAnnounce(announce)

		residentTransport := NewResidentTransportWithProperties(
			handleCtx,
			self.exchange,
			clientId,
			instanceId,
			connectH3TransferCarrierProperties(useH3Datagrams),
		)
		var datagramFragmenter *connect.H3DatagramFragmenter
		var datagramReassembler *connect.H3DatagramReassembler
		if useH3Datagrams {
			var datagramErr error
			datagramFragmenter, datagramErr = connect.NewH3DatagramFragmenter(
				self.settings.H3DatagramSettings,
				self.settings.H3DatagramStats,
			)
			if datagramErr != nil {
				glog.Infof("[t]H3 DATAGRAM sender init error = %s\n", datagramErr)
				return
			}
			datagramReassembler, datagramErr = connect.NewH3DatagramReassembler(
				self.settings.H3DatagramSettings,
				self.h3DatagramReassemblyBudget,
				self.settings.H3DatagramStats,
			)
			if datagramErr != nil {
				glog.Infof("[t]H3 DATAGRAM receiver init error = %s\n", datagramErr)
				return
			}
			defer datagramReassembler.Close()
		}
		var workers connectHandlerWorkers
		defer finishH3ConnectHandlerWorkers(&workers, func() {
			handleCancel()
			stream.CancelRead(0)
			stream.CancelWrite(0)
			residentTransport.Close()
		})
		workers.start(func() {
			defer handleCancel()
			residentTransport.Run()
		})
		workers.start(func() {
			defer handleCancel()
			select {
			case <-handleCtx.Done():
			case <-residentTransport.Done():
			}
		})

		pingTracker := NewPingTracker(self.settings.PingTrackerCount)
		deliverRoutedMessage := func(message []byte) bool {
			// Reliability tracking remains at the complete Transfer boundary,
			// independent of the selected hybrid lane or DATAGRAM fragments.
			announce.ReceiveMessage(ByteCount(len(message)))
			pingTracker.Receive()
			messageByteCount := len(message)
			sendResult := residentTransport.trySendMessage(
				handleCtx.Done(),
				message,
			)
			if sendResult == pooledMessageSendDone {
				return false
			}
			if sendResult == pooledMessageSendDropped {
				recordReceiveQueueDrop(receiveQueueBoundaryConnectH3, messageByteCount)
			}
			if sendResult == pooledMessageSendDelivered && glog.V(2) {
				glog.Infof("[rtr] <-%s\n", clientId)
			}
			return true
		}

		if useH3Datagrams {
			// Authentication, liveness, and routed frames above the negotiated
			// hybrid threshold share this reliable stream. DATAGRAM has its own
			// receive pump below; neither reader waits on resident admission.
			// Clear the authentication deadline because DATAGRAM activity does
			// not satisfy a stream read deadline. QUIC's connection-level idle
			// timeout owns peer liveness in hybrid mode, while handler cleanup
			// cancels the stream to unblock this reader deterministically.
			workers.start(func() {
				defer handleCancel()
				if err := stream.SetReadDeadline(time.Time{}); err != nil {
					return
				}
				for {
					message, err := framer.Read(stream)
					if err != nil {
						return
					}
					if len(message) != 0 {
						self.settings.H3DatagramStats.RecordStreamReceived(len(message))
						if !deliverRoutedMessage(message) {
							return
						}
						continue
					}
					announce.ReceiveMessage(0)
					connect.MessagePoolReturn(message)
					pingTracker.ReceivePing()
					datagramReassembler.Expire(time.Now())
				}
			})
		}

		workers.start(func() {
			defer handleCancel()

			for {
				var message []byte
				if useH3Datagrams {
					datagram, err := conn.ReceiveDatagram(handleCtx)
					if err != nil {
						return
					}
					message = datagramReassembler.Accept(datagram, time.Now())
					if message == nil {
						continue
					}
				} else {
					stream.SetReadDeadline(time.Now().Add(self.settings.ReadTimeout))
					var err error
					message, err = framer.Read(stream)
					if err != nil {
						if glog.V(2) {
							glog.Infof("[tr]h3 err = %s\n", err)
						}
						return
					}
					if 0 == len(message) {
						// ping
						pingTracker.ReceivePing()
						connect.MessagePoolReturn(message)
						continue
					}
				}

				if !deliverRoutedMessage(message) {
					return
				}
			}
		})

		// Hybrid lane dispatch occurs before either physical writer can block.
		// Its stream handoff is bounded by retained backing bytes as well as
		// message count. Stream-only H3 retains the resident queue and historical
		// batching path.
		var streamSend chan []byte
		var streamSendBudget *connect.H3HybridStreamSendBudget
		streamInput := (<-chan []byte)(residentTransport.receive)
		if useH3Datagrams {
			streamSend = make(chan []byte, connect.H3HybridStreamQueueMessageCount)
			streamSendBudget = connect.NewH3HybridStreamSendBudget(
				connect.H3HybridStreamQueueMessageCount,
				connect.H3HybridStreamQueueByteCount,
				self.settings.H3DatagramStats,
			)
			streamInput = streamSend
		}
		releaseStreamMessage := func(message []byte) {
			if streamSendBudget != nil {
				streamSendBudget.Release(connect.H3HybridStreamRetainedByteCount(message))
			}
			connect.MessagePoolReturn(message)
		}
		maxDatagramByteCount := initialConnectH3DatagramPathByteCount(
			self.settings.H3DatagramSettings.TargetDatagramByteCount,
			conn.SendDatagram,
		)
		sendDatagramMessage := func(message []byte) (useStream bool, sendErr error) {
			var nextMaxDatagramByteCount int
			useStream, nextMaxDatagramByteCount, sendErr = datagramFragmenter.SendHybrid(
				message,
				maxDatagramByteCount,
				conn.SendDatagram,
			)
			maxDatagramByteCount = nextMaxDatagramByteCount
			return useStream, sendErr
		}

		workers.start(func() {
			defer handleCancel()
			if streamSend != nil {
				defer func() {
					for message := range streamSend {
						releaseStreamMessage(message)
					}
				}()
			}
			// Allocate the stream batch only when a large hybrid message actually
			// selects it; a small-message-only connection retains DATAGRAM's
			// bounded scratch profile.
			var writeBatchStorage []byte
			writeUserBatch := func(
				firstMessage []byte,
			) (receiveOpen bool, pendingMessage []byte, succeeded bool) {
				var messageStorage [connectH3WriteBatchMaxMessageCount][]byte
				messages := messageStorage[:1]
				messages[0] = firstMessage
				batchByteCount := len(firstMessage) + 4
				receiveOpen = true
			drainReady:
				for len(messages) < cap(messages) {
					select {
					case <-handleCtx.Done():
						receiveOpen = false
						break drainReady
					case message, ok := <-streamInput:
						if !ok {
							receiveOpen = false
							break drainReady
						}
						framedByteCount := len(message) + 4
						if connectH3WriteBatchMaxByteCount < batchByteCount+framedByteCount {
							pendingMessage = message
							break drainReady
						}
						messages = append(messages, message)
						batchByteCount += framedByteCount
					default:
						break drainReady
					}
				}

				if writeBatchStorage == nil {
					writeBatchStorage = make([]byte, connectH3WriteBatchMaxByteCount)
				}
				stream.SetWriteDeadline(time.Now().Add(self.settings.WriteTimeout))
				err := framer.WriteBatchWithStorage(
					stream,
					messages,
					writeBatchStorage,
				)
				if err == nil {
					for _, message := range messages {
						announce.SendMessage(ByteCount(len(message)))
						if useH3Datagrams {
							self.settings.H3DatagramStats.RecordStreamSent(len(message))
						}
					}
				}
				for _, message := range messages {
					releaseStreamMessage(message)
				}
				if err != nil {
					if glog.V(2) {
						glog.Infof("[ts]h3 err = %s\n", err)
					}
					return receiveOpen, pendingMessage, false
				}
				if glog.V(2) {
					glog.Infof("[ts] ->%s batch=%d\n", clientId, len(messages))
				}
				return receiveOpen, pendingMessage, true
			}

			var pendingMessage []byte
			pingTimer := time.NewTimer(0)
			defer pingTimer.Stop()
			resetPingTimer := func() {
				pingTimer.Reset(max(
					self.settings.MinPingTimeout,
					pingTracker.MinPingTimeout(),
				))
			}
			resetPingTimer()
			defer func() {
				if pendingMessage != nil {
					releaseStreamMessage(pendingMessage)
				}
			}()
			for {
				message := pendingMessage
				pendingMessage = nil
				if message == nil {
					select {
					case <-handleCtx.Done():
						return
					case nextMessage, ok := <-streamInput:
						if !ok {
							return
						}
						message = nextMessage
					case <-pingTimer.C:
						stream.SetWriteDeadline(time.Now().Add(self.settings.WriteTimeout))
						err := framer.Write(stream, make([]byte, 0))
						if err != nil {
							glog.Infof("[ts]err = %s\n", err)
							return
						}
						announce.SendMessage(0)
						resetPingTimer()
						continue
					}
				}
				receiveOpen, nextMessage, succeeded := writeUserBatch(message)
				pendingMessage = nextMessage
				if !succeeded || !receiveOpen {
					return
				}
				resetPingTimer()
			}
		})

		if useH3Datagrams {
			workers.start(func() {
				defer handleCancel()
				defer close(streamSend)
				offerStream := func(message []byte) bool {
					retainedByteCount := connect.H3HybridStreamRetainedByteCount(message)
					if streamSendBudget.MaxByteCount() < retainedByteCount &&
						len(message) <= streamSendBudget.MaxByteCount()-connect.MessagePoolMetaByteCount {
						compactMessage := connect.MessagePoolCopy(message)
						connect.MessagePoolReturn(message)
						message = compactMessage
						retainedByteCount = connect.H3HybridStreamRetainedByteCount(message)
					}
					if !streamSendBudget.Acquire(handleCtx, retainedByteCount) {
						connect.MessagePoolReturn(message)
						if handleCtx.Err() == nil && glog.V(2) {
							glog.Infof(
								"[ts]H3 hybrid stream message retained bytes %d exceed queue limit %d\n",
								retainedByteCount,
								streamSendBudget.MaxByteCount(),
							)
						}
						return false
					}
					select {
					case <-handleCtx.Done():
						streamSendBudget.Release(retainedByteCount)
						connect.MessagePoolReturn(message)
						return false
					case streamSend <- message:
						return true
					}
				}
				for {
					select {
					case <-handleCtx.Done():
						return
					case message, ok := <-residentTransport.receive:
						if !ok {
							return
						}
						useDatagram := self.settings.H3DatagramSettings.UseDatagramForPath(
							len(message),
							maxDatagramByteCount,
						)
						if useDatagram {
							useStream, sendErr := sendDatagramMessage(message)
							if sendErr != nil {
								connect.MessagePoolReturn(message)
								if glog.V(2) {
									glog.Infof("[ts]H3 DATAGRAM error = %s\n", sendErr)
								}
								return
							}
							if !useStream {
								announce.SendMessage(ByteCount(len(message)))
								connect.MessagePoolReturn(message)
								continue
							}
						}
						if !offerStream(message) {
							return
						}
					}
				}
			})
		}

		select {
		case <-handleCtx.Done():
			return
		case <-residentTransport.Done():
			return
		}
	}
	if glog.V(2) {
		server.Trace(
			fmt.Sprintf("[rt]connect %s", clientId),
			c,
		)
	} else {
		c()
	}
	return nil
}
