// Serves the proxy control endpoints with shared route/configuration assembly.
package proxy

import (
	"context"
	"crypto/tls"
	"encoding/json"
	// "fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
)

// The tls control api (warmup, device rpc, flow trace) on one service port, on
// each host address warp assigns: ipv4, ipv6 or both. Each address family is
// its own listener worker under the same supervisor as the socks/http ingress,
// so both families serve at once. A listener failure cancels the process
// context, as for that ingress, and is retained for CloseAndWait. The api is
// not a drain target: it serves until cancellation.
type apiServer struct {
	ctx                context.Context
	cancel             context.CancelFunc
	proxyDeviceManager *ProxyDeviceManager
	transportTls       *server.TransportTls
	warmupCallback     func(*model.ProxyClient) error
	port               int
	settings           *ProxySettings
	// binds one address with the network of its family, "tcp4" or "tcp6", and
	// serves it until ctx is canceled
	listenAndServeTls func(ctx context.Context, network string, addr string, handler http.Handler, reusePort bool, httpServerOptions server.HttpServerOptions, tlsConfig *tls.Config) error
	lifecycle         *proxyListenerLifecycle
}

// Serves the api on the production tls listeners.
func NewApiServer(
	ctx context.Context,
	cancel context.CancelFunc,
	proxyDeviceManager *ProxyDeviceManager,
	transportTls *server.TransportTls,
	warmupCallback func(*model.ProxyClient) error,
	port int,
	settings *ProxySettings,
) *apiServer {
	return newApiServer(
		ctx,
		cancel,
		proxyDeviceManager,
		transportTls,
		warmupCallback,
		port,
		settings,
		server.HttpListenAndServeTlsWithReusePort,
	)
}

// Only the per-address bind and serve is replaceable; route assembly, address
// selection and listener supervision are the production ones.
func newApiServer(
	ctx context.Context,
	cancel context.CancelFunc,
	proxyDeviceManager *ProxyDeviceManager,
	transportTls *server.TransportTls,
	warmupCallback func(*model.ProxyClient) error,
	port int,
	settings *ProxySettings,
	listenAndServeTls func(ctx context.Context, network string, addr string, handler http.Handler, reusePort bool, httpServerOptions server.HttpServerOptions, tlsConfig *tls.Config) error,
) *apiServer {
	s := &apiServer{
		ctx:                ctx,
		cancel:             cancel,
		proxyDeviceManager: proxyDeviceManager,
		transportTls:       transportTls,
		warmupCallback:     warmupCallback,
		port:               port,
		settings:           settings,
		listenAndServeTls:  listenAndServeTls,
		lifecycle:          newProxyListenerLifecycle(ctx, cancel),
	}

	s.lifecycle.start(s.run)

	return s
}

// Joins the listener workers after hard cancellation and reports any retained
// listener failure.
func (self *apiServer) CloseAndWait(ctx context.Context) error {
	return self.lifecycle.CloseAndWait(ctx)
}

