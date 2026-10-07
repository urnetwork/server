package connect

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	connectcore "github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/router"
)

type RunOptions struct {
	Port                 int
	TLSDefaultHostName   string
	DirectH3LoopbackMode bool
	// Startup-only, opt-in accounting for resident SDK transfer owners.
	MemoryOwnerLedger bool
	// Optional exact host/block diagnostic scope. Empty reads the optional
	// startup Config resource; "disabled" explicitly installs no socket.
	PrivateHeapProfileTarget string
	// Optional owners for a process composing multiple service runners.
	WarpStatus       *router.WarpStatusState
	StartStatsPusher func(context.Context) func()
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
	if options.MemoryOwnerLedger {
		settings.MemoryOwnerLedger = &connectcore.TransferMemoryOwnerLedger{}
		settings.payloadOwnerLedger = &residentPayloadLedger{}
	}
	if privateHeapProfileTargetsThisInstance(options.PrivateHeapProfileTarget) {
		settings.SDKPayloadOwnerLedger = &connectcore.TransferPayloadOwnerLedger{}
	}
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
	startStatsPusher := options.StartStatsPusher
	if startStatsPusher == nil {
		startStatsPusher = server.StartStatsPusher
	}
	readiness := func(ctx context.Context) error {
		err := router.CheckStartupReadiness(ctx)
		if err != nil {
			options.WarpStatus.SetNotReady(err)
		} else {
			options.WarpStatus.SetReady()
		}
		return err
	}
	return runWithDependencies(ctx, options, readiness, startStatsPusher, server.HttpListenAndServeWithReusePort)
}

func runWithDependencies(
	ctx context.Context,
	options RunOptions,
	readiness func(context.Context) error,
	startStatsPusher func(context.Context) func(),
	listenAndServe func(context.Context, string, http.Handler, bool, server.HttpServerOptions) error,
) error {
	if ctx == nil {
		return errors.New("connect run context is nil")
	}
	if err := options.Validate(); err != nil {
		return err
	}
	privateHeapTarget, privateHeapTargetErr := privateHeapProfileStartupTarget(options.PrivateHeapProfileTarget)
	if privateHeapTargetErr != nil {
		// A missing or invalid optional diagnostic must not gate serving.
		glog.Errorf("[connect]optional private heap configuration unavailable; continuing primary startup\n")
	}
	options.PrivateHeapProfileTarget = privateHeapTarget
	listenIPv4, _, listenPort := server.RequireListenIpPort(options.Port)
	if err := validateRunListenIPv4(options, listenIPv4); err != nil {
		return err
	}
	ConfigureMessagePools()

	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Startup has no established sessions to drain. Cancel dependency reads
	// and construction immediately if the caller or a sibling has stopped.
	stopStartup := context.AfterFunc(ctx, cancel)
	defer stopStartup()
	routes := []*router.Route{}
	statusHandler := options.WarpStatus.Handler
	var exchange *Exchange
	if err := readiness(runCtx); err != nil {
		glog.Infof("[connect]not ready (%s)\n", err)
	} else {
		settings := exchangeSettingsForRun(options)
		unregisterOwnerMetrics, err := registerTransferMemoryOwnerMetrics(prometheus.DefaultRegisterer, settings.MemoryOwnerLedger)
		if err != nil {
			return fmt.Errorf("register Connect transfer owner metrics: %w", err)
		}
		defer unregisterOwnerMetrics()
		unregisterPayloadMetrics, err := registerResidentPayloadMetrics(prometheus.DefaultRegisterer, settings.payloadOwnerLedger)
		if err != nil {
			return fmt.Errorf("register Connect resident payload metrics: %w", err)
		}
		defer unregisterPayloadMetrics()
		exchange = NewExchangeFromEnv(runCtx, settings)
		defer exchange.Close()
		connectRouter, err := newConnectRouterFromExchange(runCtx, cancel, exchange)
		if err != nil {
			return fmt.Errorf("initialize Connect ingress: %w", err)
		}
		statusHandler = func(w http.ResponseWriter, r *http.Request) {
			connectRouter.statusWithWarpStatus(w, r, options.WarpStatus)
		}
		routes = append(routes, router.NewRoute("GET", "/", connectRouter.Connect))
		privateProfile, privateProfileErr := startPrivateHeapProfile(runCtx, options.PrivateHeapProfileTarget, exchange, connectRouter.connectHandler)
		if privateProfileErr != nil {
			// Optional diagnostics must not gate serving or expose private paths.
			glog.Errorf("[connect]optional private heap diagnostics unavailable; continuing primary startup\n")
		}
		defer privateProfile.Close()
		server.Warmup(connectWarmupTargets()...)
		capture, captureErr := startArinShadowCaptureRuntime(runCtx)
		if captureErr != nil {
			// Optional diagnostic authority must not gate primary serving.
			glog.Errorf("[arin-shadow]optional capture unavailable; continuing primary service startup\n")
		}
		defer capture.Close()
		// Only admitted candidates publish a process-identity metrics cohort.
		startStatsPusher(runCtx)
	}
	routes = append([]*router.Route{router.NewRoute("GET", "/status", statusHandler)}, routes...)
	stopStartup()

	draining := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			options.WarpStatus.SetDrainingIfReady()
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

	glog.Infof("[connect]serving %s %s on *:%d\n", server.RequireEnv(), server.RequireVersion(), options.Port)
	err := listenAndServe(
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
