package connect

import (
	"context"
	"errors"
	"fmt"
	"net"
	"runtime"
	"strconv"
	"strings"
	"time"

	connectcore "github.com/urnetwork/connect"
	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/router"
)

type RunOptions struct {
	Port               int
	TLSDefaultHostName string
}

func (self RunOptions) Validate() error {
	if self.Port < 1 || self.Port > 65_535 {
		return fmt.Errorf("connect port %d is outside [1,65535]", self.Port)
	}
	if self.TLSDefaultHostName != strings.TrimSpace(self.TLSDefaultHostName) || strings.ContainsAny(self.TLSDefaultHostName, "/\\\x00") {
		return errors.New("connect TLS default hostname is invalid")
	}
	return nil
}

func exchangeSettingsForRun(options RunOptions) *ExchangeSettings {
	settings := DefaultExchangeSettings()
	settings.ConnectHandlerSettings.TransportTlsSettings.DefaultHostName = options.TLSDefaultHostName
	return settings
}

// Run serves the production connect module until ctx is canceled. The CLI and
// simulator use the same exchange, router, readiness latch, and drain path.
func Run(ctx context.Context, options RunOptions) error {
	if ctx == nil {
		return errors.New("connect run context is nil")
	}
	if err := options.Validate(); err != nil {
		return err
	}
	connectcore.ResizeMessagePools(connectcore.Gib(16))

	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	routes := []*router.Route{}
	statusHandler := router.WarpStatus
	var exchange *Exchange
	if err := router.StartupReadiness(runCtx); err != nil {
		glog.Infof("[connect]not ready (%s)\n", err)
	} else {
		exchange = NewExchangeFromEnv(runCtx, exchangeSettingsForRun(options))
		defer exchange.Close()
		connectRouter := NewConnectRouterWithDefaults(runCtx, cancel, exchange)
		statusHandler = connectRouter.Status
		routes = append(routes, router.NewRoute("GET", "/", connectRouter.Connect))
		server.Warmup()
	}
	routes = append([]*router.Route{router.NewRoute("GET", "/status", statusHandler)}, routes...)

	draining := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			router.SetWarpStatusDrainingIfReady()
			if exchange != nil {
				exchange.Drain()
			}
			cancel()
		case <-draining:
		}
	}()
	defer close(draining)

	go server.HandleError(func() {
		for {
			select {
			case <-runCtx.Done():
				return
			case <-time.After(30 * time.Second):
			}
			if glog.V(1) {
				glog.Infof("[connect]goroutines=%d/%d\n", runtime.NumGoroutine(), runtime.GOMAXPROCS(0))
			}
		}
	})

	server.StartStatsPusher(runCtx)
	glog.Infof("[connect]serving %s %s on *:%d\n", server.RequireEnv(), server.RequireVersion(), options.Port)
	listenIPv4, _, listenPort := server.RequireListenIpPort(options.Port)
	err := server.HttpListenAndServeWithReusePort(
		runCtx,
		net.JoinHostPort(listenIPv4, strconv.Itoa(listenPort)),
		router.NewRouter(runCtx, routes),
		false,
		server.HttpServerOptions{
			ReadTimeout:     15 * time.Second,
			WriteTimeout:    30 * time.Second,
			IdleTimeout:     5 * time.Minute,
			ShutdownTimeout: 30 * time.Second,
		},
	)
	if err != nil && runCtx.Err() == nil {
		return err
	}
	glog.Infof("[connect]close\n")
	return nil
}
