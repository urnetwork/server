package main

import (
	"context"
	"net"
	"os"
	"strconv"
	"syscall"
	"time"

	"github.com/docopt/docopt-go"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/router"
	"github.com/urnetwork/server/v2026/task"
	"github.com/urnetwork/server/v2026/taskworker"
)

// drain/readiness metrics (TASKDRAIN1 §2.5). Pushed via the stats pusher;
// series go stale shortly after the drain because the process exits, so the
// drain summary log lines in `TaskWorker.Drain` are the durable record.
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
	prometheus.MustRegister(
		readyGauge,
		drainInflightGauge,
		drainSecondsGauge,
		drainCanceledGauge,
	)
}

func main() {
	usage := `BringYour task worker.

Usage:
  taskworker [--port=<port>] [--count=<count>] [--batch_size=<batch_size>]
  taskworker init-tasks
  taskworker -h | --help
  taskworker --version

Options:
  -h --help     Show this screen.
  --version     Show version.
  -p --port=<port>  Listen port [default: 80].
  -n --count=<count>  Number of worker processes [default: 8].
  -b --batch_size=<batch_size>  Batch size [default: 4].`

	opts, err := docopt.ParseArgs(usage, os.Args[1:], server.RequireVersion())
	if err != nil {
		panic(err)
	}

	quitEvent := server.NewEventWithContext(context.Background())
	closeFn := quitEvent.SetOnSignals(syscall.SIGQUIT, syscall.SIGTERM)
	defer closeFn()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if initTasks_, _ := opts.Bool("init-tasks"); initTasks_ {
		taskworker.InitTasks(ctx)
	} else {
		// note the total parallelism is count*batch_size
		count, _ := opts.Int("--count")
		batchSize, _ := opts.Int("--batch_size")
		port, _ := opts.Int("--port")

		glog.Infof(
			"[taskworker]starting %s %s %d task workers with batch size %d\n",
			server.RequireEnv(),
			server.RequireVersion(),
			count,
			batchSize,
		)

		server.StartStatsPusher(ctx)
		// the public network stats gauges (the grafana /stats.json feed).
		// only the taskworker runs the collector, so only its pushed
		// registry carries the urnetwork_stats_* series
		controller.StartStatsCollector(ctx)

		var taskWorker *task.TaskWorker
		if err := router.StartupReadiness(ctx); err != nil {
			// /status is latched to `error not ready ...` and NO tasks are
			// claimed: the deploy poll fails, times out, and reverts to the
			// old container (TASKDRAIN1 §2.2). Never exit instead — that
			// flaps the container without ever producing the truthful status.
			glog.Infof("[taskworker]not ready (%s)\n", err)
			readyGauge.Set(0)
		} else {
			taskworker.InitTasks(ctx)

			settings := task.DefaultTaskWorkerSettings()
			settings.BatchSize = batchSize
			taskWorker = taskworker.InitTaskWorkerWithSettings(ctx, settings)
			for i := 0; i < count; i += 1 {
				go server.HandleError(func() {
					defer cancel()
					for {
						// try again after unhandled errors. these signal a transient issue such as db load
						server.HandleError(taskWorker.Run)
						select {
						case <-ctx.Done():
							return
						case <-time.After(1 * time.Second):
						}
					}
				})
			}
			readyGauge.Set(1)
		}

		// drain on sigterm
		go server.HandleError(func() {
			defer cancel()
			select {
			case <-ctx.Done():
				return
			case <-quitEvent.Ctx.Done():
				// IfReady: a not-ready container keeps reporting its error
				// through SIGTERM instead of hiding it behind "draining"
				router.SetWarpStatusDrainingIfReady()
				readyGauge.Set(0)
				if taskWorker != nil {
					drainStartTime := time.Now()
					inflightCount := taskWorker.InflightCount()
					drainInflightGauge.Set(float64(inflightCount))
					glog.Infof("[taskworker]drain start with %d in flight\n", inflightCount)
					taskWorker.Drain()
					if !taskWorker.WaitFinalHandback() {
						glog.Infof(
							"[taskworker]final handback grace ended with %d tasks still running; claims remain leased\n",
							taskWorker.InflightCount(),
						)
					}
					drainSecondsGauge.Set(time.Since(drainStartTime).Seconds())
					drainCanceledGauge.Set(float64(taskWorker.DrainCanceledCount()))
				}
			}
		})

		routes := []*router.Route{
			router.NewRoute("GET", "/status", router.WarpStatus),
		}

		glog.Infof(
			"[taskworker]serving %s %s on *:%d\n",
			server.RequireEnv(),
			server.RequireVersion(),
			port,
		)

		listenIpv4, _, listenPort := server.RequireListenIpPort(port)

		reusePort := false

		httpServerOptions := server.HttpServerOptions{
			ReadTimeout:     15 * time.Second,
			WriteTimeout:    30 * time.Second,
			IdleTimeout:     5 * time.Minute,
			ShutdownTimeout: 30 * time.Second,
		}

		err := server.HttpListenAndServeWithReusePort(
			ctx,
			net.JoinHostPort(listenIpv4, strconv.Itoa(listenPort)),
			router.NewRouter(ctx, routes),
			reusePort,
			httpServerOptions,
		)
		if err != nil && ctx.Err() == nil {
			// a listen/serve failure outside shutdown: fail loudly so the
			// deploy poll reverts
			panic(err)
		}
		if err != nil {
			// a drain-deadline shutdown is a logged outcome, not a panic
			glog.Infof("[taskworker]status server shutdown error (%s)\n", err)
		}
		glog.Infof("[taskworker]close\n")
	}
}
