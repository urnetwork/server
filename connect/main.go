package main

import (
	"context"
	// "fmt"
	// "net/http"
	"net"
	"os"
	"strconv"
	"syscall"
	"time"

	"github.com/docopt/docopt-go"
	// "github.com/prometheus/client_golang/prometheus"
	// "github.com/prometheus/client_golang/prometheus/push"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
)

func main() {
	usage := `BringYour task worker.

Usage:
  connect [--port=<port>]
  connect -h | --help
  connect --version

Options:
  -h --help     Show this screen.
  --version     Show version.
  -p --port=<port>  Listen port [default: 80].`

	opts, err := docopt.ParseArgs(usage, os.Args[1:], server.RequireVersion())
	if err != nil {
		panic(err)
	}

	// server.Logger().Printf("%s\n", opts)

	quitEvent := server.NewEventWithContext(context.Background())
	defer quitEvent.Set()

	closeFn := quitEvent.SetOnSignals(syscall.SIGQUIT, syscall.SIGTERM)
	defer closeFn()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	exchange := NewExchangeFromEnvWithDefaults(ctx)
	defer exchange.Close()

	handlerId := model.CreateNetworkClientHandler(ctx)

	connectHandler := NewConnectHandlerWithDefaults(ctx, handlerId, exchange)
	// update the heartbeat
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(model.NetworkClientHandlerHeartbeatTimeout):
			}
			// try again after unhandled errors. these signal a transient issue such as db load
			server.HandleError(func() {
				err := model.HeartbeatNetworkClientHandler(ctx, handlerId)
				if err != nil {
					// shut down
					cancel()
				}
			})
		}
	}()

	// drain on sigterm
	go func() {
		defer cancel()
		select {
		case <-ctx.Done():
			return
		case <-quitEvent.Ctx.Done():
			exchange.Drain()
		}
	}()

	routes := []*router.Route{
		router.NewRoute("GET", "/status", router.WarpStatus),
		router.NewRoute("GET", "/", connectHandler.Connect),
	}

	port, _ := opts.Int("--port")

	server.Warmup()

	glog.Infof(
		"[connect]serving %s %s on *:%d\n",
		server.RequireEnv(),
		server.RequireVersion(),
		port,
	)

	// if os.Getenv("SKIP_METRICS") == "" {
	// 	pushMetrics := push.New("push-gateway.cluster.bringyour.dev", "my_job").
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

	// rateLimitHandler := NewConnectionHandlerRateLimitWithDefaults(quitEvent.Ctx, handlerId)
	// defer rateLimitHandler.Close()

	listenIpv4, _, listenPort := server.RequireListenIpPort(port)

	reusePort := false

	err = server.HttpListenAndServeWithReusePort(
		ctx,
		net.JoinHostPort(listenIpv4, strconv.Itoa(listenPort)),
		router.NewRouter(ctx, routes),
		reusePort,
	)
	if err != nil {
		panic(err)
	}
	glog.Infof("[connect]close\n")
}
