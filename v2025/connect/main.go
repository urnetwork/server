package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"syscall"
	"time"

	"github.com/docopt/docopt-go"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/push"

	"github.com/golang/glog"

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

	exchange := NewExchangeFromEnvWithDefaults(quitEvent.Ctx)
	defer exchange.Close()

	handlerId := model.CreateNetworkClientHandler(quitEvent.Ctx)

	connectHandler := NewConnectHandlerWithDefaults(quitEvent.Ctx, handlerId, exchange)
	// update the heartbeat
	go func() {
		for {
			select {
			case <-quitEvent.Ctx.Done():
				return
			case <-time.After(model.NetworkClientHandlerHeartbeatTimeout):
			}
			err := model.HeartbeatNetworkClientHandler(quitEvent.Ctx, handlerId)
			if err != nil {
				// shut down
				quitEvent.Set()
			}
		}
	}()

	routes := []*router.Route{
		router.NewRoute("GET", "/status", router.WarpStatus),
		router.NewRoute("GET", "/", connectHandler.Connect),
	}

	port, _ := opts.Int("--port")

	glog.Infof(
		"[connect]serving %s %s on *:%d\n",
		server.RequireEnv(),
		server.RequireVersion(),
		port,
	)

	if os.Getenv("SKIP_METRICS") == "" {
		pushMetrics := push.New("push-gateway.cluster.bringyour.dev", "my_job").
			Gatherer(prometheus.DefaultGatherer).
			Grouping("warp_block", server.RequireBlock()).
			Grouping("warp_env", server.RequireEnv()).
			Grouping("warp_version", server.RequireVersion()).
			Grouping("warp_service", server.RequireService()).
			Grouping("warp_config_version", server.RequireConfigVersion()).
			Grouping("warp_host", server.RequireHost())

		go func() {
			for {
				select {
				case <-quitEvent.Ctx.Done():
					return
				case <-time.NewTicker(30 * time.Second).C:
					err := pushMetrics.Push()
					if err != nil {
						glog.Errorf("[api]pushMetrics.Push = %s\n", err)
					}
				}
			}
		}()
	}

	routerHandler := router.NewRouter(quitEvent.Ctx, routes)
	if err := http.ListenAndServe(fmt.Sprintf(":%d", port), routerHandler); err != nil {
		glog.Errorf("[connect]close = %s\n", err)
	}
}
