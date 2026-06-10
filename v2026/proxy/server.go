package proxy

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/urnetwork/glog/v2026"
	"github.com/urnetwork/proxy/v2026"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

const InternalSocksPort = 8080
const InternalHttpPort = 8081
const InternalHttpsPort = 8082
const InternalApiPort = 8083
const InternalWgPort = 8084

func DefaultProxySettings() *ProxySettings {
	return &ProxySettings{
		ProxyReadTimeout:         15 * time.Second,
		ProxyWriteTimeout:        30 * time.Second,
		ProxyIdleTimeout:         5 * time.Minute,
		ProxyTlsHandshakeTimeout: 30 * time.Second,
		ProxyConnectTimeout:      30 * time.Minute,
		NotificationTimeout:      5 * time.Second,
		WarmupTimeout:            30 * time.Minute,
		MaxRequestBytes:          32 * model.Kib,
		EventRateLimitTimeout:    1 * time.Second,
		ReconcileTimeout:         30 * time.Minute,
	}
}

type ProxySettings struct {
	ProxyReadTimeout         time.Duration
	ProxyWriteTimeout        time.Duration
	ProxyIdleTimeout         time.Duration
	ProxyTlsHandshakeTimeout time.Duration
	ProxyConnectTimeout      time.Duration
	NotificationTimeout      time.Duration
	WarmupTimeout            time.Duration
	MaxRequestBytes          model.ByteCount
	EventRateLimitTimeout    time.Duration
	// how often the wg peers are reconciled against the full set of proxy
	// clients for this host/block. The reconcile re-applies the full set
	// (healing clients dropped by a partial restore) and removes peers whose
	// proxy client no longer exists. <= 0 disables the reconcile.
	ReconcileTimeout time.Duration
}

type socks5Server struct {
	ctx                context.Context
	cancel             context.CancelFunc
	proxyDeviceManager *ProxyDeviceManager
	transportTls       *server.TransportTls
	settings           *ProxySettings
}

func NewSocks5Server(
	ctx context.Context,
	cancel context.CancelFunc,
	proxyDeviceManager *ProxyDeviceManager,
	transportTls *server.TransportTls,
	settings *ProxySettings,
) *socks5Server {
	s := &socks5Server{
		ctx:                ctx,
		cancel:             cancel,
		proxyDeviceManager: proxyDeviceManager,
		transportTls:       transportTls,
		settings:           settings,
	}

	go server.HandleError(s.run, cancel)

	return s
}

func (self *socks5Server) run() {
	defer self.cancel()

	validUser := func(username string, password string, userAddr string) bool {
		proxyId, err := model.ParseSignedProxyId(username)
		if err != nil {
			return false
		}

		addrPort, err := netip.ParseAddrPort(userAddr)
		if err != nil {
			glog.V(1).Infof("[socks]user address %s err=%s\n", userAddr, err)
			return false
		}

		glog.V(1).Infof("[socks]user valid %s (%s)\n", proxyId, addrPort)

		return self.proxyDeviceManager.ValidCaller(proxyId, addrPort.Addr())
	}

	connectDial := func(ctx context.Context, r proxy.SocksRequest, network string, addr string) (net.Conn, error) {
		username := r.AuthContext.Payload["username"]
		// the proxy id was already verified by the credential store
		proxyId := model.RequireEncodedProxyId(username)

		if r.DestAddr.FQDN != "" {
			// Dial by FQDN so the tun does DNS resolution (DoH). SocksProxy.Resolve
			// preserves the FQDN (returns nil), so `addr` is host:port, not ip:port
			// — use SplitHostPort to read the port (ParseAddrPort fails on a
			// hostname and would zero the port -> "host unreachable").
			if _, port, err := net.SplitHostPort(addr); err == nil {
				addr = net.JoinHostPort(r.DestAddr.FQDN, port)
			}
		}

		pd, err := self.proxyDeviceManager.OpenProxyDevice(proxyId)
		if err != nil {
			return nil, err
		}

		return pd.DialContext(ctx, network, addr)
	}

	socksProxy := proxy.NewSocksProxy()
	socksProxy.ConnectDialWithRequest = connectDial
	socksProxy.ValidUser = validUser
	socksProxy.ProxyReadTimeout = self.settings.ProxyReadTimeout
	socksProxy.ProxyWriteTimeout = self.settings.ProxyWriteTimeout

	listenIpv4, listenIpv6, listenPort := server.RequireListenIpPort(InternalSocksPort)

	go server.HandleError(func() {
		defer self.cancel()
		err := socksProxy.ListenAndServe(
			self.ctx,
			"tcp4",
			net.JoinHostPort(listenIpv4, strconv.Itoa(listenPort)),
		)
		if err != nil {
			panic(err)
		}
	})

	if listenIpv6 != "" {
		go server.HandleError(func() {
			defer self.cancel()
			err := socksProxy.ListenAndServe(
				self.ctx,
				"tcp6",
				net.JoinHostPort(listenIpv6, strconv.Itoa(listenPort)),
			)
			if err != nil {
				panic(err)
			}
		})
	}

	select {
	case <-self.ctx.Done():
	}
}

