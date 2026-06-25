package main

import (
	"context"
	"net"
	"os"
	"strconv"
	"syscall"
	"time"

	"github.com/docopt/docopt-go"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/glog"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/proxy"
	"github.com/urnetwork/server/router"
)

// this value is set via the linker, e.g.
// -ldflags "-X main.Version=$WARP_VERSION-$WARP_VERSION_CODE"
var Version string

func main() {
	usage := `BringYour proxy server.

Usage:
  proxy [--port=<port>]
  proxy -h | --help
  proxy --version

Options:
  -h --help     Show this screen.
  --version     Show version.
  -p --port=<port>  Listen port [default: 80].`

	opts, err := docopt.ParseArgs(usage, os.Args[1:], server.RequireVersion())
	if err != nil {
		panic(err)
	}

	settings := proxy.DefaultProxySettings()

	// use up to a 16gib message pool per instance
	connect.ResizeMessagePools(connect.Gib(16))

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
		case <-quitEvent.Ctx.Done():
			// FIXME drain
		}
	})

	proxyDeviceManager := proxy.NewProxyDeviceManagerWithDefaults(ctx)
	defer proxyDeviceManager.Close()

	transportTls, err := server.NewTransportTlsFromConfigWithDefaults()
	if err != nil {
		panic(err)
	}

	port, _ := opts.Int("--port")

	server.Warmup()

	proxy.NewSocks5Server(
		ctx,
		cancel,
		proxyDeviceManager,
		transportTls,
		settings,
	)

	proxy.NewHttpServer(
		ctx,
		cancel,
		proxyDeviceManager,
		transportTls,
		settings,
	)

	wg := proxy.NewWgServer(
		ctx,
		cancel,
		proxyDeviceManager,
		settings,
	)

	warmup := func(proxyClient *model.ProxyClient) error {
		return wg.AddProxyClients(proxyClient)
	}

	proxy.NewApiServer(
		ctx,
		cancel,
		proxyDeviceManager,
		transportTls,
		warmup,
		proxy.InternalApiPort,
		settings,
	)

	// if server.RequireEnv() != "local" {
	// 	newWatchdog(
	// 		ctx,
	// 		5*time.Second,
	// 		InternalHttpPort,
	// 	)
	// }

	notif := proxy.NewProxyClientNotification(ctx, settings)
	sub := notif.AddProxyClientsCallback(func(proxyClients []*model.ProxyClient) error {
		if 0 < settings.WarmupTimeout {
			warmupStartTime := server.NowUtc().Add(-settings.WarmupTimeout)
			for _, proxyClient := range proxyClients {
				if warmupStartTime.Before(proxyClient.CreateTime) {
					// warmup the device
					_, err := proxyDeviceManager.OpenProxyDevice(proxyClient.ProxyId)
					if err != nil {
						glog.Infof("[proxy][%s]warmup err=%s\n", proxyClient.ProxyId, err)
					} else {
						glog.Infof("[proxy][%s]warmup\n", proxyClient.ProxyId)
					}
				}
			}
		}

		// a returned error means the notification re-delivers these clients on
		// the next poll instead of advancing past them
		err := wg.AddProxyClients(proxyClients...)
		if err != nil {
			glog.Infof("[proxy]wg add proxy clients err=%s\n", err)
		}
		return err
	})
	defer sub()

	// periodically reconcile the wg peers against the full set of proxy
	// clients for this host/block
	fullSyncSub := notif.AddProxyClientsFullSyncCallback(func(proxyClients []*model.ProxyClient, syncStartTime time.Time) {
		err := wg.SyncProxyClients(proxyClients, syncStartTime)
		if err != nil {
			glog.Infof("[proxy]wg sync proxy clients err=%s\n", err)
		}
	})
	defer fullSyncSub()

	routes := []*router.Route{
		router.NewRoute("GET", "/status", router.WarpStatus),
	}

	listenIpv4, _, listenPort := server.RequireListenIpPort(port)

	glog.Infof(
		"Listen %s:%d, api (:%d), socks5 (:%d), http (:%d), https (:%d)",
		listenIpv4,
		listenPort,
		proxy.InternalApiPort,
		proxy.InternalSocksPort,
		proxy.InternalHttpPort,
		proxy.InternalHttpsPort,
	)

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

	select {
	case <-ctx.Done():
	}

	os.Exit(1)
}
