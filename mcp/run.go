package mcp

// Production lifecycle for the MCP service.
//
// What is the process's rather than an instance's, so a test that runs mcp in one process beside other services
// (e2e-hosts-in-process) sets it once, before mcp serves: the /status latch, router's global one, which Startup and
// Run set; the access tokens' issuer and keys, oauth's (oauth.Testing_SetSignerKeys, oauth.Testing_SetIssuer); the
// resource every token must name, McpResource; and the seal key the tool calls' state rides under, proxy.yml's
// first non-blank secret in production and Testing_SetSealSecret's in a test. RunOptions.Listener gives the server a
// port the OS picked.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/router"
)

// RunOptions are the bounded MCP service settings.
type RunOptions struct {
	Port int
	// A listener already bound, which Run serves in place of binding Port: a test's on 127.0.0.1:0, so the OS picks
	// the port. Run takes it and closes it at the drain. nil: Port, as warp maps it.
	Listener net.Listener
	// The http server's timeouts and drain, a test's shorter grace among them. nil: HttpServerOptions().
	HttpServer *server.HttpServerOptions
	// What readiness runs before /status latches ready. nil: Startup, the migration-aware check then the warmup of
	// what the tools read. A test that runs mcp beside other services gives the check alone, since the warmup loads
	// process-wide state, the IP databases and the location searches, that outlives the server.
	Startup func(ctx context.Context) error
}

// Validate rejects invalid settings before configuration or network access.
func (self RunOptions) Validate() error {
	if self.Listener == nil && (self.Port < 1 || self.Port > 65_535) {
		return fmt.Errorf("mcp port %d is outside [1,65535]", self.Port)
	}
	return nil
}

// Run serves the production MCP module until ctx is canceled. The command and
// tests share this readiness, metric-pusher, listener, and drain lifecycle.
func Run(ctx context.Context, options RunOptions) error {
	startup := options.Startup
	if startup == nil {
		startup = Startup
	}
	return runWithDependencies(ctx, options, startup, server.StartStatsPusher, server.HttpListenAndServeWithReusePort, Routes)
}

// runWithDependencies keeps the externally effectful lifecycle boundaries
// replaceable for deterministic tests.
func runWithDependencies(
	ctx context.Context,
	options RunOptions,
	startup func(context.Context) error,
	startStatsPusher func(context.Context) func(),
	listenAndServe func(context.Context, string, http.Handler, bool, server.HttpServerOptions) error,
	routes func() []*router.Route,
) error {
	if ctx == nil {
		return errors.New("mcp run context is nil")
	}
	if err := options.Validate(); err != nil {
		return err
	}

	processCtx, processCancel := context.WithCancel(context.Background())
	defer processCancel()
	serveCtx, serveCancel := context.WithCancel(processCtx)
	defer serveCancel()

	draining := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			router.SetWarpStatusDrainingIfReady()
			mcpReadyGauge.Set(0)
			serveCancel()
		case <-draining:
		}
	}()
	defer close(draining)

	flushStats := func() {}
	if err := startup(ctx); err != nil {
		glog.Infof("[mcp]not ready (%s)\n", err)
		router.SetWarpStatusNotReady(err)
		mcpReadyGauge.Set(0)
	} else if ctx.Err() == nil {
		router.SetWarpStatusReady()
		mcpReadyGauge.Set(1)
		flushStats = startStatsPusher(processCtx)
	} else {
		router.SetWarpStatusDrainingIfReady()
		mcpReadyGauge.Set(0)
	}

	mcpRouter := router.NewRouter(processCtx, routes())
	httpServerOptions := HttpServerOptions()
	if options.HttpServer != nil {
		httpServerOptions = *options.HttpServer
	}
	var err error
	if options.Listener != nil {
		port := options.Port
		if address, ok := options.Listener.Addr().(*net.TCPAddr); ok {
			port = address.Port
		}
		// the port alone, as for a bound one
		glog.Infof("[mcp]serving on port %d\n", port)
		err = server.HttpServeListener(serveCtx, options.Listener, mcpRouter, httpServerOptions)
	} else {
		glog.Infof("[mcp]serving %s %s on *:%d\n", server.RequireEnv(), server.RequireVersion(), options.Port)
		listenIpv4, _, listenPort := server.RequireListenIpPort(options.Port)
		err = listenAndServe(
			serveCtx,
			net.JoinHostPort(listenIpv4, strconv.Itoa(listenPort)),
			mcpRouter,
			false,
			httpServerOptions,
		)
	}
	if err != nil && serveCtx.Err() == nil {
		return err
	}
	if err != nil {
		glog.Infof("[mcp]server shutdown error (%s)\n", err)
	}
	mcpRouter.FlushStats()
	flushStats()
	glog.Infof("[mcp]close\n")
	return nil
}
