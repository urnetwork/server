package main

import (
	"context"
	"flag"
	"net"
	"runtime"
	"strconv"
	"syscall"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/mcp"
	"github.com/urnetwork/server/router"
)

const DrainTimeout = 60 * time.Second

var (
	host = flag.String("host", "0.0.0.0", "host to listen on")
	port = flag.Int("port", 80, "port number to listen on")
	p    = flag.Int("p", 80, "port number to listen on (short form)")
)

func main() {
	flag.Parse()

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

	// Debugging - log goroutine count
	go server.HandleError(func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(30 * time.Second):
			}

			// `go_goroutines` from the default prometheus registry carries this
			// to grafana already; the periodic line is redundant volume
			if glog.V(1) {
				glog.Infof("[mcp]goroutines=%d/%d\n", runtime.NumGoroutine(), runtime.GOMAXPROCS(0))
			}
		}
	})

	server.Warmup()

	glog.Infof(
		"[mcp]serving %s %s on %s:%d\n",
		server.RequireEnv(),
		server.RequireVersion(),
		*host,
		*port,
	)

	routes := mcp.Routes()

	listenIpv4, _, listenPort := server.RequireListenIpPort(*port)

	reusePort := false

	err := server.HttpListenAndServeWithReusePort(
		ctx,
		net.JoinHostPort(listenIpv4, strconv.Itoa(listenPort)),
		router.NewRouter(ctx, routes),
		reusePort,
		mcp.HttpServerOptions(),
	)
	if err != nil {
		glog.Fatalf("[mcp]Server failed: %v", err)
	}
	glog.Infof("[mcp]close\n")
}
