package main

import (
	"context"

	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

type ConnectRouter struct {
	ctx            context.Context
	cancel         context.CancelFunc
	exchange       *Exchange
	service        string
	envService     string
	connectHandler *ConnectHandler
}

func NewConnectRouterWithDefaults(
	ctx context.Context,
	cancel context.CancelFunc,
	exchange *Exchange,
) *ConnectRouter {
	return NewConnectRouter(
		ctx,
		cancel,
		exchange,
		DefaultConnectHandlerSettings(),
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

// func (self *ConnectRouter) ProxyConnect(w http.ResponseWriter, r *http.Request) {
// 	self.proxyConnectHandler.Connect(w, r)
// }
