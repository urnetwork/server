package main

import (
	"context"
	// "crypto/tls"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"syscall"
	"time"

	// "github.com/elazarl/goproxy"
	// socks5 "github.com/things-go/go-socks5"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/glog"
	"github.com/urnetwork/proxy"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// FIXME this is meant to be deployed with no lb and no containers
// FIXME is launches a block of ports per version,
// FIXME and registers the host, ports, version when online
// FIXME the ncm will use the latest versions when distributing new proxies
// FIXME a client can run a proxy on any host:port
// FIXME the proxy server spins up a new resident unique to the service
// FIXME each proxy instance has an idle timeout where it will shut down if not used in M minutes

// this value is set via the linker, e.g.
// -ldflags "-X main.Version=$WARP_VERSION-$WARP_VERSION_CODE"
var Version string

const ListenSocksPort = 8080
const ListenHttpPort = 8081
const ListenHttpsPort = 8082

func DefaultProxySettings() *ProxySettings {
	return &ProxySettings{
		ProxyReadTimeout:         15 * time.Second,
		ProxyWriteTimeout:        30 * time.Second,
		ProxyIdleTimeout:         5 * time.Minute,
		ProxyTlsHandshakeTimeout: 30 * time.Second,
	}
}

type ProxySettings struct {
	ProxyReadTimeout         time.Duration
	ProxyWriteTimeout        time.Duration
	ProxyIdleTimeout.        time.Duration
	ProxyTlsHandshakeTimeout time.Duration
}

func main() {

	settings := DefaultProxySettings()

	// use up to a 4gib message pool per instance
	connect.ResizeMessagePools(connect.Gib(4))

	quitEvent := server.NewEventWithContext(context.Background())
	defer quitEvent.Set()

	closeFn := quitEvent.SetOnSignals(syscall.SIGQUIT, syscall.SIGTERM)
	defer closeFn()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// drain on sigterm
	go server.HandleError(func() {
		defer cancel()
		select {
		case <-ctx.Done():
		case <-quitEvent.Ctx.Done():
			// FIXME drain
		}
	})

	proxyDeviceManager := NewProxyDeviceManagerWithDefaults(ctx)
	defer proxyDeviceManager.Close()

	transportTls, err := server.NewTransportTlsFromConfigWithDefaults()
	if err != nil {
		panic(err)
	}

	glog.Infof("Listen socks5 (:%d), http (:%d), https (:%d)", ListenSocksPort, ListenHttpPort, ListenHttpsPort)

	newSocks5Server(
		ctx,
		cancel,
		proxyDeviceManager,
		transportTls,
		settings,
	)

	newHttpServer(
		ctx,
		cancel,
		proxyDeviceManager,
		transportTls,
		settings,
	)

	newWatchdog(ctx, 5*time.Second)

	select {
	case <-ctx.Done():
	}

	os.Exit(1)
}

type socks5Server struct {
	ctx                context.Context
	cancel             context.CancelFunc
	proxyDeviceManager *ProxyDeviceManager
	transportTls       *server.TransportTls
	settings           *ProxySettings
}

func newSocks5Server(
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
			addrPort, _ := netip.ParseAddrPort(addr)
			addr = fmt.Sprintf("%s:%d", r.DestAddr.FQDN, addrPort.Port())
		}

		pd, err := self.proxyDeviceManager.OpenProxyDevice(proxyId)
		if err != nil {
			return nil, err
		}

		return pd.Tun().DialContext(ctx, network, addr)
	}

	socksProxy := proxy.NewSocksProxy()
	socksProxy.ConnectDialWithRequest = connectDial
	socksProxy.ValidUser = validUser
	socksProxy.ProxyReadTimeout = self.settings.ProxyReadTimeout
	socksProxy.ProxyWriteTimeout = self.settings.ProxyWriteTimeout

	err := socksProxy.ListenAndServe(self.ctx, "tcp", fmt.Sprintf(":%d", ListenSocksPort))
	if err != nil {
		panic(err)
	}
}

type httpServer struct {
	ctx                context.Context
	cancel             context.CancelFunc
	proxyDeviceManager *ProxyDeviceManager
	transportTls       *server.TransportTls
	settings           *ProxySettings
}

func newHttpServer(
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

		headerAuth := r.Header.Get("Proxy-Authorization")

		bearerPrefix := "bearer "
		basicPrefix := "basic "

		if len(bearerPrefix) < len(headerAuth) && strings.ToLower(headerAuth[:len(bearerPrefix)]) == bearerPrefix {
			signedProxyId := headerAuth[len(bearerPrefix):]
			proxyId, err := model.ParseSignedProxyId(signedProxyId)
			if err == nil {
				return proxyId, nil
			}
		} else if len(basicPrefix) < len(headerAuth) && strings.ToLower(headerAuth[:len(basicPrefix)]) == basicPrefix {
			// user:pass
			combinedSignedProxyId, err := base64.StdEncoding.DecodeString(headerAuth[len(basicPrefix):])
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

		return pd.Tun().DialContext(r.Context(), network, addr)
	}

	httpProxy := proxy.NewHttpProxy()
	// httpProxy.Logger = self
	httpProxy.GetTlsConfigForClient = self.transportTls.GetTlsConfigForClient
	httpProxy.ConnectDialWithRequest = connectDial
	httpProxy.ProxyReadTimeout = self.settings.ProxyReadTimeout
	httpProxy.ProxyWriteTimeout = self.settings.ProxyWriteTimeout
	httpProxy.ProxyIdleTimeout = self.settings.ProxyIdleTimeout
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
	go server.HandleError(func() {
		defer self.cancel()
		err := httpProxy.ListenAndServe(self.ctx, "tcp", fmt.Sprintf(":%d", ListenHttpPort))
		if err != nil {
			panic(err)
		}
	})

	// listen https
	go server.HandleError(func() {
		defer self.cancel()
		err := httpProxy.ListenAndServeTls(self.ctx, "tcp", fmt.Sprintf(":%d", ListenHttpsPort))
		if err != nil {
			panic(err)
		}
	})

	select {
	case <-self.ctx.Done():
	}
}
