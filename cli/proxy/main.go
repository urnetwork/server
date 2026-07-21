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

	// use up to a 8gib message pool per instance
	connect.ResizeMessagePools(connect.Gib(8))

	quitEvent := server.NewEventWithContext(context.Background())
	defer quitEvent.Set()

	closeFn := quitEvent.SetOnSignals(syscall.SIGQUIT, syscall.SIGTERM)
	defer closeFn()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// readiness gates /status (PROXYDRAIN1.md §3.1): warpctl's new-container
	// health poll gets a 503 until the initial proxy client sync has been
	// applied, so the DNAT flip cannot beat the wg peer restore
	readiness := proxy.NewReadiness()

	// the drain coordinator runs the graceful shutdown on SIGTERM
	// (PROXYDRAIN1.md §3.2); ingress targets are registered below
	drainCoordinator := proxy.NewDrainCoordinator(readiness, settings)

	// drain on sigterm, then cancel: established socks/http tunnels and the
	// (conntrack-pinned) wg flows are served through the drain grace, and the
	// process exits the moment the drain completes rather than idling toward
	// the `docker stop -t` ceiling
	go server.HandleError(func() {
		defer cancel()
		select {
		case <-ctx.Done():
		case <-quitEvent.Ctx.Done():
			drainCoordinator.Drain(ctx)
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

	server.StartStatsPusher(ctx)

	socks5Server := proxy.NewSocks5Server(
		ctx,
		cancel,
		proxyDeviceManager,
		transportTls,
		settings,
	)
	drainCoordinator.AddTarget(socks5Server)

	httpServer := proxy.NewHttpServer(
		ctx,
		cancel,
		proxyDeviceManager,
		transportTls,
		settings,
	)
	drainCoordinator.AddTarget(httpServer)

	// the wg server is NOT a drain target: its conntrack-pinned clients
	// cannot migrate until this process exits and warpctl flushes their
	// entries, so it serves until the final teardown (PROXYDRAIN1.md §3.2)
	wg := proxy.NewWgServer(
		ctx,
		cancel,
		proxyDeviceManager,
		settings,
	)
	// Advertise this replacement generation before readiness. The old
	// instance tags its drain-end export for this generation, and the
	// post-readiness sequence below waits for it (the drain-complete
	// beacon) before pre-warming.
	wg.PrepareWgHandoff(ctx)
	// at drain end, export the active wg peers' endpoints so the replacement
	// instance can re-establish their sessions from the server side
	// (PROXYDRAIN1.md §3.4)
	drainCoordinator.AddBeforeExit(wg.ExportWgHandoff)

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

	// the device rpc endpoint (a DeviceRemote such as a browser connects here
	// directly with the device's signed proxy id to control the hosted device)
	// is served on GET /device-rpc by the proxy api TLS listener (NewApiServer).

	// flush this instance's recently active proxy ids so a replacement
	// instance can pre-warm them during a deploy (PROXYDRAIN1.md §3.3)
	proxy.StartActivityFlusher(ctx, proxyDeviceManager, settings)

	notif := proxy.NewProxyClientNotification(ctx, settings)
	// flip readiness once the initial sync has been applied (wg peers
	// restored); until then /status returns 503 and warpctl will not flip
	// traffic to this instance. Readiness is gated on the sync ONLY — the
	// handoff/pre-warm sequence below never holds up the flip.
	go server.HandleError(func() {
		select {
		case <-ctx.Done():
			return
		case <-notif.InitialSyncDone():
			glog.Infof("[proxy]initial proxy client sync applied; ready\n")
			readiness.SetInitialSyncDone()
		}
		// Consume the drained instance's endpoint handoff and re-establish
		// its wg sessions from the server side (PROXYDRAIN1.md §3.4).
		// ApplyWgHandoff blocks until the old instance's generation-tagged
		// export appears — written at drain END, so it doubles as the old
		// instance's drain-complete beacon (an empty peer set is the
		// completion marker on no-peer deploys) — or until the poll budget
		// expires (the old-instance-crashed fallback). Only then does the
		// pre-warm run. Sequencing the pre-warm AFTER the handoff outcome,
		// instead of racing the old instance's drain grace, keeps the reused
		// persisted window identities from running live in BOTH containers
		// during the grace, where the old side's live-ctx window eviction
		// could api-remove the exact identity this side is using
		// (REVIEW2-UPDATE1 §4.4). The accepted trade: the pre-warmed set
		// turns warm at old-drain-end rather than at flip. Customer-driven
		// lazy device opens are NOT gated — only the forced pre-warm
		// establishment is — and the wg endpoint seeding lands before the
		// pre-warmed devices establish, in the order the packets will flow.
		// The handoff runs in its own error scope so a handoff failure
		// still falls through to the pre-warm.
		server.HandleError(func() {
			wg.ApplyWgHandoff(ctx)
		})
		proxy.Prewarm(ctx, proxyDeviceManager, settings)
	})
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
		router.NewRoute("GET", "/status", readiness.StatusHandler),
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

	if drainCoordinator.Drained() {
		// a graceful drained shutdown; exit promptly and cleanly so `docker
		// stop` returns immediately instead of waiting toward its -t ceiling
		os.Exit(0)
	}
	os.Exit(1)
}
