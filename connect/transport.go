package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	// "os"
	"strings"
	"time"
	// "runtime/debug"
	"encoding/binary"
	mathrand "math/rand"
	"strconv"

	"github.com/gorilla/websocket"
	quic "github.com/quic-go/quic-go"

	"github.com/golang/glog"
	"github.com/prometheus/client_golang/prometheus"

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

var serviceTransitionTime = time.Now().Add(30 * time.Second)

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
		MaximumExchangeMessageByteCount: ByteCount(4 * 1024 * 1024),

		QuicConnectTimeout:   15 * time.Second,
		QuicHandshakeTimeout: 15 * time.Second,

		ListenH3Port: 443,
		// FIXME use a different port and DNAT 53->(different port) from the routers
		ListenDnsPort:        53,
		EnableProxyProtocol:  true,
		FramerSettings:       connect.DefaultFramerSettings(),
		TransportTlsSettings: DefaultTransportTlsSettings(),

		ConnectionAnnounceTimeout:   5 * time.Second,
		ConnectionAnnounceSettings:  *DefaultConnectionAnnounceSettings(),
		ConnectionRateLimitSettings: *DefaultConnectionRateLimitSettings(),
	}
}

type ConnectHandlerSettings struct {
	MinPingTimeout                  time.Duration
	MaxPingTimeout                  time.Duration
	PingTrackerCount                int
	WriteTimeout                    time.Duration
	ReadTimeout                     time.Duration
	MaximumExchangeMessageByteCount ByteCount
	QuicConnectTimeout              time.Duration
	QuicHandshakeTimeout            time.Duration
	ListenH3Port                    int
	ListenDnsPort                   int
	EnableProxyProtocol             bool
	FramerSettings                  *connect.FramerSettings
	TransportTlsSettings            *TransportTlsSettings
	ConnectionAnnounceTimeout       time.Duration
	ConnectionAnnounceSettings
	ConnectionRateLimitSettings
}

type ConnectHandler struct {
	ctx       context.Context
	cancel    context.CancelFunc
	handlerId server.Id
	exchange  *Exchange
	settings  *ConnectHandlerSettings

	transportTls *TransportTls
}

func NewConnectHandlerWithDefaults(ctx context.Context, handlerId server.Id, exchange *Exchange) *ConnectHandler {
	return NewConnectHandler(ctx, handlerId, exchange, DefaultConnectHandlerSettings())
}

func NewConnectHandler(ctx context.Context, handlerId server.Id, exchange *Exchange, settings *ConnectHandlerSettings) *ConnectHandler {
	cancelCtx, cancel := context.WithCancel(ctx)

	transportTls, err := NewTransportTlsFromConfig(settings.TransportTlsSettings)
	if err != nil {
		glog.Errorf("[c]Could not initialize tls config. Disabling transport. = %s\n", err)
		transportTls = NewTransportTls(map[string]bool{}, DefaultTransportTlsSettings())
	}

	h := &ConnectHandler{
		ctx:          cancelCtx,
		cancel:       cancel,
		handlerId:    handlerId,
		exchange:     exchange,
		settings:     settings,
		transportTls: transportTls,
	}

	go h.run()

	return h
}

func (self *ConnectHandler) run() {
	defer self.cancel()

	if server.HasPort(self.settings.ListenH3Port) {
		go self.runH3()
	}
	if server.HasPort(self.settings.ListenDnsPort) {
		go self.runH3Dns()
	}

	select {
	case <-self.ctx.Done():
	}
}

