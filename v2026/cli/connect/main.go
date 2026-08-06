package main

import (
	"context"
	// "fmt"
	"net"
	// "net/http"
	"os"
	"strconv"
	// "strings"
	"runtime"
	"syscall"
	"time"

	"github.com/docopt/docopt-go"
	// "github.com/prometheus/client_golang/prometheus"
	// "github.com/prometheus/client_golang/prometheus/push"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"
	connectserver "github.com/urnetwork/server/v2026/connect"
	// "github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/router"
)

func main() {
	usage := `BringYour connect server.

Usage:
  connect [--port=<port>]
  connect -h | --help
  connect --version

Options:
  -h --help     Show this screen.
  --version     Show version.
  -p --port=<port>  Listen port [default: 80].`

	opts, err := docopt.ParseArgs(usage, os.Args[1:], server.RequireVersion())
	if err != nil {
		panic(err)
	}

	// use up to a 16gib message pool per instance
	connect.ResizeMessagePools(connect.Gib(16))

	// server.Logger().Printf("%s\n", opts)

	quitEvent := server.NewEventWithContext(context.Background())
	defer quitEvent.Set()

	closeFn := quitEvent.SetOnSignals(syscall.SIGQUIT, syscall.SIGTERM)
	defer closeFn()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	routes := []*router.Route{
		router.NewRoute("GET", "/status", router.WarpStatus),
	}

	// one-shot readiness latch before listen (same pattern as api/taskworker,
	// TASKDRAIN1 §2.2 / APIDRAIN1 §2.1): a container that cannot reach
	// pg/redis serves only the latched `error not ready ...` status and hosts
	// NO exchange — the deploy poll fails for the full window and warpctl
	// reverts while the old container keeps its residents. Never exit instead
	// (that flaps the container without the truthful status). Warmup is
	// skipped when not ready: it hard-depends on pg and would panic into a
	// crash loop.
	var exchange *connectserver.Exchange
	if err := router.StartupReadiness(ctx); err != nil {
		glog.Infof("[connect]not ready (%s)\n", err)
	} else {
		exchange = connectserver.NewExchangeFromEnvWithDefaults(ctx)
		defer exchange.Close()

		connectRouter := connectserver.NewConnectRouterWithDefaults(ctx, cancel, exchange)
		routes = append(routes,
			router.NewRoute("GET", "/", connectRouter.Connect),
			// router.NewRoute("CONNECT", "", connectRouter.ProxyConnect),
		)

		server.Warmup()
	}

	// drain on sigterm
	go server.HandleError(func() {
		defer cancel()
		select {
		case <-ctx.Done():
		case <-quitEvent.Ctx.Done():
			// flip /status so pollers and dashboards see a truthful draining
			// signal through the migrate/evict window (deliberately not an
			// error status). IfReady: a not-ready container keeps reporting
			// its error through SIGTERM instead of hiding it
			router.SetWarpStatusDrainingIfReady()
			if exchange != nil {
				exchange.Drain()
			}
		}
	})

	// Debugging
	go server.HandleError(func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(30 * time.Second):
			}

			glog.Infof("[connect]goroutines=%d/%d\n", runtime.NumGoroutine(), runtime.GOMAXPROCS(0))
		}
	})

	port, _ := opts.Int("--port")

	glog.Infof(
		"[connect]serving %s %s on *:%d\n",
		server.RequireEnv(),
		server.RequireVersion(),
		port,
	)

	server.StartStatsPusher(ctx)

	// rateLimitHandler := NewConnectionHandlerRateLimitWithDefaults(quitEvent.Ctx, handlerId)
	// defer rateLimitHandler.Close()

	listenIpv4, _, listenPort := server.RequireListenIpPort(port)

	reusePort := false

	httpServerOptions := server.HttpServerOptions{
		ReadTimeout:     15 * time.Second,
		WriteTimeout:    30 * time.Second,
		IdleTimeout:     5 * time.Minute,
		ShutdownTimeout: 30 * time.Second,
	}

	err = server.HttpListenAndServeWithReusePort(
		ctx,
		net.JoinHostPort(listenIpv4, strconv.Itoa(listenPort)),
		router.NewRouter(ctx, routes),
		reusePort,
		httpServerOptions,
	)
	if err != nil {
		panic(err)
	}
	glog.Infof("[connect]close\n")
}
