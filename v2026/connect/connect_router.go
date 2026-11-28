package connect

import (
	"context"

	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/router"
)

type ConnectRouter struct {
	ctx            context.Context
	cancel         context.CancelFunc
	exchange       *Exchange
	service        string
	envService     string
	connectHandler *ConnectHandler
}

func connectHandlerSettingsFromExchange(exchange *Exchange) *ConnectHandlerSettings {
	return &exchange.settings.ConnectHandlerSettings
}

// Keeps the handler and exchange on one settings snapshot. Callers that
// customize ingress cannot safely reconstruct handler defaults here: doing so
// silently restores Proxy Protocol and discards TLS identity settings after
// the exchange has already been created.
func NewConnectRouterFromExchange(
	ctx context.Context,
	cancel context.CancelFunc,
	exchange *Exchange,
) *ConnectRouter {
	return NewConnectRouter(
		ctx,
		cancel,
		exchange,
		connectHandlerSettingsFromExchange(exchange),
	)
}

func newConnectRouterFromExchange(
	ctx context.Context,
	cancel context.CancelFunc,
	exchange *Exchange,
) (*ConnectRouter, error) {
	return newConnectRouter(
		ctx,
		cancel,
		exchange,
		connectHandlerSettingsFromExchange(exchange),
	)
}

// NewConnectRouterFromExchangeE is the checked construction used by process
// startup outside this package, where a TLS or handler initialization failure
// must be reported rather than crash the process.
func NewConnectRouterFromExchangeE(
	ctx context.Context,
	cancel context.CancelFunc,
	exchange *Exchange,
) (*ConnectRouter, error) {
	return newConnectRouterFromExchange(ctx, cancel, exchange)
}

func NewConnectRouter(
	ctx context.Context,
	cancel context.CancelFunc,
	exchange *Exchange,
	connectHandlerSettings *ConnectHandlerSettings,
) *ConnectRouter {
	connectRouter, err := newConnectRouter(ctx, cancel, exchange, connectHandlerSettings)
	if err != nil {
		panic(err)
	}
	return connectRouter
}

func newConnectRouter(
	ctx context.Context,
	cancel context.CancelFunc,
	exchange *Exchange,
	connectHandlerSettings *ConnectHandlerSettings,
) (*ConnectRouter, error) {
	handlerId := model.CreateNetworkClientHandler(ctx)

	// stamp the handler in the provider liveness writer set while it lives and
	// the process's lease writes succeed, so the API enforces leases only
	// while every live handler writes them (transport_announce_liveness_lease.go)
	go server.HandleError(func() {
		runProviderLivenessWriter(
			ctx,
			handlerId,
			model.ProviderLivenessWriterInterval,
			defaultProviderLivenessLeaseWriter,
			model.PublishProviderLivenessWriter,
			observeLivenessLeaseOutcome,
		)
	})

	// update the heartbeat. a handler that handler expiry retired shuts the
	// process down only once the database confirms it (handler_heartbeat.go)
	newHandlerHeartbeat(ctx, handlerId, DefaultHandlerHeartbeatSettings(), cancel)

	service := strings.ToLower(server.RequireService())
	envService := strings.ToLower(fmt.Sprintf("%s-%s", server.RequireEnv(), server.RequireService()))

	connectHandler, err := newConnectHandlerWithPacketConns(
		ctx,
		handlerId,
		exchange,
		connectHandlerSettings,
		ConnectHandlerPacketConns{},
	)
	if err != nil {
		cancel()
		return nil, err
	}

	return &ConnectRouter{
		ctx:            ctx,
		cancel:         cancel,
		exchange:       exchange,
		service:        service,
		envService:     envService,
		connectHandler: connectHandler,
	}, nil
}

func (self *ConnectRouter) Connect(w http.ResponseWriter, r *http.Request) {
	self.connectHandler.Connect(w, r)
}

// The routes of connect names behind the lb, alike on the private listener
// nginx forwards them to and in-process behind the lb_alt front: only GET /
// reaches connect, the websocket and H1+ upgrades included. The router
// answers any other path 404 and any other method 405 with Allow, cookies
// dropped, before connect counts an H1 handshake or charges the address's
// rate limit.
func newConnectRoutes(connect http.HandlerFunc) []*router.Route {
	return []*router.Route{router.NewRoute("GET", "/", connect)}
}

// The handler behind this router's routes. The alt front dispatches accepted
// QUIC connections into the same handler instead of mounting the H1 route, so
// both fronts share one handler id, heartbeat and drain.
func (self *ConnectRouter) ConnectHandler() *ConnectHandler {
	return self.connectHandler
}

// Status prevents Warp from activating a replacement until every QUIC
// listener enabled by its port allocation is actually accepting. It also
// becomes unready again if a listener exits between supervised restarts.
func (self *ConnectRouter) Status(w http.ResponseWriter, r *http.Request) {
	self.statusWithWarpStatus(w, r, nil, nil)
}

// Keeps transport readiness on the actual handler while selecting the
// composed service's independent startup/drain latch. With the lb_alt front
// configured, publicFront adds its readiness headers (front_status.go); it is
// nil otherwise.
func (self *ConnectRouter) statusWithWarpStatus(w http.ResponseWriter, r *http.Request, status *router.WarpStatusState, publicFront *frontServer) {
	listenerPorts, err := self.connectHandler.ListenerReadyUdpPorts()
	if publicFront != nil {
		publicFront.writeStatusHeaders(w.Header(), status.Status(), err == nil)
	}
	if err != nil {
		http.Error(w, fmt.Sprintf("not ready: %s", err), http.StatusServiceUnavailable)
		return
	}
	// Warp's LB rollout probes every Connect block directly and requires this
	// explicit capability signal. An older constant-ok server therefore cannot
	// accidentally authorize activation of a new QUIC/DNS mapping.
	w.Header().Set("X-UR-Connect-Listeners-Ready", "1")
	portStrings := make([]string, len(listenerPorts))
	for i, port := range listenerPorts {
		portStrings[i] = strconv.Itoa(port)
	}
	w.Header().Set("X-UR-Connect-UDP-Listeners", strings.Join(portStrings, ","))
	status.Handler(w, r)
}

// func (self *ConnectRouter) ProxyConnect(w http.ResponseWriter, r *http.Request) {
// 	self.proxyConnectHandler.Connect(w, r)
// }
