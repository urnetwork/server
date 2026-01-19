package main

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	// mathrand "math/rand"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/stripe/goproxy"
	"github.com/things-go/go-socks5"
	// "github.com/things-go/go-socks5/statute"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/glog/v2026"
	// "github.com/urnetwork/proxy/v2026"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// users authenticate with the proxy network using a signed proxy id,
// which is set as either:
// - the username for username/password authentication (socks5 or https basic)
// - the bearer token for bearer authentication (https)
// - the hostname as <signed_proxy_id>.connect.<domain> (https)
// *important* if the client intends to use socks5,
//             or does not support https auth and does not support tls ech (e.g. older browsers),
//             the proxy should be locked to known ip subnets,
//             since the signed proxy id will be exposed in plaintext packets
// The proxy connect creates a userspace network per proxy id,
// and multiplexes incoming socks5 and https proxy request to the appropriate userspace network.
// The userspace network sends/received raw packets to an exchange resident
// via a `ResidentTransport`, which route to the `ResidentProxyDevice` inside the resident.
// When the resident is created, it is configured according to its `model.ProxyDevice`
// as either a normal resident or a proxy device resident.

func DefaultProxyConnectHandlerSettings() *ProxyConnectHandlerSettings {
	// connectHandlerSettings := DefaultConnectHandlerSettings()
	return &ProxyConnectHandlerSettings{
		// WriteTimeout:               connectHandlerSettings.WriteTimeout,
		// ReadTimeout:                connectHandlerSettings.ReadTimeout,
		ProxyConnectionIdleTimeout: 15 * time.Minute,
		ListenSocksPort:            1080,
		ListenHttpsPort:            444,
		FramerSettings:             connect.DefaultFramerSettings(),
		EnableProxyProtocol:        true,
		// ProxyConnectTimeout:        15 * time.Second,

		TransportTlsSettings: DefaultTransportTlsSettings(),
	}
}

type ProxyConnectHandlerSettings struct {
	// WriteTimeout               time.Duration
	// ReadTimeout                time.Duration
	ProxyConnectionIdleTimeout time.Duration
	ListenSocksPort            int
	ListenHttpsPort            int
	FramerSettings             *connect.FramerSettings
	EnableProxyProtocol        bool
	// ProxyConnectTimeout        time.Duration
	// Mtu                        int

	TransportTlsSettings *TransportTlsSettings
}

type ProxyConnectHandler struct {
	ctx          context.Context
	cancel       context.CancelFunc
	handlerId    server.Id
	exchange     *Exchange
	transportTls *TransportTls
	settings     *ProxyConnectHandlerSettings

	// stateLock        sync.Mutex
	// proxyConnections map[server.Id]*ProxyConnection
}

func NewProxyConnectHandlerWithDefaults(
	ctx context.Context,
	handlerId server.Id,
	exchange *Exchange,
) *ProxyConnectHandler {
	return NewProxyConnectHandler(
		ctx,
		handlerId,
		exchange,
		DefaultProxyConnectHandlerSettings(),
	)
}

func NewProxyConnectHandler(
	ctx context.Context,
	handlerId server.Id,
	exchange *Exchange,
	settings *ProxyConnectHandlerSettings) *ProxyConnectHandler {
	cancelCtx, cancel := context.WithCancel(ctx)

	transportTls, err := NewTransportTlsFromConfig(settings.TransportTlsSettings)
	if err != nil {
		glog.Errorf("[c]Could not initialize tls config. Disabling transport. = %s\n", err)
		transportTls = NewTransportTls(map[string]bool{}, DefaultTransportTlsSettings())
	}

	h := &ProxyConnectHandler{
		ctx:          cancelCtx,
		cancel:       cancel,
		handlerId:    handlerId,
		exchange:     exchange,
		transportTls: transportTls,
		settings:     settings,
		// proxyConnections: map[server.Id]*ProxyConnection{},
	}

	if server.HasPort(settings.ListenHttpsPort) {
		go server.HandleError(h.runHttps)
	}

	if server.HasPort(settings.ListenSocksPort) {
		go server.HandleError(h.runSocks)
	}

	return h
}