type httpServer struct {
	ctx                context.Context
	cancel             context.CancelFunc
	proxyDeviceManager *ProxyDeviceManager
	transportTls       *server.TransportTls
	settings           *ProxySettings
}

func NewHttpServer(
	ctx context.Context,
	cancel context.CancelFunc,
	proxyDeviceManager *ProxyDeviceManager,
	transportTls *server.TransportTls,
	settings *ProxySettings,
) *httpServer {
	s := &httpServer{
		ctx:                ctx,
		cancel:             cancel,
		proxyDeviceManager: proxyDeviceManager,
		transportTls:       transportTls,
		settings:           settings,
	}

	go server.HandleError(s.run, cancel)

	return s
}

func (self *httpServer) run() {
	defer self.cancel()

	// tr := &http.Transport{
	// 	IdleConnTimeout: 300 * time.Second,
	// 	TLSHandshakeTimeout: 30 * time.Second,

	// }
	// httpProxy.Tr = tr

	authProxyId := func(r *http.Request) (server.Id, error) {
		if r.TLS != nil {
			host := r.TLS.ServerName
			hostProxyId := strings.SplitN(host, ".", 2)[0]
			proxyId, err := model.ParseSignedProxyId(hostProxyId)
			if err == nil {
				return proxyId, nil
			}
		}

		authHeader := r.Header.Get("Proxy-Authorization")
		return authHeaderProxyId(authHeader)
	}

	connectDial := func(r *http.Request, network string, addr string) (net.Conn, error) {
		proxyId, err := authProxyId(r)
		if err != nil {
			return nil, err
		}

		addrPort, err := netip.ParseAddrPort(r.RemoteAddr)
		if err != nil {
			return nil, err
		}

		if !self.proxyDeviceManager.ValidCaller(proxyId, addrPort.Addr()) {
			return nil, fmt.Errorf("Not authorized")
		}

		pd, err := self.proxyDeviceManager.OpenProxyDevice(proxyId)
		if err != nil {
			return nil, err
		}

		return pd.DialContext(r.Context(), network, addr)
	}

	httpProxy := proxy.NewHttpProxy()
	// httpProxy.Logger = self
	httpProxy.GetTlsConfigForClient = self.transportTls.GetTlsConfigForClient
	httpProxy.ConnectDialWithRequest = connectDial
	httpProxy.ProxyReadTimeout = self.settings.ProxyReadTimeout
	httpProxy.ProxyWriteTimeout = self.settings.ProxyWriteTimeout
	httpProxy.ProxyIdleTimeout = self.settings.ProxyIdleTimeout
	httpProxy.ProxyConnectTimeout = self.settings.ProxyConnectTimeout
	httpProxy.ProxyTlsHandshakeTimeout = self.settings.ProxyTlsHandshakeTimeout
	// httpProxy.AllowHTTP2 = true
	// httpProxy.Tr = &http.Transport{
	// 	TLSHandshakeTimeout:   self.settings.TlsHandshakeTimeout,
	// 	ResponseHeaderTimeout: self.settings.ResponseHeaderTimeout,
	// 	IdleConnTimeout:       300 * time.Second,
	// 	MaxIdleConns:          0,
	// 	MaxIdleConnsPerHost:   4,
	// 	MaxConnsPerHost:       0,
	// 	ForceAttemptHTTP2:     true,
	// }

	// listen http
	func() {
		listenIpv4, listenIpv6, listenPort := server.RequireListenIpPort(InternalHttpPort)

		go server.HandleError(func() {
			defer self.cancel()
			err := httpProxy.ListenAndServe(
				self.ctx,
				"tcp4",
				net.JoinHostPort(listenIpv4, strconv.Itoa(listenPort)),
			)
			if err != nil {
				panic(err)
			}
		})
		if listenIpv6 != "" {
			go server.HandleError(func() {
				defer self.cancel()
				err := httpProxy.ListenAndServe(
					self.ctx,
					"tcp6",
					net.JoinHostPort(listenIpv6, strconv.Itoa(listenPort)),
				)
				if err != nil {
					panic(err)
				}
			})
		}
	}()

	// listen https
	func() {
		listenIpv4, listenIpv6, listenPort := server.RequireListenIpPort(InternalHttpsPort)

		go server.HandleError(func() {
			defer self.cancel()
			err := httpProxy.ListenAndServeTls(
				self.ctx,
				"tcp4",
				net.JoinHostPort(listenIpv4, strconv.Itoa(listenPort)),
			)
			if err != nil {
				panic(err)
			}
		})
		if listenIpv6 != "" {
			go server.HandleError(func() {
				defer self.cancel()
				err := httpProxy.ListenAndServeTls(
					self.ctx,
					"tcp6",
					net.JoinHostPort(listenIpv6, strconv.Itoa(listenPort)),
				)
				if err != nil {
					panic(err)
				}
			})
		}
	}()

	select {
	case <-self.ctx.Done():
	}
}

