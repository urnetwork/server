package main

import (
	"context"
	"net"
	"os"
	"runtime"
	"strconv"
	"syscall"
	"time"

	"github.com/docopt/docopt-go"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/api"
	"github.com/urnetwork/server/v2026/router"
)

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

	// drain on sigterm: cancel ctx, which triggers graceful http shutdown
	// (stops accepting new conns and waits up to DrainTimeout for in-flight)
	go server.HandleError(func() {
		defer cancel()
		select {
		case <-ctx.Done():
		case <-quitEvent.Ctx.Done():
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

	routes := api.Routes()

	port, _ := opts.Int("--port")

	server.Warmup()

	glog.Infof(
		"[api]serving %s %s on *:%d\n",
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
