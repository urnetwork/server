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

	connectcore "github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/router"
)

type RunOptions struct {
	Port                 int
	TLSDefaultHostName   string
	DirectH3LoopbackMode bool
}

func connectWarmupTargets() []server.WarmupTarget {
	// Connect derives expected latency and verification metadata from client
	// addresses, but it does not serve API network/location search endpoints.
	return []server.WarmupTarget{server.WarmupTargetIPDatabase}
}

func (self RunOptions) Validate() error {
	if self.Port < 1 || self.Port > 65_535 {
		return fmt.Errorf("connect port %d is outside [1,65535]", self.Port)
	}
	if self.TLSDefaultHostName != strings.TrimSpace(self.TLSDefaultHostName) || strings.ContainsAny(self.TLSDefaultHostName, "/\\\x00") {
		return errors.New("connect TLS default hostname is invalid")
	}
	if self.DirectH3LoopbackMode {
		ip := net.ParseIP(self.TLSDefaultHostName)
		if ip == nil || ip.To4() == nil || !ip.IsLoopback() {
			return errors.New("direct H3 loopback mode requires an IPv4 loopback TLS default hostname")
		}
	}
	return nil
}

func exchangeSettingsForRun(options RunOptions) *ExchangeSettings {
	settings := DefaultExchangeSettings()
	settings.ConnectHandlerSettings.TransportTlsSettings.DefaultHostName = options.TLSDefaultHostName
	if options.DirectH3LoopbackMode {
		// Production ingress supplies Proxy Protocol on every new UDP flow. The
		// simulator owns its exact loopback listeners and dials them directly,
		// so retaining that wrapper would discard every QUIC Initial before TLS.
		settings.ConnectHandlerSettings.EnableProxyProtocol = false
	}
	return settings
}

// Keeps the simulator-only bypass confined to a socket the same host owns.
// An ordinary production listener may still bind any configured address.
func validateRunListenIPv4(options RunOptions, listenIPv4 string) error {
	if !options.DirectH3LoopbackMode {
		return nil
	}
	ip := net.ParseIP(listenIPv4)
	if ip == nil || ip.To4() == nil || !ip.IsLoopback() {
		return fmt.Errorf("direct H3 loopback mode cannot bind %q", listenIPv4)
	}
	return nil
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
	listenIPv4, _, listenPort := server.RequireListenIpPort(options.Port)
	if err := validateRunListenIPv4(options, listenIPv4); err != nil {
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
		connectRouter := NewConnectRouterFromExchange(runCtx, cancel, exchange)
		statusHandler = connectRouter.Status
		routes = append(routes, router.NewRoute("GET", "/", connectRouter.Connect))
		server.Warmup(connectWarmupTargets()...)
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
