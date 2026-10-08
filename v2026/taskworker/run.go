package taskworker

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
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
	Port            int
	Count           int
	BatchSize       int
	WorkloadProfile WorkloadProfile
	// Optional owners for a process composing multiple service runners.
	WarpStatus       *router.WarpStatusState
	StartStatsPusher func(context.Context) func()
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
	return self.WorkloadProfile.Validate()
}

// Run serves the production taskworker module until ctx is canceled. It keeps
// the command and integration harness on the same readiness, claim, and final
// handback implementation.
func Run(ctx context.Context, options RunOptions) error {
	startStatsPusher := options.StartStatsPusher
	if startStatsPusher == nil {
		startStatsPusher = server.StartStatsPusher
	}
	return runWithDependencies(
		ctx,
		options,
		router.CheckStartupReadiness,
		startStatsPusher,
		server.HttpListenAndServeWithReusePort,
		startTaskworkerRuntime,
	)
}

// taskworkerRuntime is the lifecycle surface needed after startup. Tests use
// it to prove terminal metrics flush ordering without opening a database.
type taskworkerRuntime interface {
	InflightCount() int
	DrainCanceledCount() int
	Drain()
	WaitFinalHandback() bool
}

// startTaskworkerRuntime initializes the registered tasks, queue collector,
// and execution loops after readiness has admitted this process.
func startTaskworkerRuntime(admission context.Context, ctx context.Context, cancel context.CancelFunc, options RunOptions) (runtime taskworkerRuntime, returnErr error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			if err, ok := recovered.(error); ok {
				returnErr = fmt.Errorf("taskworker initialization: %w", err)
			} else {
				returnErr = fmt.Errorf("taskworker initialization: %v", recovered)
			}
		}
	}()
	if err := admission.Err(); err != nil {
		return nil, err
	}
	// Initialization only rearms the existing idempotent RunOnce identities.
	// Keep its reads/transactions cancelable before starting lifetime workers.
	if err := initTaskScheduleForProfile(admission, options.WorkloadProfile); err != nil {
		return nil, err
	}
	if err := admission.Err(); err != nil {
		return nil, err
	}
	settings := task.DefaultTaskWorkerSettings()
	settings.BatchSize = options.BatchSize
	worker, err := InitTaskWorkerForProfile(ctx, settings, options.WorkloadProfile)
	if err != nil {
		return nil, err
	}
	// Once constructed, the original worker belongs to the drain owner even
	// if later observer startup fails. It must never become another attempt.
	runtime = worker
	// Retention is a best-effort lifetime observer. Its local reaper must not
	// inherit the finite scheduling attempt, and storage I/O cannot gate work.
	go server.HandleError(func() { controller.ApplyBlobRetention(ctx) })
	controller.StartStatsCollector(ctx)
	task.StartQueueMetrics(ctx)
	for range options.Count {
		go server.HandleError(func() {
			defer cancel()
			for {
				server.HandleError(worker.Run)
				select {
				case <-ctx.Done():
					return
				case <-time.After(time.Second):
				}
			}
		})
	}
	return worker, nil
}

func runWithDependencies(
	ctx context.Context,
	options RunOptions,
	readiness func(context.Context) error,
	startStatsPusher func(context.Context) func(),
	listenAndServe func(context.Context, string, http.Handler, bool, server.HttpServerOptions) error,
	startRuntime func(context.Context, context.Context, context.CancelFunc, RunOptions) (taskworkerRuntime, error),
) error {
	return runWithDependenciesAndDrainLogger(ctx, options, readiness, startStatsPusher, listenAndServe, startRuntime, glog.Infof)
}

func runWithDependenciesAndDrainLogger(
	ctx context.Context,
	options RunOptions,
	readiness func(context.Context) error,
	startStatsPusher func(context.Context) func(),
	listenAndServe func(context.Context, string, http.Handler, bool, server.HttpServerOptions) error,
	startRuntime func(context.Context, context.Context, context.CancelFunc, RunOptions) (taskworkerRuntime, error),
	drainLogf func(string, ...any),
) error {
	return runWithStartupWait(ctx, options, readiness, startStatsPusher, listenAndServe, startRuntime, drainLogf, waitTaskworkerStartup)
}

