package main

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	mathrand "math/rand"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/elazarl/goproxy"
	"github.com/things-go/go-socks5"
	"github.com/things-go/go-socks5/statute"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/glog"
	"github.com/urnetwork/proxy"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
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
	connectHandlerSettings := DefaultConnectHandlerSettings()
	return &ProxyConnectHandlerSettings{
		WriteTimeout:               connectHandlerSettings.WriteTimeout,
		ReadTimeout:                connectHandlerSettings.ReadTimeout,
		ProxyConnectionIdleTimeout: 15 * time.Minute,
		ListenSocksPort:            1080,
		ListenHttpsPort:            444,
		FramerSettings:             connect.DefaultFramerSettings(),
		EnableProxyProtocol:        true,
		ProxyConnectTimeout:        15 * time.Second,
		Mtu:                        1440,

		TransportTlsSettings: DefaultTransportTlsSettings(),
	}
}

type ProxyConnectHandlerSettings struct {
	WriteTimeout               time.Duration
	ReadTimeout                time.Duration
	ProxyConnectionIdleTimeout time.Duration
	ListenSocksPort            int
	ListenHttpsPort            int
	FramerSettings             *connect.FramerSettings
	EnableProxyProtocol        bool
	ProxyConnectTimeout        time.Duration
	Mtu                        int

	TransportTlsSettings *TransportTlsSettings
}

type ProxyConnectHandler struct {
	ctx          context.Context
	cancel       context.CancelFunc
	handlerId    server.Id
	exchange     *Exchange
	transportTls *TransportTls
	settings     *ProxyConnectHandlerSettings

	stateLock        sync.Mutex
	proxyConnections map[server.Id]*ProxyConnection
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
		ctx:              cancelCtx,
		cancel:           cancel,
		handlerId:        handlerId,
		exchange:         exchange,
		transportTls:     transportTls,
		settings:         settings,
		proxyConnections: map[server.Id]*ProxyConnection{},
	}

	if server.HasPort(settings.ListenHttpsPort) {
		go server.HandleError(h.runHttps)
	}

	if server.HasPort(settings.ListenSocksPort) {
		go server.HandleError(h.runSocks)
	}

	return h
}

func (self *ProxyConnectHandler) connectProxyForRequest(r *http.Request) (proxyConnection *ProxyConnection, returnErr error) {
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

	proxyConnection, returnErr = self.connectProxy(proxyId)
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

	if !proxyConnection.ValidCaller(addrPort.Addr()) {
		returnErr = fmt.Errorf("Invalid caller")
		return
	}

	return
}

func (self *ProxyConnectHandler) connectProxy(proxyId server.Id) (*ProxyConnection, error) {
	nextProxyConnection := func() (*ProxyConnection, error) {
		proxyConnection, err := NewProxyConnection(self.ctx, self.exchange, proxyId, self.settings)
		if err != nil {
			return nil, err
		}
		go server.HandleError(func() {
			defer func() {
				self.stateLock.Lock()
				defer self.stateLock.Unlock()
				proxyConnection.Close()
				if currentProxyConnection := self.proxyConnections[proxyId]; proxyConnection == currentProxyConnection {
					delete(self.proxyConnections, proxyId)
				}
			}()
			proxyConnection.Run()

			glog.V(1).Infof("[rp]close %s\n", proxyId)
		})
		go server.HandleError(func() {
			defer proxyConnection.Cancel()
			for {
				if proxyConnection.CancelIfIdle() {
					glog.V(1).Infof("[rp]idle %s\n", proxyId)
					return
				}

				select {
				case <-proxyConnection.Done():
					return
				case <-time.After(self.settings.ProxyConnectionIdleTimeout):
				}
			}
		})

		var replacedProxyConnection *ProxyConnection
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			replacedProxyConnection = self.proxyConnections[proxyId]
			self.proxyConnections[proxyId] = proxyConnection
		}()
		if replacedProxyConnection != nil {
			replacedProxyConnection.Cancel()
		}
		glog.V(1).Infof("[rp]open %s\n", proxyId)

		return proxyConnection, nil
	}

	var proxyConnection *ProxyConnection
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		proxyConnection = self.proxyConnections[proxyId]
	}()

	if proxyConnection == nil || !proxyConnection.UpdateActivity() {
		var err error
		proxyConnection, err = nextProxyConnection()
		if err != nil {
			return nil, err
		}
	}

	return proxyConnection, nil
}

