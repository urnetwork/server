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

	"github.com/gorilla/websocket"
	quic "github.com/quic-go/quic-go"

	"github.com/golang/glog"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/connect/v2025"
	"github.com/urnetwork/connect/v2025/protocol"
	"github.com/urnetwork/server/v2025"
	"github.com/urnetwork/server/v2025/controller"
	"github.com/urnetwork/server/v2025/jwt"
	"github.com/urnetwork/server/v2025/model"
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

func init() {
	prometheus.MustRegister(connectedGauge)
}

func DefaultConnectHandlerSettings() *ConnectHandlerSettings {
	platformTransportSettings := connect.DefaultPlatformTransportSettings()
	return &ConnectHandlerSettings{
		// use the min value from older version of the client
		// `platformTransportSettings.PingTimeout`
		MinPingTimeout:        1 * time.Second,
		PingTrackerCount:      4,
		WriteTimeout:          platformTransportSettings.WriteTimeout,
		ReadTimeout:           platformTransportSettings.ReadTimeout,
		SyncConnectionTimeout: 60 * time.Second,

		// a single exchange message size is encoded as an `int32`
		// because message must be serialized/deserialized from memory,
		// there is a global limit on the size per message
		// messages above this size will be ignored from clients and the exchange
		MaximumExchangeMessageByteCount: ByteCount(4 * 1024 * 1024),

		QuicConnectTimeout:   2 * time.Second,
		QuicHandshakeTimeout: 2 * time.Second,

		ListenH3Port:         443,
		ListenDnsPort:        53,
		EnableProxyProtocol:  true,
		FramerSettings:       connect.DefaultFramerSettings(),
		TransportTlsSettings: DefaultTransportTlsSettings(),
	}
}

type ConnectHandlerSettings struct {
	MinPingTimeout                  time.Duration
	PingTrackerCount                int
	WriteTimeout                    time.Duration
	ReadTimeout                     time.Duration
	SyncConnectionTimeout           time.Duration
	MaximumExchangeMessageByteCount ByteCount
	QuicConnectTimeout              time.Duration
	QuicHandshakeTimeout            time.Duration
	ListenH3Port                    int
	ListenDnsPort                   int
	EnableProxyProtocol             bool
	FramerSettings                  *connect.FramerSettings
	TransportTlsSettings            *TransportTlsSettings
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

	go self.runH3()
	go self.runH3Dns()

	select {
	case <-self.ctx.Done():
	}
}