func (self *ProxyConnectHandler) connectProxyForRequest(r *http.Request, tunDial TunDial) (proxyConnection *ProxyConnection, returnErr error) {
	glog.Infof("[tp]found headers: %v\n", r.Header)
	host := r.Header.Get("X-Forwarded-Host")
	if host == "" {
		host = r.Header.Get("Host")
	}
	if host == "" && r.TLS != nil {
		host = r.TLS.ServerName
	}
	if host == "" {
		returnErr = fmt.Errorf("Unknown host")
		return
	}
	glog.Infof("[tp]connect with host=%s\n", host)

	hostProxyId := strings.SplitN(host, ".", 2)[0]
	proxyId, err := model.ParseSignedProxyId(hostProxyId)
	if err != nil {
		// validate the proxy id in the host with the authorization header
		// the signed proxy id can be provided as either:
		// - Bearer <signed_proxy_id>
		// - Basic base64(<signed_proxy_id>:) (the signed proxy id is the username)
		headerAuth := r.Header.Get("Proxy-Authorization")
		if headerAuth == "" {
			headerAuth = r.Header.Get("Authorization")
		}

		bearerPrefix := "bearer "
		basicPrefix := "basic "

		if len(bearerPrefix) < len(headerAuth) && strings.ToLower(headerAuth[:len(bearerPrefix)]) == bearerPrefix {
			signedProxyId := headerAuth[len(bearerPrefix):]
			proxyId, err = model.ParseSignedProxyId(signedProxyId)
			if err != nil {
				returnErr = err
				return
			}
		} else if len(basicPrefix) < len(headerAuth) && strings.ToLower(headerAuth[:len(basicPrefix)]) == basicPrefix {
			// user:pass
			combinedSignedProxyId, err := base64.StdEncoding.DecodeString(headerAuth[len(basicPrefix):])
			if err != nil {
				returnErr = err
				return
			}
			signedProxyId := strings.SplitN(string(combinedSignedProxyId), ":", 2)[0]
			proxyId, err = model.ParseSignedProxyId(signedProxyId)

			if err != nil {
				returnErr = err
				return
			}
		} else {
			returnErr = err
			return
		}
	}
	glog.Infof("[tp]connect with host=%s proxyId=%s\n", host, proxyId)

	proxyConnection, returnErr = self.connectProxy(proxyId, tunDial)
	if returnErr != nil {
		return
	}

	addrPortStr := r.Header.Get("X-Forwarded-For")
	if addrPortStr == "" {
		addrPortStr = r.RemoteAddr
	}
	if addrPortStr == "" {
		returnErr = fmt.Errorf("Unknown peer address")
		return
	}
	glog.Infof("[tp]connect with host=%s proxyId=%s addr=%s\n", host, proxyId, addrPortStr)
	addrPort, err := netip.ParseAddrPort(addrPortStr)
	if err != nil {
		returnErr = err
		return
	}

	if !self.ValidCaller(proxyId, addrPort.Addr()) {
		returnErr = fmt.Errorf("Invalid caller")
		return
	}

	return
}

func (self *ProxyConnectHandler) connectProxy(proxyId server.Id, tunDial TunDial) (*ProxyConnection, error) {
	// self.stateLock.Lock()
	// defer self.stateLock.Unlock()

	proxyConnection, err := NewProxyConnection(self.ctx, self.exchange, tunDial, proxyId, self.settings)
	if err != nil {
		return nil, err
	}
	// go server.HandleError(func() {
	// 	defer proxyConnection.Close()
	// 	proxyConnection.Run()

	// 	glog.V(1).Infof("[rp]close %s\n", proxyId)
	// })
	// go server.HandleError(func() {
	// 	defer proxyConnection.Cancel()
	// 	for {
	// 		if proxyConnection.CancelIfIdle() {
	// 			glog.V(1).Infof("[rp]idle %s\n", proxyId)
	// 			return
	// 		}

	// 		select {
	// 		case <-proxyConnection.Done():
	// 			return
	// 		case <-time.After(self.settings.ProxyConnectionIdleTimeout):
	// 		}
	// 	}
	// })

	return proxyConnection, nil
}

