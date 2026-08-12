// Connect transports terminate client H1 WebSocket and H3 QUIC connections,
// then expose each connection as one route to its resident client.
package connect

import (
	"bufio"
	"context"
	"crypto/tls"
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
	"github.com/urnetwork/glog"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/server"
	// "github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/model"
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
		// FIXME use a different port and DNAT 53->(different port) from the routers
		ListenDnsPort:       53,
		EnableProxyProtocol: true,
		// Floor the framer at the connect runtime minimum message length: every
		// framer on the resident exchange flow must admit the handshake's TLS
		// server flight (one ~2.2 KiB pack). Also backs the websocket read limit.
		FramerSettings:       connect.DefaultFramerSettings(int(connect.DefaultClientSettings().MinimumMessageLenLimit())),
		TransportTlsSettings: server.DefaultTransportTlsSettings(),

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
	QuicConnectTimeout        time.Duration
	QuicHandshakeTimeout      time.Duration
	ListenH3Port              int
	ListenDnsPort             int
	EnableProxyProtocol       bool
	FramerSettings            *connect.FramerSettings
	TransportTlsSettings      *server.TransportTlsSettings
	ConnectionAnnounceTimeout time.Duration
	// per-connection latency/speed test schedule.
	// nil selects a default based on the transport version.
	ConnectionTestConfig *TestConfig
	ConnectionAnnounceSettings
	ConnectionRateLimitSettings
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
	return &quic.Config{
		HandshakeIdleTimeout: settings.QuicConnectTimeout + settings.QuicHandshakeTimeout,
		MaxIdleTimeout:       settings.MaxPingTimeout * 4,
		KeepAlivePeriod:      0,
		Allow0RTT:            true,
		InitialPacketSize:    1400,
	}
}

type ConnectHandler struct {
	ctx       context.Context
	cancel    context.CancelFunc
	handlerId server.Id
	exchange  *Exchange
	settings  *ConnectHandlerSettings

	transportTls          *server.TransportTls
	serviceTransitionTime time.Time
	h3PacketConn          net.PacketConn
	dnsPacketConn         net.PacketConn

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
		activeCount:           activeCount,
		activeZero:            activeZero,
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

			readTimer := time.NewTimer(0)
			defer readTimer.Stop()
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

					sendResult := residentTransport.sendMessage(
						handleCtx.Done(),
						message,
						readTimer,
						self.settings.ReadTimeout,
					)
					if sendResult == pooledMessageSendDone {
						return
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

			stream.SetWriteDeadline(time.Now().Add(self.settings.WriteTimeout))
			return framer.Write(stream, authFrameBytes)
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

		residentTransport := NewResidentTransport(
			handleCtx,
			self.exchange,
			clientId,
			instanceId,
		)
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

		workers.start(func() {
			defer handleCancel()

			readTimer := time.NewTimer(0)
			defer readTimer.Stop()
			for {
				stream.SetReadDeadline(time.Now().Add(self.settings.ReadTimeout))
				message, err := framer.Read(stream)
				if err != nil {
					if glog.V(2) {
						glog.Infof("[tr]h3 err = %s\n", err)
					}
					return
				}

				// reliability tracking
				announce.ReceiveMessage(ByteCount(len(message)))

				if 0 == len(message) {
					// ping
					pingTracker.ReceivePing()
					connect.MessagePoolReturn(message)
					continue
				}

				pingTracker.Receive()

				sendResult := residentTransport.sendMessage(
					handleCtx.Done(),
					message,
					readTimer,
					self.settings.ReadTimeout,
				)
				if sendResult == pooledMessageSendDone {
					return
				}
				if sendResult == pooledMessageSendDelivered {
					if glog.V(2) {
						glog.Infof("[rtr] <-%s\n", clientId)
					}
				}
			}
		})

		workers.start(func() {
			defer handleCancel()

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
					case message, ok := <-residentTransport.receive:
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

				stream.SetWriteDeadline(time.Now().Add(self.settings.WriteTimeout))
				err := framer.WriteBatch(stream, messages)
				if err == nil {
					for _, message := range messages {
						announce.SendMessage(ByteCount(len(message)))
					}
				}
				for _, message := range messages {
					connect.MessagePoolReturn(message)
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
			defer func() {
				if pendingMessage != nil {
					connect.MessagePoolReturn(pendingMessage)
				}
			}()
			for {
				message := pendingMessage
				pendingMessage = nil
				if message == nil {
					select {
					case <-handleCtx.Done():
						return
					case nextMessage, ok := <-residentTransport.receive:
						if !ok {
							return
						}
						message = nextMessage
					case <-time.After(max(self.settings.MinPingTimeout, pingTracker.MinPingTimeout())):
						stream.SetWriteDeadline(time.Now().Add(self.settings.WriteTimeout))
						err := framer.Write(stream, make([]byte, 0))
						if err != nil {
							glog.Infof("[ts]err = %s\n", err)
							return
						}
						announce.SendMessage(0)
						continue
					}
				}
				receiveOpen, nextMessage, succeeded := writeUserBatch(message)
				pendingMessage = nextMessage
				if !succeeded || !receiveOpen {
					return
				}
			}
		})

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