type wgServer struct {
	ctx                context.Context
	cancel             context.CancelFunc
	proxyDeviceManager *ProxyDeviceManager
	settings           *ProxySettings
	wgProxy            *proxy.WgProxy
}

func NewWgServer(
	ctx context.Context,
	cancel context.CancelFunc,
	proxyDeviceManager *ProxyDeviceManager,
	settings *ProxySettings,
) *wgServer {
	serverConfig := model.LoadServerProxyConfig()

	wgProxySettings := proxy.DefaultWgProxySettings()
	wgProxySettings.PrivateKey = serverConfig.Wg.PrivateKey
	wgProxySettings.FirewallMark = server.RequireFwMark()
	wgProxy := proxy.NewWgProxy(ctx, wgProxySettings)

	s := &wgServer{
		ctx:                ctx,
		cancel:             cancel,
		proxyDeviceManager: proxyDeviceManager,
		settings:           settings,
		wgProxy:            wgProxy,
	}

	go server.HandleError(s.run, cancel)

	return s
}

func (self *wgServer) run() {
	defer self.cancel()

	listenIpv4, listenIpv6, listenPort := server.RequireListenIpPort(InternalWgPort)

	err := self.wgProxy.ListenAndServe(listenIpv4, listenIpv6, listenPort)
	if err != nil {
		panic(err)
	}
}

type wgClientCounts struct {
	total             int
	noWgConfig        int
	publicKeyMismatch int
	invalidAuthToken  int
}

// validWgClients converts proxy clients to wg clients, dropping clients that
// fail validation. The drops are logged individually and tallied in the
// returned counts.
func (self *wgServer) validWgClients(proxyClients []*model.ProxyClient) (map[netip.Addr]*proxy.WgClient, *wgClientCounts) {
	serverConfig := model.LoadServerProxyConfig()

	counts := &wgClientCounts{
		total: len(proxyClients),
	}
	clients := map[netip.Addr]*proxy.WgClient{}
	for _, proxyClient := range proxyClients {
		if proxyClient.WgConfig == nil {
			counts.noWgConfig += 1
			continue
		}
		if proxyClient.WgConfig.ProxyPublicKey != serverConfig.Wg.PublicKey {
			glog.Infof("[wg][%s]public key mismatch %s<>%s\n", proxyClient.ProxyId, proxyClient.WgConfig.ProxyPublicKey, serverConfig.Wg.PublicKey)
			counts.publicKeyMismatch += 1
			continue
		}
		// verify that the access token is still valid
		if proxyId, err := model.ParseSignedProxyId(proxyClient.AuthToken); err != nil {
			glog.Infof("[wg][%s]signed proxy id err=%s\n", proxyClient.ProxyId, err)
			counts.invalidAuthToken += 1
			continue
		} else if proxyId != proxyClient.ProxyId {
			glog.Infof("[wg][%s]signed proxy id mismatch %s\n", proxyClient.ProxyId, proxyId)
			counts.invalidAuthToken += 1
			continue
		}
		glog.V(1).Infof("[wg][%s]add client %s %s\n", proxyClient.ProxyId, proxyClient.WgConfig.ClientPublicKey, proxyClient.WgConfig.ClientIpv4)
		// the factory is called from wg device goroutines, which do not recover
		// panics. Model calls raise on a canceled ctx (e.g. instance shutdown
		// with in-flight client packets), so convert panics to an error - the
		// device drops the packet instead of crashing the process.
		tun := func() (tun proxy.WgTun, err error) {
			if r := server.HandleError(func() {
				tun, err = self.proxyDeviceManager.OpenProxyDevice(proxyClient.ProxyId)
			}); r != nil {
				if rErr, ok := r.(error); ok {
					err = rErr
				} else {
					err = fmt.Errorf("open proxy device: %v", r)
				}
			}
			return
		}
		client := &proxy.WgClient{
			PublicKey:  proxyClient.WgConfig.ClientPublicKey,
			ClientIpv4: proxyClient.WgConfig.ClientIpv4,
			Tun:        tun,
		}
		clients[proxyClient.WgConfig.ClientIpv4] = client
	}
	return clients, counts
}

