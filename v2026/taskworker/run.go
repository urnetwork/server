package taskworker

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/router"
	"github.com/urnetwork/server/v2026/task"
)

var readyGauge = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: "urnetwork",
	Subsystem: "taskworker",
	Name:      "ready",
	Help:      "1 when the readiness latch passed and workers are claiming tasks",
})

var drainInflightGauge = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: "urnetwork",
	Subsystem: "taskworker",
	Name:      "drain_inflight",
	Help:      "tasks in flight when the drain started",
})

var drainSecondsGauge = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: "urnetwork",
	Subsystem: "taskworker",
	Name:      "drain_seconds",
	Help:      "how long the drain took",
})

var drainCanceledGauge = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: "urnetwork",
	Subsystem: "taskworker",
	Name:      "drain_canceled",
	Help:      "task executions canceled by the drain and rescheduled with their claims released",
})

func init() {
	prometheus.MustRegister(readyGauge, drainInflightGauge, drainSecondsGauge, drainCanceledGauge)
}

type RunOptions struct {
	Port      int
	Count     int
	BatchSize int
}

func (self RunOptions) Validate() error {
	if self.Port < 1 || self.Port > 65_535 {
		return fmt.Errorf("taskworker port %d is outside [1,65535]", self.Port)
	}
	if self.Count < 1 || self.Count > 1_024 {
		return fmt.Errorf("taskworker count %d is outside [1,1024]", self.Count)
	}
	if self.BatchSize < 1 || self.BatchSize > 1_024 {
		return fmt.Errorf("taskworker batch size %d is outside [1,1024]", self.BatchSize)
	}
	return nil
}

// Run serves the production taskworker module until ctx is canceled. It keeps
// the command and integration harness on the same readiness, claim, and final
// handback implementation.
func Run(ctx context.Context, options RunOptions) error {
	if ctx == nil {
		return errors.New("taskworker run context is nil")
	}
	if err := options.Validate(); err != nil {
		return err
	}
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	glog.Infof("[taskworker]starting %s %s %d task workers with batch size %d\n", server.RequireEnv(), server.RequireVersion(), options.Count, options.BatchSize)
	server.StartStatsPusher(runCtx)
	controller.StartStatsCollector(runCtx)

	var worker *task.TaskWorker
	if err := router.StartupReadiness(runCtx); err != nil {
		glog.Infof("[taskworker]not ready (%s)\n", err)
		readyGauge.Set(0)
	} else {
		InitTasks(runCtx)
		settings := task.DefaultTaskWorkerSettings()
		settings.BatchSize = options.BatchSize
		worker = InitTaskWorkerWithSettings(runCtx, settings)
		for index := 0; index < options.Count; index++ {
			go server.HandleError(func() {
				defer cancel()
				for {
					server.HandleError(worker.Run)
					select {
					case <-runCtx.Done():
						return
					case <-time.After(time.Second):
					}
				}
			})
		}
		readyGauge.Set(1)
	}

	draining := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			router.SetWarpStatusDrainingIfReady()
			readyGauge.Set(0)
			if worker != nil {
				drainStart := time.Now()
				inflight := worker.InflightCount()
				drainInflightGauge.Set(float64(inflight))
				glog.Infof("[taskworker]drain start with %d in flight\n", inflight)
				worker.Drain()
				if !worker.WaitFinalHandback() {
					glog.Infof("[taskworker]final handback grace ended with %d tasks still running; claims remain leased\n", worker.InflightCount())
				}
				drainSecondsGauge.Set(time.Since(drainStart).Seconds())
				drainCanceledGauge.Set(float64(worker.DrainCanceledCount()))
			}
			cancel()
		case <-draining:
		}
	}()
	defer close(draining)

	glog.Infof("[taskworker]serving %s %s on *:%d\n", server.RequireEnv(), server.RequireVersion(), options.Port)
	listenIPv4, _, listenPort := server.RequireListenIpPort(options.Port)
	err := server.HttpListenAndServeWithReusePort(
		runCtx,
		net.JoinHostPort(listenIPv4, strconv.Itoa(listenPort)),
		router.NewRouter(runCtx, []*router.Route{router.NewRoute("GET", "/status", router.WarpStatus)}),
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
	if err != nil {
		glog.Infof("[taskworker]status server shutdown error (%s)\n", err)
	}
	glog.Infof("[taskworker]close\n")
	return nil
}