func (self *ConnectHandler) Connect(w http.ResponseWriter, r *http.Request) {
	connectedGauge.Add(1)
	defer connectedGauge.Sub(1)

	// attemp to parse the auth message from the header
	// if that fails, expect the auth message as the first message
	auth := func() *protocol.Auth {
		glog.V(2).Infof("[c]header: %v\n", r.Header)

		headerAuth := r.Header.Get("Authorization")
		headerAppVersion := r.Header.Get("X-UR-AppVersion")
		headerInstanceId := r.Header.Get("X-UR-InstanceId")

		bearerPrefix := "Bearer "

		if strings.HasPrefix(headerAuth, bearerPrefix) {
			jwt := headerAuth[len(bearerPrefix):]

			instanceId, err := server.ParseId(headerInstanceId)
			if err == nil {
				return &protocol.Auth{
					ByJwt:      jwt,
					InstanceId: instanceId.Bytes(),
					AppVersion: headerAppVersion,
				}
			} else {
				glog.Infof("[c]Bad header X-UR-InstanceId: %s\n", headerInstanceId)
			}
		}

		return nil
	}()

	handleCtx, handleCancel := context.WithCancel(self.ctx)
	defer handleCancel()

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

	c := func() {
		connectionId := controller.ConnectNetworkClient(handleCtx, clientId, clientAddress, self.handlerId)
		defer server.HandleError(func() {
			model.DisconnectNetworkClient(self.ctx, connectionId)
		})

		go server.HandleError(func() {
			// disconnect the client if the model marks the connection closed
			defer handleCancel()

			for {
				select {
				case <-handleCtx.Done():
					return
				case <-time.After(self.settings.SyncConnectionTimeout):
				}

				if !model.IsNetworkClientConnected(handleCtx, connectionId) {
					// client kicked off
					return
				}
			}
		}, handleCancel)

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
				ws.SetReadDeadline(time.Now().Add(self.settings.ReadTimeout))
				messageType, message, err := ws.ReadMessage()
				if err != nil {
					return
				}

				switch messageType {
				case websocket.BinaryMessage:
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
						glog.V(2).Infof("[rtr] <-%s\n", clientId.String())
					case <-time.After(self.settings.ReadTimeout):
					}
					// else ignore
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

					ws.SetWriteDeadline(time.Now().Add(self.settings.WriteTimeout))
					if err := ws.WriteMessage(websocket.BinaryMessage, message); err != nil {
						// note that for websocket a deadline timeout cannot be recovered
						return
					}
					glog.V(2).Infof("[ts] ->%s\n", clientId.String())
				case <-time.After(max(self.settings.MinPingTimeout, pingTracker.MinPingTimeout())):
					ws.SetWriteDeadline(time.Now().Add(self.settings.WriteTimeout))
					if err := ws.WriteMessage(websocket.BinaryMessage, make([]byte, 0)); err != nil {
						// note that for websocket a dealine timeout cannot be recovered
						return
					}
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
			fmt.Sprintf("[t]connect %s", clientId.String()),
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
		HandshakeIdleTimeout: self.settings.QuicConnectTimeout + self.settings.QuicHandshakeTimeout,
		MaxIdleTimeout:       self.settings.MinPingTimeout * 2,
		KeepAlivePeriod:      0,
		Allow0RTT:            true,
	}

	// type clientConfig struct {
	// 	tlsConfig *tls.Config
	// 	err error
	// }
	// clientConfigs := map[string]*clientConfig{}

	tlsConfig := &tls.Config{
		GetConfigForClient: self.transportTls.GetTlsConfigForClient,
	}

	glog.V(2).Infof("[c]h3 listen :%d\n", port)

	serverAddr := &net.UDPAddr{
		IP:   net.IPv4zero,
		Port: port,
	}

	serverConn, err := net.ListenUDP("udp", serverAddr)
	if err != nil {
		return
	}
	packetConn, err := connTransform(serverConn)
	if err != nil {
		return
	}
	defer packetConn.Close()
	earlyListener, err := (&quic.Transport{
		Conn: packetConn,
		// createdConn: true,
		// isSingleUse: true,
	}).ListenEarly(tlsConfig, quicConfig)
	defer earlyListener.Close()

	for {
		glog.V(2).Infof("[c]h3 wait to accept connection :%d\n", port)
		earlyConn, err := earlyListener.Accept(handleCtx)
		if err != nil {
			glog.Infof("[c]h3 accept connection :%d err = %s\n", port, err)
			return
		}

		glog.V(2).Infof("[c]h3 accept connection :%d\n", port)
		go func() {
			err := self.connectQuic(earlyConn)
			if err != nil {
				glog.V(2).Infof("[c]h3 connection exited :%d err = %s\n", port, err)
			} else {
				glog.V(2).Infof("[c]h3 connection exited :%d\n", port)
			}
		}()
	}
}

func (self *ConnectHandler) connectQuic(earlyConn quic.EarlyConnection) error {

	handleCtx, handleCancel := context.WithCancel(self.ctx)
	defer handleCancel()

	stream, err := earlyConn.AcceptStream(handleCtx)
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

	stream.SetWriteDeadline(time.Now().Add(self.settings.WriteTimeout))
	err = framer.Write(stream, authFrameBytes)
	if err != nil {
		// server.Logger("TIMEOUT HC\n")
		return err
	}

	// find the client ip:port from the addr
	clientAddress := earlyConn.RemoteAddr().String()

	c := func() {
		connectionId := controller.ConnectNetworkClient(handleCtx, clientId, clientAddress, self.handlerId)
		defer server.HandleError(func() {
			model.DisconnectNetworkClient(self.ctx, connectionId)
		})

		go server.HandleError(func() {
			// disconnect the client if the model marks the connection closed
			defer handleCancel()

			for {
				select {
				case <-handleCtx.Done():
					return
				case <-time.After(self.settings.SyncConnectionTimeout):
				}

				if !model.IsNetworkClientConnected(handleCtx, connectionId) {
					// client kicked off
					return
				}
			}
		}, handleCancel)

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
					glog.V(2).Infof("[rtr] <-%s\n", clientId.String())
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
					glog.V(2).Infof("[ts] ->%s\n", clientId.String())
				case <-time.After(max(self.settings.MinPingTimeout, pingTracker.MinPingTimeout())):
					stream.SetWriteDeadline(time.Now().Add(self.settings.WriteTimeout))
					err := framer.Write(stream, make([]byte, 0))
					if err != nil {
						glog.Infof("[ts]err = %s\n", err)
						return
					}
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
			fmt.Sprintf("[rt]connect %s", clientId.String()),
			c,
		)
	} else {
		c()
	}
	return nil
}