func (self *ProxyConnectHandler) runHttps() {

	httpProxy := goproxy.NewProxyHttpServer()

	// tr := &http.Transport{
	// 	IdleConnTimeout: 300 * time.Second,
	// 	TLSHandshakeTimeout: 30 * time.Second,

	// }
	// httpProxy.Tr = tr

	// httpProxy.AllowHTTP2 = false
	httpProxy.ConnectDialContext = func(proxyCtx *goproxy.ProxyCtx, network string, addr string) (conn net.Conn, err error) {
		return self.connectProxyForRequest(proxyCtx.Req, TunDial{
			Network: network,
			Address: addr,
		})
	}

	// proxyConnection.httpProxy = httpProxy

	addr := fmt.Sprintf(":%d", self.settings.ListenHttpsPort)

	tlsConfig := &tls.Config{
		GetConfigForClient: self.transportTls.GetTlsConfigForClient,
	}

	httpServer := &http.Server{
		Addr:      addr,
		Handler:   httpProxy,
		TLSConfig: tlsConfig,
	}

	listenIpv4, _, listenPort := server.RequireListenIpPort(self.settings.ListenHttpsPort)

	listenConfig := net.ListenConfig{}

	serverConn, err := listenConfig.Listen(
		self.ctx,
		"tcp",
		net.JoinHostPort(listenIpv4, strconv.Itoa(listenPort)),
	)
	if err != nil {
		return
	}
	defer serverConn.Close()

	if self.settings.EnableProxyProtocol {
		serverConn = NewPpServerConn(serverConn, DefaultWarpPpSettings())
	}

	httpServer.ServeTLS(serverConn, "", "")
}

// func (self *ProxyConnectHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
// 	proxyConnection, err := self.connectProxyForRequest(r)
// 	if err != nil {
// 		http.Error(w, "Unauthorized", http.StatusUnauthorized)
// 		return
// 	}

// 	proxyConnection.ServeHTTP(w, r)
// }

func (self *ProxyConnectHandler) runSocks() {
	handleCtx, handleCancel := context.WithCancel(self.ctx)
	defer handleCancel()

	socksServer := socks5.NewServer(
		socks5.WithLogger(self),
		socks5.WithCredential(self),
		// socks5.WithRewriter(self),
		socks5.WithResolver(self),
		socks5.WithRule(socks5.NewPermitConnAndAss()),
		socks5.WithDialAndRequest(func(ctx context.Context, network string, addr string, request *socks5.Request) (net.Conn, error) {
			username := request.AuthContext.Payload["username"]
			// the proxy id was already verified by the credential store
			proxyId := model.RequireEncodedProxyId(username)

			// addrPort, err := netip.ParseAddrPort(addr)
			// if err != nil {
			// 	return nil, err
			// }

			// FIXME use standard connect security here
			// if PRIVATESUBNET {
			// 	retunr nil, fmt.Errorf("Cannot access private subnets")
			// }

			// FIXME caller IP

			if request.DestAddr.FQDN != "" {
				addrPort, _ := netip.ParseAddrPort(addr)
				addr = fmt.Sprintf("%s:%d", request.DestAddr.FQDN, addrPort.Port())
			}

			conn, err := self.connectProxy(proxyId, TunDial{
				Network: network,
				Address: addr,
			})
			if err != nil {
				glog.Infof("[socks]connect (%s) %s %s (%s) err=%s\n", proxyId, network, addr, request.DestAddr.FQDN, err)
				return nil, err
			}
			glog.Infof("[socks]connect (%s) %s %s (%s) success\n", proxyId, network, addr, request.DestAddr.FQDN)
			return conn, err
		}),
	)

	listenIpv4, _, listenPort := server.RequireListenIpPort(self.settings.ListenSocksPort)

	listenConfig := net.ListenConfig{}

	serverConn, err := listenConfig.Listen(
		handleCtx,
		"tcp",
		net.JoinHostPort(listenIpv4, strconv.Itoa(listenPort)),
	)
	if err != nil {
		return
	}
	defer serverConn.Close()

	if self.settings.EnableProxyProtocol {
		serverConn = NewPpServerConn(serverConn, DefaultWarpPpSettings())
	}

	socksServer.Serve(serverConn)
}

// socks.Logger
func (self *ProxyConnectHandler) Errorf(format string, args ...any) {
	glog.Errorf("[socks]"+format, args...)
}

// socks.CredentialStore
func (self *ProxyConnectHandler) Valid(username string, password string, userAddr string) bool {
	proxyId, err := model.ParseSignedProxyId(username)
	if err != nil {
		return false
	}

	addrPort, err := netip.ParseAddrPort(userAddr)
	if err != nil {
		glog.Infof("[socks]user address %s err=%s\n", userAddr, err)
		return false
	}

	glog.Infof("[socks]user valid %s (%s)\n", proxyId, addrPort)

	// proxyConnection, err := self.connectProxy(proxyId)
	// if err != nil {
	// 	return false
	// }

	return self.ValidCaller(proxyId, addrPort.Addr())
}

// socks.NameResolver
func (self *ProxyConnectHandler) Resolve(ctx context.Context, name string) (context.Context, net.IP, error) {
	// names are not resolved locally
	return ctx, net.ParseIP("0.0.0.0").To4(), nil
}