// Startup owns bounded dependency reads while the closed status remains
// available. The same drain path joins startup before using its runtime.
func runWithStartupWait(
	ctx context.Context,
	options RunOptions,
	readiness func(context.Context) error,
	startStatsPusher func(context.Context) func(),
	listenAndServe func(context.Context, string, http.Handler, bool, server.HttpServerOptions) error,
	startRuntime func(context.Context, context.Context, context.CancelFunc, RunOptions) (taskworkerRuntime, error),
	drainLogf func(string, ...any),
	startupWait func(context.Context, time.Duration) error,
) error {
	if ctx == nil {
		return errors.New("taskworker run context is nil")
	}
	if err := options.Validate(); err != nil {
		return err
	}
	// Journald/stdout backpressure must never delay drain, cancellation, or
	// final metric handback. This logger is deliberately best effort at exit.
	logDrainAsync := func(format string, args ...any) {
		if drainLogf != nil {
			go drainLogf(format, args...)
		}
	}
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	glog.Infof("[taskworker]starting %s %s %d task workers with batch size %d\n", server.RequireEnv(), server.RequireVersion(), options.Count, options.BatchSize)

	admission, cancelAdmission := context.WithCancel(ctx)
	defer cancelAdmission()
	readyGauge.Set(0)
	options.WarpStatus.SetNotReady(errors.New("startup dependency checks pending"))
	var startup taskworkerStartupResult
	startupDone := make(chan struct{})
	go func() {
		defer close(startupDone)
		startup = startTaskworkerAfterReadiness(admission, runCtx, cancel, options, readiness, startStatsPusher, startRuntime, startupWait)
	}()
	shutdown := make(chan struct{})
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		select {
		case <-ctx.Done():
		case <-shutdown:
		}
		cancelAdmission()
		<-startupDone
		options.WarpStatus.SetDrainingIfReady()
		readyGauge.Set(0)
		if worker := startup.worker; worker != nil {
			drainStart := time.Now()
			inflight := worker.InflightCount()
			drainInflightGauge.Set(float64(inflight))
			logDrainAsync("[taskworker]drain start with %d in flight\n", inflight)
			worker.Drain()
			if !worker.WaitFinalHandback() {
				logDrainAsync("[taskworker]final handback grace ended with %d tasks still running; claims remain leased\n", worker.InflightCount())
			}
			drainSecondsGauge.Set(time.Since(drainStart).Seconds())
			drainCanceledGauge.Set(float64(worker.DrainCanceledCount()))
		}
		cancel()
	}()

	glog.Infof("[taskworker]serving %s %s on *:%d\n", server.RequireEnv(), server.RequireVersion(), options.Port)
	listenIPv4, _, listenPort := server.RequireListenIpPort(options.Port)
	err := listenAndServe(
		runCtx,
		net.JoinHostPort(listenIPv4, strconv.Itoa(listenPort)),
		router.NewRouter(runCtx, []*router.Route{router.NewRoute("GET", "/status", options.WarpStatus.Handler)}),
		false,
		server.HttpServerOptions{
			ReadTimeout:     15 * time.Second,
			WriteTimeout:    30 * time.Second,
			IdleTimeout:     5 * time.Minute,
			ShutdownTimeout: 30 * time.Second,
		},
	)
	unavailable := err != nil && runCtx.Err() == nil && ctx.Err() == nil
	close(shutdown)
	<-drained
	if err != nil {
		logDrainAsync("[taskworker]status server shutdown error (%s)\n", err)
	}
	// Drain and final claim handback have completed before runCtx is canceled.
	// Push once more so those terminal execution, queue, and drain samples are
	// not lost with the process.
	if startup.flushStats != nil {
		startup.flushStats()
	}
	if startup.closeCapture != nil {
		startup.closeCapture()
	}
	logDrainAsync("[taskworker]close\n")
	if unavailable {
		return err
	}
	if startup.worker != nil && startup.err != nil {
		return startup.err
	}
	return nil
}
