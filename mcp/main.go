package main

import (
	"context"
	// "fmt"
	// "net/http"
	"os"
	"syscall"

	// "net"
	// "errors"
	"net"
	"runtime"
	"strconv"

	"time"

	"github.com/docopt/docopt-go"
	// "github.com/prometheus/client_golang/prometheus"
	// "github.com/prometheus/client_golang/prometheus/push"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/router"
)

const DrainTimeout = 60 * time.Second

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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// drain on sigterm
	go server.HandleError(func() {
		defer cancel()
		select {
		case <-ctx.Done():
			return
		case <-quitEvent.Ctx.Done():
			select {
			case <-ctx.Done():
				return
			case <-time.After(DrainTimeout):
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

			glog.Infof("[api]goroutines=%d/%d\n", runtime.NumGoroutine(), runtime.GOMAXPROCS(0))
		}
	})

	routes := []*router.Route{
		router.NewRoute("GET", "/status", router.WarpStatus),
	}

	// server.().Printf("%s\n", opts)

	port, _ := opts.Int("--port")

	server.Warmup()

	glog.Infof(
		"[api]serving %s %s on *:%d\n",
		server.RequireEnv(),
		server.RequireVersion(),
		port,
	)

	// if os.Getenv("SKIP_METRICS") == "" {
	// 	pushMetrics := push.New("https://push-gateway.cluster.bringyour.dev", "my_job").
	// 		Gatherer(prometheus.DefaultGatherer).
	// 		Grouping("warp_block", server.RequireBlock()).
	// 		Grouping("warp_env", server.RequireEnv()).
	// 		Grouping("warp_version", server.RequireVersion()).
	// 		Grouping("warp_service", server.RequireService()).
	// 		Grouping("warp_config_version", server.RequireConfigVersion()).
	// 		Grouping("warp_host", server.RequireHost())

	// 	go func() {
	// 		for {
	// 			select {
	// 			case <-quitEvent.Ctx.Done():
	// 				return
	// 			case <-time.NewTicker(30 * time.Second).C:
	// 				err := pushMetrics.Push()
	// 				if err != nil {
	// 					glog.Errorf("[api]pushMetrics.Push = %s\n", err)
	// 				}
	// 			}
	// 		}
	// 	}()
	// }

	listenIpv4, _, listenPort := server.RequireListenIpPort(port)

	reusePort := false

	httpServerOptions := server.HttpServerOptions{
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  5 * time.Minute,
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
	glog.Infof("[api]close\n")
}