// A serve returns only at shutdown, so each address family must start in its
// own worker: starting ipv6 after the ipv4 serve returned would leave the ipv6
// address unserved for the life of the process. Each worker binds with the
// network of its family, as the socks/http ingress does: with "tcp", an empty
// or wildcard host is a dual-stack [::] socket, which on Linux collides with
// the other family's bind of the same port.
func (self *apiServer) run() {
	defer self.cancel()

	router, httpServerOptions, tlsConfig := self.httpConfiguration()
	reusePort := false

	listenIpv4, listenIpv6, listenPort := server.RequireListenIpPort(self.port)

	// warp exports an empty address for a family the block's network lacks,
	// and that family gets no listener. For ipv4, a "tcp4" bind of the empty
	// host would listen on every ipv4 address of the host (warp exports these
	// addresses only to host-networked containers), none of which warp
	// assigned to the block or routes its traffic to. Without warp's addresses,
	// RequireListenIpPort returns 0.0.0.0, so the api always has a listener.
	if listenIpv4 != "" {
		self.lifecycle.startListener(func() {
			defer self.cancel()
			err := self.listenAndServeTls(
				self.ctx,
				"tcp4",
				net.JoinHostPort(listenIpv4, strconv.Itoa(listenPort)),
				router,
				reusePort,
				httpServerOptions,
				tlsConfig,
			)
			if err != nil {
				panic(err)
			}
		})
	}
	if listenIpv6 != "" {
		self.lifecycle.startListener(func() {
			defer self.cancel()
			err := self.listenAndServeTls(
				self.ctx,
				"tcp6",
				net.JoinHostPort(listenIpv6, strconv.Itoa(listenPort)),
				router,
				reusePort,
				httpServerOptions,
				tlsConfig,
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

// Assembles the identical authenticated routes, tls selection and timeouts for
// the production listener and its explicitly owned in-process test listener.
func (self *apiServer) httpConfiguration() (http.Handler, server.HttpServerOptions, *tls.Config) {
	// the device rpc control endpoint shares this TLS listener: a DeviceRemote
	// (e.g. a browser) connects with the device's signed proxy id to control the
	// hosted DeviceLocal. It authenticates the same way as /warmup (signed proxy
	// id) and terminates the same per-proxy SNI TLS, so it belongs here rather
	// than on the forward-proxy listener.
	deviceRpc := NewDeviceRpcHandler(self.proxyDeviceManager, self.settings)
	flowTrace := flowTraceHandler{
		auth: authHeaderProxyId, open: self.proxyDeviceManager.OpenProxyDevice,
		lookup: self.proxyDeviceManager.flowTraceDevice,
	}

	routes := []*router.Route{
		router.NewRoute("POST", "/warmup", self.HandleWarmup),
		router.NewRoute("GET", deviceRpcPath, deviceRpc.ServeHTTP),
		router.NewRoute("POST", flowTracePath, flowTrace.ServeHTTP),
		router.NewRoute("GET", flowTracePath, flowTrace.ServeHTTP),
	}

	router := router.NewRouter(self.ctx, routes)

	httpServerOptions := server.HttpServerOptions{
		ReadTimeout:     15 * time.Second,
		WriteTimeout:    30 * time.Second,
		IdleTimeout:     5 * time.Minute,
		ShutdownTimeout: 30 * time.Second,
	}

	tlsConfig := &tls.Config{
		GetConfigForClient: self.transportTls.GetTlsConfigForClient,
	}

	return router, httpServerOptions, tlsConfig
}

type WarmupRequest struct {
	TimeoutSeconds int `json:"timeout_seconds,omitempty"`
}

type WarmupResponse struct {
	Ready bool `json:"ready"`
}

func (self *apiServer) HandleWarmup(w http.ResponseWriter, r *http.Request) {
	authHeader := r.Header.Get("Authorization")
	proxyId, err := authHeaderProxyId(authHeader)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}

	var warmupRequest WarmupRequest

	defer r.Body.Close()
	r.Body = http.MaxBytesReader(w, r.Body, self.settings.MaxRequestBytes)
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
		return
	}

	if 0 < len(bodyBytes) {
		err = json.Unmarshal(bodyBytes, &warmupRequest)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	// else use the default object

	proxyDevice, err := self.proxyDeviceManager.OpenProxyDevice(proxyId)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if self.warmupCallback != nil {
		proxyClient, err := model.GetProxyClient(self.ctx, proxyId)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if proxyClient != nil {
			err = self.warmupCallback(proxyClient)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		}
	}

	timeout := time.Duration(warmupRequest.TimeoutSeconds) * time.Second
	ready := proxyDevice.WaitForReady(r.Context(), timeout)

	warmupResponse := &WarmupResponse{
		Ready: ready,
	}

	out, err := json.Marshal(warmupResponse)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Write(out)
}