func (self *ProxyConnectHandler) ValidCaller(proxyId server.Id, addr netip.Addr) bool {
	// FIXME
	return true
}

type ProxyConnection struct {
	ctx    context.Context
	cancel context.CancelFunc

	exchange          *Exchange
	tunDial           TunDial
	proxyDeviceConfig *model.ProxyDeviceConfig

	residentTransport *ResidentTransport

	// tnet      *proxy.Net
	// httpProxy *goproxy.ProxyHttpServer

	// modified by the reader only
	lookaheadBuffer []byte

	readDeadline  time.Time
	writeDeadline time.Time

	stateLock        sync.Mutex
	lastActivityTime time.Time

	settings *ProxyConnectHandlerSettings
}

var localIpCounter atomic.Uint32

func NewProxyConnection(
	ctx context.Context,
	exchange *Exchange,
	tunDial TunDial,
	proxyId server.Id,
	settings *ProxyConnectHandlerSettings,
) (*ProxyConnection, error) {
	cancelCtx, cancel := context.WithCancel(ctx)

	proxyDeviceConfig := model.GetProxyDeviceConfig(cancelCtx, proxyId)
	if proxyDeviceConfig == nil {
		return nil, fmt.Errorf("Proxy device does not exist.")
	}

	glog.Infof(
		"[tp]connect to client_id=%s instance_id=%s (proxy_id=%s)\n",
		proxyDeviceConfig.ClientId,
		proxyDeviceConfig.InstanceId,
		proxyDeviceConfig.ProxyId,
	)

	residentTransport := NewResidentProxyTransport(
		cancelCtx,
		exchange,
		tunDial,
		proxyDeviceConfig.ClientId,
		proxyDeviceConfig.InstanceId,
	)

	proxyConnection := &ProxyConnection{
		ctx:               cancelCtx,
		cancel:            cancel,
		exchange:          exchange,
		tunDial:           tunDial,
		proxyDeviceConfig: proxyDeviceConfig,
		residentTransport: residentTransport,
		// tnet:              tnet,
		// httpProxy:         httpProxy,
		lastActivityTime: time.Now(),
		settings:         settings,
	}
	go server.HandleError(proxyConnection.run)

	return proxyConnection, nil
}

func (self *ProxyConnection) run() {
	go server.HandleError(func() {
		defer self.cancel()
		self.residentTransport.Run()
		// close is done in the write
	})

	go server.HandleError(func() {
		defer self.cancel()
		for {
			if self.CancelIfIdle() {
				glog.V(1).Infof("[rp]idle %s\n", self.proxyDeviceConfig.ProxyId)
				return
			}

			select {
			case <-self.Done():
				return
			case <-time.After(self.settings.ProxyConnectionIdleTimeout):
			}
		}
	})

	select {
	case <-self.ctx.Done():
	}
}

func (self *ProxyConnection) Read(b []byte) (n int, err error) {
	// read from lookahead
	// if need more, pull from receive
	self.UpdateActivity()

	if len(b) == 0 {
		return 0, nil
	}

	if 0 < len(self.lookaheadBuffer) {
		m := copy(b, self.lookaheadBuffer)
		self.lookaheadBuffer = self.lookaheadBuffer[m:]
		n += m
	}

	// fill blocking if there is no data
	if n == 0 {
		if len(self.lookaheadBuffer) != 0 {
			panic(fmt.Errorf("lookaheader buffer must be empty (%d)", len(self.lookaheadBuffer)))
		}
		if self.lookaheadBuffer != nil {
			connect.MessagePoolReturn(self.lookaheadBuffer)
			self.lookaheadBuffer = nil
		}

		if !self.readDeadline.IsZero() {
			timeout := self.readDeadline.Sub(time.Now())
			if timeout <= 0 {
				return n, fmt.Errorf("Timeout")
			}

			select {
			case <-self.ctx.Done():
				return n, io.EOF
			case self.lookaheadBuffer = <-self.residentTransport.receive:
				m := copy(b[n:], self.lookaheadBuffer)
				self.lookaheadBuffer = self.lookaheadBuffer[m:]
				n += m
			case <-time.After(timeout):
				return n, fmt.Errorf("Timeout")
			}
		} else {
			select {
			case <-self.ctx.Done():
				return n, io.EOF
			case self.lookaheadBuffer = <-self.residentTransport.receive:
				m := copy(b[n:], self.lookaheadBuffer)
				self.lookaheadBuffer = self.lookaheadBuffer[m:]
				n += m
			}
		}
	}

	// fill as much as possible non blocking
	for n < len(b) {
		if len(self.lookaheadBuffer) != 0 {
			panic(fmt.Errorf("lookaheader buffer must be empty (%d)", len(self.lookaheadBuffer)))
		}
		if self.lookaheadBuffer != nil {
			connect.MessagePoolReturn(self.lookaheadBuffer)
			self.lookaheadBuffer = nil
		}
		nb := true
		select {
		case <-self.ctx.Done():
			return n, io.EOF
		case self.lookaheadBuffer = <-self.residentTransport.receive:
			m := copy(b[n:], self.lookaheadBuffer)
			self.lookaheadBuffer = self.lookaheadBuffer[m:]
			n += m
		default:
			nb = false
		}
		if !nb {
			break
		}
	}

	glog.V(1).Infof("[tun]proxy connect (%s, %s) read %d\n", self.tunDial.Network, self.tunDial.Address, n)

	return
}