func (self *ProxyConnectHandler) runHttps() {
	httpProxy := goproxy.NewProxyHttpServer()

	// httpProxy.Tr = &http.Transport{
	// 	Dial: func(network string, addr string) (net.Conn, error) {
	// 		fmt.Printf("HTTP DIAL 1\n")

	// 		return tnet.DialContext(context.Background(), network, addr)
	// 	},
	// 	DialContext: func(ctx context.Context, network string, addr string) (net.Conn, error) {
	// 		fmt.Printf("HTTP DIAL 2\n")

	// 		addrPort, err := netip.ParseAddrPort(addr)
	// 		if err != nil {
	// 			return nil, err
	// 		}

	// 		return tnet.DialContextTCP(ctx, net.TCPAddrFromAddrPort(addrPort))
	// 	},
	// }

	// httpProxy.Tr = &http.Transport{
	// }

	connectDialWithReq := func(r *http.Request, network string, addr string) (net.Conn, error) {
		proxyConnection, err := self.connectProxyForRequest(r)
		if err != nil {
			return nil, err
		}

		switch network {
		case "tcp", "tcp4", "tcp6":
			network = "tcp4"
		case "udp", "udp4", "udp6":
			network = "udp4"
		default:
			return nil, fmt.Errorf("Unsupported network: %s", network)
		}

		// rCtx, _ := context.WithTimeout(cancelCtx, settings.ProxyConnectTimeout)
		return proxyConnection.tnet.DialContext(self.ctx, network, addr)
	}

	httpProxy.AllowHTTP2 = false
	httpProxy.ConnectDialWithReq = func(r *http.Request, network string, addr string) (conn net.Conn, err error) {
		server.HandleError(func() {
			conn, err = connectDialWithReq(r, network, addr)
		}, func() {
			err = fmt.Errorf("Could not connect")
		})
		return
	}

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

func (self *ProxyConnectHandler) runSocks() {
	handleCtx, handleCancel := context.WithCancel(self.ctx)
	defer handleCancel()

	socksServer := socks5.NewServer(
		socks5.WithLogger(self),
		socks5.WithCredential(self),
		socks5.WithRewriter(self),
		socks5.WithRule(socks5.NewPermitConnAndAss()),
		socks5.WithDialAndRequest(func(ctx context.Context, network string, addr string, request *socks5.Request) (net.Conn, error) {
			username := request.AuthContext.Payload["username"]
			// the proxy id was already verified by the credential store
			proxyId := model.RequireEncodedProxyId(username)

			addrPort, err := netip.ParseAddrPort(addr)
			if err != nil {
				return nil, err
			}

			// FIXME use standard connect security here
			// if PRIVATESUBNET {
			// 	retunr nil, fmt.Errorf("Cannot access private subnets")
			// }

			// FIXME caller IP

			proxyConnection, err := self.connectProxy(proxyId)
			if err != nil {
				return nil, err
			}

			switch network {
			case "tcp", "tcp4", "tcp6":
				return proxyConnection.tnet.DialContextTCP(ctx, net.TCPAddrFromAddrPort(addrPort))
			case "udp", "udp4", "udp6":
				return proxyConnection.tnet.DialContextUDP(ctx, net.UDPAddrFromAddrPort(addrPort))
			default:
				return nil, fmt.Errorf("Unsupported network: %s", network)
			}
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
	glog.Errorf("[tp]"+format, args...)
}

// socks.CredentialStore
func (self *ProxyConnectHandler) Valid(username string, password string, userAddr string) bool {
	proxyId, err := model.ParseSignedProxyId(username)
	if err != nil {
		return false
	}

	addrPort, err := netip.ParseAddrPort(userAddr)
	if err != nil {
		return false
	}

	proxyConnection, err := self.connectProxy(proxyId)
	if err != nil {
		return false
	}

	return proxyConnection.ValidCaller(addrPort.Addr())
}

// socks.AddressRewriter
func (self *ProxyConnectHandler) Rewrite(ctx context.Context, request *socks5.Request) (context.Context, *statute.AddrSpec) {
	dest := request.DestAddr

	if dest.FQDN != "" {
		// resolve via the device
		username := request.AuthContext.Payload["username"]
		// the proxy id was already verified by the credential store
		proxyId := model.RequireEncodedProxyId(username)

		proxyConnection, err := self.connectProxy(proxyId)

		if err == nil {
			addrs := proxyConnection.tnet.DohCache().Query(ctx, "A", dest.FQDN)
			if 0 < len(addrs) {
				// choose one randomly
				addr := addrs[mathrand.Intn(len(addrs))]
				ip := net.IP(addr.AsSlice())

				dest = &statute.AddrSpec{
					IP:   ip,
					Port: dest.Port,
				}
			}
		}
		// else let the name resolve locally
	}

	return ctx, dest
}

type ProxyConnection struct {
	ctx    context.Context
	cancel context.CancelFunc

	exchange          *Exchange
	proxyDeviceConfig *model.ProxyDeviceConfig

	tnet *proxy.Net

	stateLock        sync.Mutex
	lastActivityTime time.Time

	settings *ProxyConnectHandlerSettings
}

var localIpCounter atomic.Uint32

func NewProxyConnection(
	ctx context.Context,
	exchange *Exchange,
	proxyId server.Id,
	settings *ProxyConnectHandlerSettings,
) (*ProxyConnection, error) {
	cancelCtx, cancel := context.WithCancel(ctx)

	proxyDeviceConfig := model.GetProxyDeviceConfig(ctx, proxyId)
	if proxyDeviceConfig == nil {
		return nil, fmt.Errorf("Proxy device does not exist.")
	}

	tnet, err := proxy.CreateNetTUN(
		cancelCtx,
		[]netip.Addr{netip.MustParseAddr(fmt.Sprintf("169.254.2.%d", localIpCounter.Add(1)))},
		settings.Mtu,
	)
	if err != nil {
		return nil, err
	}

	proxyConnection := &ProxyConnection{
		ctx:               cancelCtx,
		cancel:            cancel,
		exchange:          exchange,
		proxyDeviceConfig: proxyDeviceConfig,
		tnet:              tnet,
		// httpProxy:         httpProxy,
		lastActivityTime: time.Now(),
		settings:         settings,
	}

	return proxyConnection, nil
}

func (self *ProxyConnection) Run() {
	defer self.cancel()

	exchangeTransport := NewResidentProxyTransport(
		self.ctx,
		self.exchange,
		self.proxyDeviceConfig.ClientId,
		self.proxyDeviceConfig.InstanceId,
	)
	go server.HandleError(func() {
		defer self.cancel()
		exchangeTransport.Run()
		// close is done in the write
	})
	go server.HandleError(func() {
		defer self.cancel()
		select {
		case <-self.ctx.Done():
		case <-exchangeTransport.Done():
		}
	})

	go server.HandleError(func() {
		defer func() {
			self.cancel()
			exchangeTransport.Close()
		}()
		for {
			packet := connect.MessagePoolGet(self.settings.Mtu)
			n, err := self.tnet.Read(packet)
			if err != nil {
				return
			}
			self.UpdateActivity()
			select {
			case <-self.ctx.Done():
				connect.MessagePoolReturn(packet)
				return
			case exchangeTransport.send <- packet[0:n]:
			case <-time.After(self.settings.WriteTimeout):
				// drop
				connect.MessagePoolReturn(packet)
			}
		}
	})

	go server.HandleError(func() {
		defer self.cancel()
		for {
			select {
			case <-self.ctx.Done():
				return
			case packet := <-exchangeTransport.receive:
				self.UpdateActivity()
				_, err := self.tnet.Write(packet)
				connect.MessagePoolReturn(packet)
				if err != nil {
					return
				}
			}
		}
	})

	select {
	case <-self.ctx.Done():
	case <-exchangeTransport.Done():
	}
}

func (self *ProxyConnection) ValidCaller(addr netip.Addr) bool {
	// FIXME
	return true
}

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

func (self *ProxyConnection) Close() {
	self.cancel()
	self.tnet.Close()
}
