package mcp

// Production lifecycle for the MCP service.

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
}

// Validate rejects invalid settings before configuration or network access.
func (self RunOptions) Validate() error {
	if self.Port < 1 || self.Port > 65_535 {
		return fmt.Errorf("mcp port %d is outside [1,65535]", self.Port)
	}
	return nil
}

// Run serves the production MCP module until ctx is canceled. The command and
// tests share this readiness, metric-pusher, listener, and drain lifecycle.
func Run(ctx context.Context, options RunOptions) error {
	return runWithDependencies(ctx, options, Startup, server.StartStatsPusher, server.HttpListenAndServeWithReusePort, Routes)
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

	glog.Infof("[mcp]serving %s %s on *:%d\n", server.RequireEnv(), server.RequireVersion(), options.Port)
	listenIpv4, _, listenPort := server.RequireListenIpPort(options.Port)
	mcpRouter := router.NewRouter(processCtx, routes())
	err := listenAndServe(
		serveCtx,
		net.JoinHostPort(listenIpv4, strconv.Itoa(listenPort)),
		mcpRouter,
		false,
		HttpServerOptions(),
	)
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