func (self *ConnectHandler) Connect(w http.ResponseWriter, r *http.Request) {
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
	defer handleCancel()

	connectedGauge.Add(1)
	defer connectedGauge.Sub(1)

	// find the client ip:port from the request header
	// `X-Forwarded-For` is added by the warp lb
	clientAddress := r.Header.Get("X-UR-Forwarded-For")
	if clientAddress == "" {
		clientAddress = r.Header.Get("X-Forwarded-For")
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
		glog.V(1).Infof("[t]rate limit err = %s\n", err)
		return
	}

	// attemp to parse the auth message from the header
	// if that fails, expect the auth message as the first message
	auth, transportVersion := func() (*protocol.Auth, int) {
		glog.V(2).Infof("[c]header: %v\n", r.Header)

		headerAuth := r.Header.Get("Authorization")
		headerAppVersion := r.Header.Get("X-UR-AppVersion")
		headerInstanceId := r.Header.Get("X-UR-InstanceId")
		headerTransportVersion := r.Header.Get("X-UR-TransportVersion")

		transportVersion := 0
		if i, err := strconv.Atoi(headerTransportVersion); err == nil {
			transportVersion = i
		}

		bearerPrefix := "Bearer "

		if strings.HasPrefix(headerAuth, bearerPrefix) {
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

	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer ws.Close()

	// enforce the message size limit on messages in
	ws.SetReadLimit(self.settings.MaximumExchangeMessageByteCount)

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

	byJwt, err := jwt.ParseByJwt(auth.ByJwt)
	if err != nil {
		glog.Infof("[t]auth jwt err = %s\n", err)
		return
	}

	if byJwt.ClientId == nil {
		return
	}

	clientId := *byJwt.ClientId

	instanceId, err := server.IdFromBytes(auth.InstanceId)
	if err != nil {
		return
	}

	// verify the client is still part of the network
	// this will fail for example if the client has been removed
	client := model.GetNetworkClient(handleCtx, clientId)
	if client == nil || client.NetworkId != byJwt.NetworkId {
		// server.Logger("ERROR HB\n")
		return
	}

	connectionId := server.NewId()
	self.exchange.registerConnection(clientId, connectionId, handleCancel)
	defer self.exchange.unregisterConnection(clientId, connectionId)

	c := func() {
		announceTimeout := time.Duration(0)
		if serviceTransitionTime.Before(time.Now()) {
			// the service has transitioned all the connections from the old to new
			// now we delay the announcement to make sure the transport is stable
			announceTimeout = self.settings.ConnectionAnnounceTimeout
		}
		var testConfig *TestConfig
		if transportVersion < 2 {
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
		defer announce.Close()

		residentTransport := NewResidentTransport(
			handleCtx,
			self.exchange,
			clientId,
			instanceId,
		)
		go func() {
			defer handleCancel()
			server.HandleError(residentTransport.Run)
			// close is done in the write
		}()

		pingTracker := NewPingTracker(self.settings.PingTrackerCount)

		go server.HandleError(func() {
			defer func() {
				handleCancel()
				residentTransport.Close()
			}()

			var speedTest *SpeedTest

			for {
				ws.SetReadDeadline(time.Now().Add(self.settings.ReadTimeout))
				messageType, message, err := ws.ReadMessage()
				if err != nil {
					// glog.Errorf("[t]read err = %s\n", err)
					if connectionId := announce.ConnectionId(); connectionId != nil {
						model.ClientError(handleCtx, client.NetworkId, client.ClientId, *connectionId, "read", err)
					}
					return
				}

				switch messageType {
				case websocket.BinaryMessage:
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
						continue
					}
					if speedTest != nil {
						speedTest.TotalByteCount += model.ByteCount(len(message))
						continue
					}

					pingTracker.Receive()

					select {
					case <-handleCtx.Done():
						return
					case <-residentTransport.Done():
						return
					case residentTransport.send <- message:
						glog.V(2).Infof("[rtr] <-%s\n", clientId)
						// case <-time.After(self.settings.ReadTimeout):
					}
					// else ignore
				}
			}
		}, handleCancel)

		go server.HandleError(func() {
			defer handleCancel()

			write := func(message []byte) error {
				ws.SetWriteDeadline(time.Now().Add(self.settings.WriteTimeout))
				err := ws.WriteMessage(websocket.BinaryMessage, message)
				if err != nil {
					// note that for websocket a deadline timeout cannot be recovered
					if connectionId := announce.ConnectionId(); connectionId != nil {
						model.ClientError(handleCtx, client.NetworkId, client.ClientId, *connectionId, "write", err)
					}
					return err
				}
				// reliability tracking
				announce.SendMessage(ByteCount(len(message)))
				glog.V(2).Infof("[ts] ->%s\n", clientId)
				return nil
			}

			for {
				select {
				case <-handleCtx.Done():
					return
				case <-residentTransport.Done():
					return
				case message, ok := <-residentTransport.receive:
					if !ok {
						return
					}

					if len(message) <= 16 {
						glog.Infof("[rts]send message must be >16 bytes (%d)\n", len(message))
					} else if write(message) != nil {
						return
					}

				case <-time.After(max(self.settings.MinPingTimeout, pingTracker.MinPingTimeout())):
					ws.SetWriteDeadline(time.Now().Add(self.settings.WriteTimeout))
					err := ws.WriteMessage(websocket.BinaryMessage, make([]byte, 0))
					if err != nil {
						// note that for websocket a dealine timeout cannot be recovered
						if connectionId := announce.ConnectionId(); connectionId != nil {
							model.ClientError(handleCtx, client.NetworkId, client.ClientId, *connectionId, "write", err)
						}
						return
					}
					// reliability tracking
					announce.SendMessage(0)
				case latencyTest := <-announce.PendingLatencyTest:
					if announce.SendLatency(latencyTest) {
						message := latencyTest.TestId.Bytes()
						ws.SetWriteDeadline(time.Now().Add(self.settings.WriteTimeout))
						err := ws.WriteMessage(websocket.BinaryMessage, message)
						if err != nil {
							// note that for websocket a dealine timeout cannot be recovered
							if connectionId := announce.ConnectionId(); connectionId != nil {
								model.ClientError(handleCtx, client.NetworkId, client.ClientId, *connectionId, "write", err)
							}
							return
						}
						// reliability tracking
						announce.SendMessage(model.ByteCount(len(message)))
					}
				case speedTest := <-announce.PendingSpeedTest:
					// client should echo control values and packets in speed test mode

					if announce.SendSpeed(speedTest) {
						controlMessage := make([]byte, 5)
						binary.BigEndian.PutUint32(controlMessage[1:5], speedTest.TestId)
						controlMessage[0] = connect.TransportControlSpeedStart
						if write(controlMessage) != nil {
							return
						}
						// read data size amount of speed test and time limit of speed test
						chunk := make([]byte, 1024)
						for range (speedTest.TotalByteCount + model.ByteCount(len(chunk)-1)) / model.ByteCount(len(chunk)) {
							mathrand.Read(chunk)
							if write(chunk) != nil {
								return
							}
						}
						controlMessage[0] = connect.TransportControlSpeedStop
						if write(controlMessage) != nil {
							return
						}
					}

				}
			}
		}, handleCancel)

		select {
		case <-handleCtx.Done():
			return
		case <-residentTransport.Done():
			glog.Infof("[t]resident transport done\n")
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

func (self *ConnectHandler) listenQuic(port int, connTransform func(net.PacketConn) (net.PacketConn, error)) {
	handleCtx, handleCancel := context.WithCancel(self.ctx)

	defer handleCancel()

	quicConfig := &quic.Config{
		HandshakeIdleTimeout:    self.settings.QuicConnectTimeout + self.settings.QuicHandshakeTimeout,
		MaxIdleTimeout:          self.settings.MaxPingTimeout * 4,
		KeepAlivePeriod:         0,
		Allow0RTT:               true,
		DisablePathMTUDiscovery: true,
		InitialPacketSize:       1400,
	}

	// type clientConfig struct {
	// 	tlsConfig *tls.Config
	// 	err error
	// }
	// clientConfigs := map[string]*clientConfig{}

	tlsConfig := &tls.Config{
		GetConfigForClient: self.transportTls.GetTlsConfigForClient,
	}

	listenIpv4, _, listenPort := server.RequireListenIpPort(port)
	// listenIp := net.ParseIP(listenIpv4)
	// if listenIp == nil {
	// 	return
	// }

	glog.V(2).Infof("[c]h3 listen %s:%d\n", listenIpv4, listenPort)

	// serverAddr := &net.UDPAddr{
	// 	IP:   listenIp,
	// 	Port: listenPort,
	// }

	reusePort := false

	listenConfig := net.ListenConfig{}
	if reusePort {
		listenConfig.Control = server.SoReusePort
	}

	serverConn, err := listenConfig.ListenPacket(
		handleCtx,
		"udp",
		net.JoinHostPort(listenIpv4, strconv.Itoa(listenPort)),
	)
	if err != nil {
		return
	}
	defer serverConn.Close()
	packetConn, err := connTransform(serverConn)
	if err != nil {
		return
	}
	defer packetConn.Close()
	listener, err := (&quic.Transport{
		Conn: packetConn,
		// createdConn: true,
		// isSingleUse: true,
	}).ListenEarly(tlsConfig, quicConfig)
	defer listener.Close()

	for {
		glog.V(2).Infof("[c]h3 wait to accept connection %s:%d\n", listenIpv4, listenPort)
		conn, err := listener.Accept(handleCtx)
		if err != nil {
			glog.Infof("[c]h3 accept connection %s:%d err = %s\n", listenIpv4, listenPort, err)
			return
		}

		glog.Infof("[c]h3 accept connection %s:%d\n", listenIpv4, listenPort)
		go func() {
			defer conn.CloseWithError(0, "")

			err := self.connectQuic(conn)
			if err != nil {
				glog.Infof("[c]h3 connection exited %s:%d err = %s\n", listenIpv4, listenPort, err)
			} else {
				glog.Infof("[c]h3 connection exited %s:%d\n", listenIpv4, listenPort)
			}
		}()
	}
}

func (self *ConnectHandler) connectQuic(conn *quic.Conn) error {
	handleCtx, handleCancel := context.WithCancel(self.ctx)
	defer handleCancel()

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

	stream.SetReadDeadline(time.Now().Add(self.settings.ReadTimeout))
	authFrameBytes, err := framer.Read(stream)
	if err != nil {
		return err
	}

	message, err := connect.DecodeFrame(authFrameBytes)
	if err != nil {
		return err
	}
	auth, ok := message.(*protocol.Auth)
	if !ok {
		return err
	}

	byJwt, err := jwt.ParseByJwt(auth.ByJwt)
	if err != nil {
		return err
	}

	if byJwt.ClientId == nil {
		return fmt.Errorf("Missing client id.")
	}

	clientId := *byJwt.ClientId

	instanceId, err := server.IdFromBytes(auth.InstanceId)
	if err != nil {
		return err
	}

	// verify the client is still part of the network
	// this will fail for example if the client has been removed
	client := model.GetNetworkClient(handleCtx, clientId)
	if client == nil || client.NetworkId != byJwt.NetworkId {
		// server.Logger("ERROR HB\n")
		return fmt.Errorf("Client id is not part of network.")
	}

	connectionId := server.NewId()
	self.exchange.registerConnection(clientId, connectionId, handleCancel)
	defer self.exchange.unregisterConnection(clientId, connectionId)

	stream.SetWriteDeadline(time.Now().Add(self.settings.WriteTimeout))
	err = framer.Write(stream, authFrameBytes)
	if err != nil {
		// server.Logger("TIMEOUT HC\n")
		return err
	}

	c := func() {
		announceTimeout := time.Duration(0)
		if serviceTransitionTime.Before(time.Now()) {
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
		defer announce.Close()

		residentTransport := NewResidentTransport(
			handleCtx,
			self.exchange,
			clientId,
			instanceId,
		)
		go func() {
			defer handleCancel()
			server.HandleError(residentTransport.Run)
			// close is done in the write
		}()

		pingTracker := NewPingTracker(self.settings.PingTrackerCount)

		go server.HandleError(func() {
			defer func() {
				handleCancel()
				residentTransport.Close()
			}()

			for {
				stream.SetReadDeadline(time.Now().Add(self.settings.ReadTimeout))
				message, err := framer.Read(stream)
				if err != nil {
					glog.V(2).Infof("[tr]h3 err = %s\n", err)
					return
				}

				// reliability tracking
				announce.ReceiveMessage(ByteCount(len(message)))

				if 0 == len(message) {
					// ping
					pingTracker.ReceivePing()
					continue
				}

				pingTracker.Receive()

				select {
				case <-handleCtx.Done():
					return
				case <-residentTransport.Done():
					return
				case residentTransport.send <- message:
					glog.V(2).Infof("[rtr] <-%s\n", clientId)
				case <-time.After(self.settings.ReadTimeout):
				}
			}
		}, handleCancel)

		go server.HandleError(func() {
			defer handleCancel()

			for {
				select {
				case <-handleCtx.Done():
					return
				case <-residentTransport.Done():
					return
				case message, ok := <-residentTransport.receive:
					if !ok {
						return
					}

					stream.SetWriteDeadline(time.Now().Add(self.settings.WriteTimeout))
					err := framer.Write(stream, message)
					if err != nil {
						glog.V(2).Infof("[ts]h3 err = %s\n", err)
						return
					}
					// reliability tracking
					announce.SendMessage(ByteCount(len(message)))
					glog.V(2).Infof("[ts] ->%s\n", clientId)
				case <-time.After(max(self.settings.MinPingTimeout, pingTracker.MinPingTimeout())):
					stream.SetWriteDeadline(time.Now().Add(self.settings.WriteTimeout))
					err := framer.Write(stream, make([]byte, 0))
					if err != nil {
						glog.Infof("[ts]err = %s\n", err)
						return
					}
					// reliability tracking
					announce.SendMessage(0)
				}
			}
		}, handleCancel)

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
