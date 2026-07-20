package main

import (
	"context"
	"errors"
	"net"
	"os"
	"runtime"
	"strconv"
	"syscall"
	"time"

	"github.com/docopt/docopt-go"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/api"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/stats"
)

// readiness metric (APIDRAIN1.md §2.1/§2.5); the drain gauges are the
// service-neutral urnetwork_http_server_* series from the shared http drain
var readyGauge = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: "urnetwork",
	Subsystem: "api",
	Name:      "ready",
	Help:      "1 when the readiness latch passed (pg+redis answered at startup) and the instance is not draining",
})

func init() {
	prometheus.MustRegister(readyGauge)
}

func main() {
	usage := `BringYour API server.

Usage:
  api [--port=<port>]
  api -h | --help
  api --version

Options:
  -h --help     Show this screen.
  --version     Show version.
  -p --port=<port>  Listen port [default: 80].`

	opts, err := docopt.ParseArgs(usage, os.Args[1:], server.RequireVersion())
	if err != nil {
		panic(err)
	}

	quitEvent := server.NewEventWithContext(context.Background())
	defer quitEvent.Set()

	closeFn := quitEvent.SetOnSignals(syscall.SIGQUIT, syscall.SIGTERM)
	defer closeFn()

	// the process ctx outlives the drain: the router stats reporter and the
	// stats pusher keep publishing through the drain window and stop only at
	// exit, after the final flush (APIDRAIN1.md §2.5)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// the serve ctx ends at SIGTERM, which starts the http drain sequence:
	// keepalive retire grace, then graceful shutdown bounded by
	// ShutdownTimeout (APIDRAIN1.md §2.2-2.3)
	serveCtx, serveCancel := context.WithCancel(ctx)
	defer serveCancel()

	go server.HandleError(func() {
		defer serveCancel()
		select {
		case <-serveCtx.Done():
		case <-quitEvent.Ctx.Done():
			// the DNAT flip already happened; flip /status so pollers and
			// dashboards see a truthful draining signal through the grace.
			// IfReady: a not-ready container keeps reporting its error
			// through SIGTERM instead of hiding it behind "draining"
			router.SetWarpStatusDrainingIfReady()
			readyGauge.Set(0)
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

			glog.Infof("[api]goroutines=%d/%d\n", runtime.NumGoroutine(), runtime.GOMAXPROCS(0))
		}
	})

	// FindProviders2 sample stats: enabled when the site dir is present and (in
	// non-local envs) a vault stats.yml salt is configured; ships finalized
	// segments to minio when vault minio.yml is present. All best-effort — no-op
	// when unconfigured, never blocks the api.
	statsHandle := stats.Enable(ctx, nil)
	defer statsHandle.Close()
	if _, err := statsHandle.StartUpload(nil); err != nil {
		glog.Infof("[api]stats upload init err=%s\n", err)
	}

	routes := api.Routes()

	port, _ := opts.Int("--port")

	if err := api.ReadinessCheck(ctx); err != nil {
		// serve /status with a latched error and skip warmup: the deploy
		// poll reads `error not ready ...`, times out, and reverts to the
		// old container (APIDRAIN1.md §2.1). Exiting instead would flap the
		// container without ever producing the truthful status. Warmup is
		// skipped because it hits the same dependencies; if traffic still
		// lands here (a restart in place, where the DNAT already points at
		// this container), the lazy init paths serve best-effort.
		glog.Infof("[api]not ready (%s)\n", err)
		router.SetWarpStatusNotReady(err)
		readyGauge.Set(0)
	} else if quitEvent.IsSet() {
		// SIGTERM landed during readiness/warmup: the signal goroutine
		// already latched draining — do not overwrite it with "ok"; the
		// serve below immediately drains
		server.Warmup()
	} else {
		server.Warmup()
		router.SetWarpStatusReady()
		readyGauge.Set(1)
		if quitEvent.IsSet() {
			// SIGTERM raced the ready latch (the signal goroutine's flip
			// ran before the Store above): restore the truthful status
			router.SetWarpStatusDrainingIfReady()
			readyGauge.Set(0)
		}
	}

	flushStats := server.StartStatsPusher(ctx)

	glog.Infof(
		"[api]serving %s %s on *:%d\n",
		server.RequireEnv(),
		server.RequireVersion(),
		port,
	)

	listenIpv4, _, listenPort := server.RequireListenIpPort(port)

	reusePort := false

	// ShutdownTimeout exceeds the max request lifetime
	// (~ReadTimeout+WriteTimeout) so the drain deadline can only cut a
	// non-conforming connection; `Shutdown` returns the moment the last
	// connection drains, so the higher ceiling is not idle time
	// (APIDRAIN1.md §2.2)
	httpServerOptions := server.HttpServerOptions{
		ReadTimeout:           15 * time.Second,
		WriteTimeout:          30 * time.Second,
		IdleTimeout:           5 * time.Minute,
		ShutdownTimeout:       60 * time.Second,
		KeepaliveDrainTimeout: 10 * time.Second,
	}

	apiRouter := router.NewRouter(ctx, routes)

	err = server.HttpListenAndServeWithReusePort(
		serveCtx,
		net.JoinHostPort(listenIpv4, strconv.Itoa(listenPort)),
		apiRouter,
		reusePort,
		httpServerOptions,
	)
	var drainCut *server.HttpDrainCutError
	if err != nil && !errors.As(err, &drainCut) {
		// a listen/serve failure, not a drain outcome: fail loudly so the
		// deploy poll reverts
		panic(err)
	}
	// the drain outcome (clean or cut) is logged and counted by the drain
	// path; report the final window's requests and push the drain gauges
	// before they leave with the process
	apiRouter.FlushStats()
	flushStats()
	glog.Infof("[api]close\n")
}
