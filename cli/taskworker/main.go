package main

import (
	"context"
	"net"
	"os"
	"strconv"
	"syscall"
	"time"

	"github.com/docopt/docopt-go"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/task"
	"github.com/urnetwork/server/taskworker"
)

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

		taskworker.InitTasks(ctx)

		// one TaskWorker can be shared with many go routines calling EvalTasks
		settings := task.DefaultTaskWorkerSettings()
		settings.BatchSize = batchSize
		taskWorker := taskworker.InitTaskWorker(ctx)
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

		// drain on sigterm
		go server.HandleError(func() {
			defer cancel()
			select {
			case <-ctx.Done():
				return
			case <-quitEvent.Ctx.Done():
				taskWorker.Drain()
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
		if err != nil {
			panic(err)
		}
		glog.Infof("[taskworker]close\n")
	}
}
