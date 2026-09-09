package api

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"runtime"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/oauth"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/stats"
)

var readyGauge = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: "urnetwork",
	Subsystem: "api",
	Name:      "ready",
	Help:      "1 when the migration-aware pg+redis startup gate passed and the instance is not draining",
})

func init() {
	prometheus.MustRegister(readyGauge)
}

type RunOptions struct {
	Port int
}

func apiWarmupTargets() []server.WarmupTarget {
	// API serves the complete model/controller surface, including every search
	// and location feature currently registered by the server. Keep this list
	// explicit: a new target must not become resident merely because it exists.
	return []server.WarmupTarget{
		server.WarmupTargetIPDatabase,
		server.WarmupTargetNetworkNameSearch,
		server.WarmupTargetLocationSearch,
		server.WarmupTargetCountryLocations,
		server.WarmupTargetLocationDirectory,
	}
}

func activateAfterReadiness(
	ctx context.Context,
	readiness func(context.Context) error,
	activate func(),
) error {
	if err := readiness(ctx); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	activate()
	return nil
}

func (self RunOptions) Validate() error {
	if self.Port < 1 || self.Port > 65_535 {
		return fmt.Errorf("api port %d is outside [1,65535]", self.Port)
	}
	return nil
}

// Run serves the production API module until ctx is canceled. Production
// commands and integration harnesses share this runner so route construction,
// readiness, warmup, metrics, and drain behavior cannot diverge.
func Run(ctx context.Context, options RunOptions) error {
	return runWithDependencies(ctx, options, ReadinessCheck, server.StartStatsPusher, server.HttpListenAndServeWithReusePort)
}

// The command and tests exercise the same startup and drain wiring. Only the
// external readiness, metrics transport, and HTTP listener are replaceable.
func runWithDependencies(
	ctx context.Context,
	options RunOptions,
	readiness func(context.Context) error,
	startStatsPusher func(context.Context) func(),
	listenAndServe func(context.Context, string, http.Handler, bool, server.HttpServerOptions) error,
) error {
	if ctx == nil {
		return errors.New("api run context is nil")
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
			readyGauge.Set(0)
			serveCancel()
		case <-draining:
		}
	}()
	defer close(draining)

	go server.HandleError(func() {
		for {
			select {
			case <-processCtx.Done():
				return
			case <-time.After(30 * time.Second):
			}
			if glog.V(1) {
				glog.Infof("[api]goroutines=%d/%d\n", runtime.NumGoroutine(), runtime.GOMAXPROCS(0))
			}
		}
	})

	var statsHandle *stats.Stats
	admitted := false
	flushStats := func() {}
	if err := activateAfterReadiness(ctx, readiness, func() {
		server.Warmup(apiWarmupTargets()...)
		controller.StartMetrics(processCtx)
		statsHandle = stats.Enable(processCtx, nil)
		if _, err := statsHandle.StartUpload(nil); err != nil {
			glog.Infof("[api]stats upload init err=%s\n", err)
		}
		oauth.NewReaperWithDefaults(processCtx)
	}); err != nil {
		glog.Infof("[api]not ready (%s)\n", err)
		router.SetWarpStatusNotReady(err)
		readyGauge.Set(0)
	} else if ctx.Err() == nil {
		admitted = true
		router.SetWarpStatusReady()
		readyGauge.Set(1)
		// Rejected candidates keep /status and logs, but must not allocate a
		// fresh process cohort in the remote metrics store on every retry.
		flushStats = startStatsPusher(processCtx)
		if ctx.Err() != nil {
			router.SetWarpStatusDrainingIfReady()
			readyGauge.Set(0)
		}
	} else {
		router.SetWarpStatusDrainingIfReady()
		readyGauge.Set(0)
	}
	if statsHandle != nil {
		defer statsHandle.Close()
	}
	// This bounded cache is separate from mutable JWT account authentication.
	// It owns its real refresh loop before the route is exposed; HTTP drain
	// completes before the deferred close joins leases and closes RPC clients.
	var reservedUpload *controller.StReservedAttemptUpload
	var err error
	if admitted {
		reservedUpload, err = controller.NewStReservedAttemptUpload(ctx)
		if err != nil {
			return fmt.Errorf("reserved validator staging startup: %w", err)
		}
	}
	defer reservedUpload.Close()

	glog.Infof("[api]serving %s %s on *:%d\n", server.RequireEnv(), server.RequireVersion(), options.Port)
	listenIPv4, _, listenPort := server.RequireListenIpPort(options.Port)
	apiRouter := router.NewRouter(processCtx, routesWithReservedAttemptUpload(reservedUpload))
	err = listenAndServe(
		serveCtx,
		net.JoinHostPort(listenIPv4, strconv.Itoa(listenPort)),
		apiRouter,
		false,
		server.HttpServerOptions{
			ReadTimeout:           15 * time.Second,
			WriteTimeout:          30 * time.Second,
			IdleTimeout:           5 * time.Minute,
			ShutdownTimeout:       60 * time.Second,
			KeepaliveDrainTimeout: 10 * time.Second,
		},
	)
	var drainCut *server.HttpDrainCutError
	if err != nil && !errors.As(err, &drainCut) {
		return err
	}
	apiRouter.FlushStats()
	flushStats()
	glog.Infof("[api]close\n")
	return nil
}
