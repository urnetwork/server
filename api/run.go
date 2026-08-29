package api

import (
	"context"
	"errors"
	"fmt"
	"net"
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
	Help:      "1 when the readiness latch passed (pg+redis answered at startup) and the instance is not draining",
})

func init() {
	prometheus.MustRegister(readyGauge)
}

type RunOptions struct {
	Port int
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
	if ctx == nil {
		return errors.New("api run context is nil")
	}
	if err := options.Validate(); err != nil {
		return err
	}

	processCtx, processCancel := context.WithCancel(context.Background())
	defer processCancel()
	controller.StartMetrics(processCtx)
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

	statsHandle := stats.Enable(processCtx, nil)
	defer statsHandle.Close()
	if _, err := statsHandle.StartUpload(nil); err != nil {
		glog.Infof("[api]stats upload init err=%s\n", err)
	}
	oauth.NewReaperWithDefaults(processCtx)

	if err := ReadinessCheck(processCtx); err != nil {
		glog.Infof("[api]not ready (%s)\n", err)
		router.SetWarpStatusNotReady(err)
		readyGauge.Set(0)
	} else if ctx.Err() == nil {
		server.Warmup()
		router.SetWarpStatusReady()
		readyGauge.Set(1)
		if ctx.Err() != nil {
			router.SetWarpStatusDrainingIfReady()
			readyGauge.Set(0)
		}
	} else {
		server.Warmup()
	}

	flushStats := server.StartStatsPusher(processCtx)
	glog.Infof("[api]serving %s %s on *:%d\n", server.RequireEnv(), server.RequireVersion(), options.Port)
	listenIPv4, _, listenPort := server.RequireListenIpPort(options.Port)
	apiRouter := router.NewRouter(processCtx, Routes())
	err := server.HttpListenAndServeWithReusePort(
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