func (self *wgServer) logClientCounts(op string, applied int, removed int, counts *wgClientCounts) {
	glog.Infof(
		"[wg]%s clients: %d/%d applied, %d removed (%d no wg config, %d key mismatch, %d bad auth token), peers %d/%d\n",
		op,
		applied,
		counts.total,
		removed,
		counts.noWgConfig,
		counts.publicKeyMismatch,
		counts.invalidAuthToken,
		self.wgProxy.ClientCount(),
		proxy.MaxClients,
	)
}

func (self *wgServer) AddProxyClients(proxyClients ...*model.ProxyClient) error {
	clients, counts := self.validWgClients(proxyClients)

	if len(clients) == 0 {
		if 0 < counts.total {
			self.logClientCounts("add", 0, 0, counts)
		}
		return nil
	}

	applied, err := self.wgProxy.AddClients(clients)
	if err != nil {
		glog.Infof("[wg]add clients err=%s\n", err)
	}
	self.logClientCounts("add", len(applied), 0, counts)

	if len(applied) == 0 {
		// a total failure is treated as transient and retried by the caller
		// (the notification does not advance past it). Partial failures are
		// logged above and healed by the periodic reconcile.
		return errors.Join(fmt.Errorf("no wg clients applied (%d valid)", len(clients)), err)
	}
	return nil
}

// SyncProxyClients reconciles the wg device against the full set of proxy
// clients for this host/block: it (re)applies the current clients, healing any
// previous partial restore, and removes peers whose proxy client no longer
// exists (e.g. reaped rows), keeping the device peer table bounded. Only peers
// applied before syncStartTime are eligible for removal, so a client warmed up
// concurrently with the db snapshot is never removed.
func (self *wgServer) SyncProxyClients(proxyClients []*model.ProxyClient, syncStartTime time.Time) error {
	clients, counts := self.validWgClients(proxyClients)

	applied, addErr := self.wgProxy.AddClients(clients)
	if addErr != nil {
		glog.Infof("[wg]sync add clients err=%s\n", addErr)
	}

	staleAddrs := []netip.Addr{}
	if 0 < len(clients) {
		for addr := range self.wgProxy.Clients() {
			if _, ok := clients[addr]; !ok {
				staleAddrs = append(staleAddrs, addr)
			}
		}
	} else if 0 < self.wgProxy.ClientCount() {
		// an empty full set with live peers is more likely a sync issue (e.g.
		// host/block misconfiguration) than a true mass removal - never remove
		// every peer at once
		glog.Infof("[wg]sync returned no clients with %d peers registered; skipping removals\n", self.wgProxy.ClientCount())
	}
	removed := 0
	var removeErr error
	if 0 < len(staleAddrs) {
		peerCount := self.wgProxy.ClientCount()
		// `RemoveClients` re-checks the add time cutoff under its state lock
		removeErr = self.wgProxy.RemoveClients(syncStartTime, staleAddrs...)
		if removeErr != nil {
			glog.Infof("[wg]sync remove clients err=%s\n", removeErr)
		}
		removed = peerCount - self.wgProxy.ClientCount()
	}
	self.logClientCounts("sync", len(applied), removed, counts)

	return errors.Join(addErr, removeErr)
}

func authHeaderProxyId(authHeader string) (server.Id, error) {
	bearerPrefix := "bearer "
	basicPrefix := "basic "

	if len(bearerPrefix) < len(authHeader) && strings.ToLower(authHeader[:len(bearerPrefix)]) == bearerPrefix {
		signedProxyId := authHeader[len(bearerPrefix):]
		proxyId, err := model.ParseSignedProxyId(signedProxyId)
		if err == nil {
			return proxyId, nil
		}
	} else if len(basicPrefix) < len(authHeader) && strings.ToLower(authHeader[:len(basicPrefix)]) == basicPrefix {
		// user:pass
		combinedSignedProxyId, err := base64.StdEncoding.DecodeString(authHeader[len(basicPrefix):])
		if err != nil {
			return server.Id{}, err
		}
		signedProxyId := strings.SplitN(string(combinedSignedProxyId), ":", 2)[0]
		proxyId, err := model.ParseSignedProxyId(signedProxyId)
		if err == nil {
			return proxyId, nil
		}
	}

	return server.Id{}, fmt.Errorf("Not authorized")
}