func (self *ProxyConnection) Write(b []byte) (int, error) {
	self.UpdateActivity()

	if len(b) == 0 {
		return 0, nil
	}

	packet := connect.MessagePoolCopy(b)

	if !self.writeDeadline.IsZero() {
		timeout := self.writeDeadline.Sub(time.Now())
		if timeout <= 0 {
			return 0, fmt.Errorf("Timeout")
		}

		select {
		case <-self.ctx.Done():
			connect.MessagePoolReturn(packet)
			return 0, fmt.Errorf("Done")
		case self.residentTransport.send <- packet:
			glog.V(1).Infof("[tun]proxy connect (%s, %s) write %d\n", self.tunDial.Network, self.tunDial.Address, len(packet))
			return len(packet), nil
		case <-time.After(timeout):
			// drop
			connect.MessagePoolReturn(packet)
			return 0, fmt.Errorf("Timeout")
		}
	} else {
		select {
		case <-self.ctx.Done():
			connect.MessagePoolReturn(packet)
			return 0, fmt.Errorf("Done")
		case self.residentTransport.send <- packet:
			glog.V(1).Infof("[tun]proxy connect (%s, %s) write %d\n", self.tunDial.Network, self.tunDial.Address, len(packet))
			return len(packet), nil
		}
	}
}

func (self *ProxyConnection) LocalAddr() net.Addr {
	return SimpleNetAddr(self.tunDial.Network, "0.0.0.0:0")
}

func (self *ProxyConnection) RemoteAddr() net.Addr {
	return SimpleNetAddr(self.tunDial.Network, "0.0.0.0:0")
}

func (self *ProxyConnection) SetDeadline(t time.Time) error {
	self.readDeadline = t
	self.writeDeadline = t
	return nil
}

func (self *ProxyConnection) SetReadDeadline(t time.Time) error {
	self.readDeadline = t
	return nil
}

func (self *ProxyConnection) SetWriteDeadline(t time.Time) error {
	self.writeDeadline = t
	return nil
}

func (self *ProxyConnection) Close() error {
	self.cancel()
	// self.tnet.Close()
	self.residentTransport.Close()
	return nil
}

// func (self *ProxyConnection) ServeHTTP(w http.ResponseWriter, r *http.Request) {
// 	self.httpProxy.ServeHTTP(w, r)
// }

// func (self *ProxyConnection) ValidCaller(addr netip.Addr) bool {
// 	// FIXME
// 	return true
// }

func (self *ProxyConnection) UpdateActivity() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	select {
	case <-self.ctx.Done():
		return false
	default:

		self.lastActivityTime = time.Now()
		return true
	}
}

func (self *ProxyConnection) CancelIfIdle() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	idleTimeout := time.Now().Sub(self.lastActivityTime)
	if self.settings.ProxyConnectionIdleTimeout <= idleTimeout {
		self.cancel()
		return true
	}
	return false
}

func (self *ProxyConnection) Done() <-chan struct{} {
	return self.ctx.Done()
}

func (self *ProxyConnection) Cancel() {
	self.cancel()
}

type simpleNetAddr struct {
	network string
	address string
}

func SimpleNetAddr(network string, address string) net.Addr {
	return &simpleNetAddr{
		network: network,
		address: address,
	}
}

func (self *simpleNetAddr) Network() string {
	return self.network
}

func (self *simpleNetAddr) String() string {
	return self.address
}
