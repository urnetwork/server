package connect

import (
	"context"

	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

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

func NewConnectRouter(
	ctx context.Context,
	cancel context.CancelFunc,
	exchange *Exchange,
	connectHandlerSettings *ConnectHandlerSettings,
) *ConnectRouter {
	handlerId := model.CreateNetworkClientHandler(ctx)

	// update the heartbeat
	go server.HandleError(func() {
		defer cancel()
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(min(5*time.Second, model.NetworkClientHandlerHeartbeatTimeout/2)):
			}
			// try again after unhandled errors. these signal a transient issue such as db load
			server.HandleError(func() {
				err := model.HeartbeatNetworkClientHandler(ctx, handlerId)
				if err != nil {
					// shut down
					cancel()
				}
			})
		}
	})

	service := strings.ToLower(server.RequireService())
	envService := strings.ToLower(fmt.Sprintf("%s-%s", server.RequireEnv(), server.RequireService()))

	connectHandler := NewConnectHandler(ctx, handlerId, exchange, connectHandlerSettings)

	return &ConnectRouter{
		ctx:            ctx,
		cancel:         cancel,
		exchange:       exchange,
		service:        service,
		envService:     envService,
		connectHandler: connectHandler,
	}
}

func (self *ConnectRouter) Connect(w http.ResponseWriter, r *http.Request) {
	self.connectHandler.Connect(w, r)
}

// Status prevents Warp from activating a replacement until every QUIC
// listener enabled by its port allocation is actually accepting. It also
// becomes unready again if a listener exits between supervised restarts.
func (self *ConnectRouter) Status(w http.ResponseWriter, r *http.Request) {
	listenerPorts, err := self.connectHandler.ListenerReadyUdpPorts()
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
	router.WarpStatus(w, r)
}

// func (self *ConnectRouter) ProxyConnect(w http.ResponseWriter, r *http.Request) {
// 	self.proxyConnectHandler.Connect(w, r)
// }
