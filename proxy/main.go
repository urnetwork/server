package main

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"syscall"

	"github.com/stripe/goproxy"
	"github.com/things-go/go-socks5"

	"github.com/urnetwork/glog"
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

func main() {

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
	)

	newHttpServer(
		ctx,
		cancel,
		proxyDeviceManager,
		transportTls,
	)

	select {
	case <-ctx.Done():
	}
}

type socks5Server struct {
	ctx                context.Context
	cancel             context.CancelFunc
	proxyDeviceManager *ProxyDeviceManager
	transportTls       *server.TransportTls
}

func newSocks5Server(
	ctx context.Context,
	cancel context.CancelFunc,
	proxyDeviceManager *ProxyDeviceManager,
	transportTls *server.TransportTls,
) *socks5Server {
	s := &socks5Server{
		ctx:                ctx,
		cancel:             cancel,
		proxyDeviceManager: proxyDeviceManager,
		transportTls:       transportTls,
	}

	go server.HandleError(s.run, cancel)

	return s
}

func (self *socks5Server) run() {
	defer self.cancel()

	socksServer := socks5.NewServer(
		socks5.WithLogger(self),
		socks5.WithCredential(self),
		// socks5.WithRewriter(self),
		socks5.WithResolver(self),
		socks5.WithRule(socks5.NewPermitConnAndAss()),
		socks5.WithDialAndRequest(func(ctx context.Context, network string, addr string, request *socks5.Request) (net.Conn, error) {
			return server.HandleError2(func() (net.Conn, error) {
				username := request.AuthContext.Payload["username"]
				// the proxy id was already verified by the credential store
				proxyId := model.RequireEncodedProxyId(username)

				if request.DestAddr.FQDN != "" {
					addrPort, _ := netip.ParseAddrPort(addr)
					addr = fmt.Sprintf("%s:%d", request.DestAddr.FQDN, addrPort.Port())
				}

				pd, err := self.proxyDeviceManager.OpenProxyDevice(proxyId)
				if err != nil {
					return nil, err
				}

				return pd.Tun().DialContext(ctx, network, addr)
			}, func() (net.Conn, error) {
				return nil, fmt.Errorf("Unexpected error")
			})
		}),
	)

	socksServer.ListenAndServe("tcp", fmt.Sprintf(":%d", ListenSocksPort))
}

// socks.Logger
func (self *socks5Server) Errorf(format string, args ...any) {
	glog.Errorf("[socks]"+format, args...)
}

// socks.CredentialStore
func (self *socks5Server) Valid(username string, password string, userAddr string) bool {
	return server.HandleError1(func() bool {
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

		return self.proxyDeviceManager.ValidCaller(proxyId, addrPort.Addr())
	}, func() bool {
		return false
	})
}

// socks.NameResolver
func (self *socks5Server) Resolve(ctx context.Context, name string) (context.Context, net.IP, error) {
	// names are not resolved locally
	return ctx, net.ParseIP("0.0.0.0").To4(), nil
}

type httpServer struct {
	ctx                context.Context
	cancel             context.CancelFunc
	proxyDeviceManager *ProxyDeviceManager
	transportTls       *server.TransportTls
}

func newHttpServer(
	ctx context.Context,
	cancel context.CancelFunc,
	proxyDeviceManager *ProxyDeviceManager,
	transportTls *server.TransportTls,
) *httpServer {
	s := &httpServer{
		ctx:                ctx,
		cancel:             cancel,
		proxyDeviceManager: proxyDeviceManager,
		transportTls:       transportTls,
	}

	go server.HandleError(s.run, cancel)

	return s
}

func (self *httpServer) run() {
	defer self.cancel()

	httpProxy := goproxy.NewProxyHttpServer()

	// tr := &http.Transport{
	// 	IdleConnTimeout: 300 * time.Second,
	// 	TLSHandshakeTimeout: 30 * time.Second,

	// }
	// httpProxy.Tr = tr

	authProxyId := func(r *http.Request) (server.Id, error) {
		glog.Infof("AUTH %v\n", r.Header)

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

	httpProxy.ConnectDialContext = func(proxyCtx *goproxy.ProxyCtx, network string, addr string) (net.Conn, error) {
		return server.HandleError2(func() (net.Conn, error) {
			r := proxyCtx.Req

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
		}, func() (net.Conn, error) {
			return nil, fmt.Errorf("Unexpected error")
		})

	}

	// listen http
	go server.HandleError(func() {
		defer self.cancel()

		addr := fmt.Sprintf(":%d", ListenHttpPort)

		httpServer := &http.Server{
			Addr:    addr,
			Handler: httpProxy,
		}

		listenConfig := net.ListenConfig{}

		serverConn, err := listenConfig.Listen(
			self.ctx,
			"tcp",
			addr,
		)
		if err != nil {
			return
		}
		defer serverConn.Close()

		httpServer.Serve(serverConn)
	})

	// listen https
	go server.HandleError(func() {
		defer self.cancel()

		addr := fmt.Sprintf(":%d", ListenHttpsPort)

		tlsConfig := &tls.Config{
			GetConfigForClient: self.transportTls.GetTlsConfigForClient,
		}

		httpServer := &http.Server{
			Addr:      addr,
			Handler:   httpProxy,
			TLSConfig: tlsConfig,
		}

		listenConfig := net.ListenConfig{}

		serverConn, err := listenConfig.Listen(
			self.ctx,
			"tcp",
			addr,
		)
		if err != nil {
			return
		}
		defer serverConn.Close()

		httpServer.ServeTLS(serverConn, "", "")
	})

	select {
	case <-self.ctx.Done():
	}
}
