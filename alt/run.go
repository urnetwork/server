package alt

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"runtime"
	"strconv"
	"time"

	connectcore "github.com/urnetwork/connect"
	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/api"
	connectserver "github.com/urnetwork/server/connect"
	"github.com/urnetwork/server/router"
)

// The public udp service ports (L2). Warp maps them to this block's allocated
// host ports through WARP_PORTS; the router in front of the proxy hosts
// translates public 53 to the whodis port.
const (
	DefaultH3Port  = 443
	DefaultDnsPort = 4053
	// Alt has no load balancer in front, so this http port carries only the
	// warp deploy poll's readiness route.
	DefaultStatusPort = 80
)

type RunOptions struct {
	H3Port  int
	DnsPort int
	// the http port serving the warp status route, and nothing else
	Port int
	// the sni names of each front. Empty derives them from the environment's
	// services config, so production needs no flags.
	ApiHosts     []string
	ConnectHosts []string
	// the tlds the whodis listener decodes. Empty uses the client default.
	DnsTlds []string
}

func (self RunOptions) Validate() error {
	for name, port := range map[string]int{"H3": self.H3Port, "DNS": self.DnsPort, "status": self.Port} {
		if port < 1 || port > 65_535 {
			return fmt.Errorf("alt %s port %d is outside [1,65535]", name, port)
		}
	}
	if self.H3Port == self.DnsPort {
		return fmt.Errorf("alt H3 and DNS ports are both %d", self.H3Port)
	}
	return nil
}

// Fills in the defaults that come from the environment. A host list given
// explicitly is used as is, which is how tests pin synthetic names.
func (self RunOptions) Settings() (*Settings, error) {
	settings := DefaultSettings()
	settings.ApiHosts = self.ApiHosts
	if len(settings.ApiHosts) == 0 {
		apiHosts, err := ServiceHosts(ApiServiceName)
		if err != nil {
			return nil, err
		}
		settings.ApiHosts = apiHosts
	}
	settings.ConnectHosts = self.ConnectHosts
	if len(settings.ConnectHosts) == 0 {
		connectHosts, err := ServiceHosts(ConnectServiceName)
		if err != nil {
			return nil, err
		}
		settings.ConnectHosts = connectHosts
	}
	if 0 < len(self.DnsTlds) {
		settings.DnsTlds = self.DnsTlds
	}
	return settings, nil
}

// Alt serves the complete api surface as well as connect, so it warms the
// api set, which already contains connect's.
func altWarmupTargets() []server.WarmupTarget {
	return []server.WarmupTarget{
		server.WarmupTargetIPDatabase,
		server.WarmupTargetNetworkNameSearch,
		server.WarmupTargetLocationSearch,
		server.WarmupTargetCountryLocations,
		server.WarmupTargetLocationDirectory,
	}
}

// Binds one public udp service port on the addresses warp allocated for this
// host. Alt has no load balancer in front, so it answers directly on every
// family whose alt record points at this host (L3).
func listenPacketFromEnv(ctx context.Context, port int) ([]net.PacketConn, error) {
	listenIpv4, listenIpv6, hostPort := server.RequireListenIpPort(port)
	listenConfig := net.ListenConfig{}
	packetConns := []net.PacketConn{}
	for _, listenIp := range []string{listenIpv4, listenIpv6} {
		if listenIp == "" {
			continue
		}
		packetConn, err := listenConfig.ListenPacket(
			ctx,
			"udp",
			net.JoinHostPort(listenIp, strconv.Itoa(hostPort)),
		)
		if err != nil {
			for _, openPacketConn := range packetConns {
				openPacketConn.Close()
			}
			return nil, err
		}
		packetConns = append(packetConns, packetConn)
	}
	if len(packetConns) == 0 {
		return nil, fmt.Errorf("alt has no listen address for service port %d", port)
	}
	return packetConns, nil
}

// Run serves the alt module until ctx is canceled.
func Run(ctx context.Context, options RunOptions) error {
	return runWithDependencies(
		ctx,
		options,
		router.StartupReadiness,
		server.StartStatsPusher,
		listenPacketFromEnv,
		server.HttpListenAndServeWithReusePort,
	)
}

// The command and tests exercise the same startup and drain wiring. Only the
// external readiness, metrics transport, socket binding, and http listener
// are replaceable.
//
// Alt has no lb in front, so its only client ingress is the two udp sockets.
// The http listener carries the warp deploy poll's readiness route alone; a
// process that is not ready reports the failure and exits rather than serving
// a route no client can reach.
func runWithDependencies(
	ctx context.Context,
	options RunOptions,
	readiness func(context.Context) error,
	startStatsPusher func(context.Context) func(),
	listenPacket func(context.Context, int) ([]net.PacketConn, error),
	listenAndServe func(context.Context, string, http.Handler, bool, server.HttpServerOptions) error,
) error {
	if ctx == nil {
		return errors.New("alt run context is nil")
	}
	if err := options.Validate(); err != nil {
		return err
	}
	settings, err := options.Settings()
	if err != nil {
		return err
	}

	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := readiness(runCtx); err != nil {
		return fmt.Errorf("alt not ready: %w", err)
	}
	connectcore.ResizeMessagePools(connectcore.Gib(16))

	exchange := connectserver.NewExchangeFromEnv(runCtx, settings.ExchangeSettings)
	defer exchange.Close()
	connectRouter, err := connectserver.NewConnectRouterFromExchangeE(runCtx, cancel, exchange)
	if err != nil {
		return fmt.Errorf("initialize alt connect front: %w", err)
	}
	apiRouter, closeApiRouter, err := api.NewRouter(runCtx, runCtx)
	if err != nil {
		return fmt.Errorf("initialize alt api front: %w", err)
	}
	defer closeApiRouter()

	altServer, err := NewAlt(runCtx, connectRouter.ConnectHandler(), apiRouter, settings)
	if err != nil {
		return fmt.Errorf("initialize alt dispatch: %w", err)
	}

	packetConns := []net.PacketConn{}
	defer func() {
		for _, packetConn := range packetConns {
			packetConn.Close()
		}
	}()
	// every socket is registered before any of them serves, so the status
	// route reports the complete set from the first poll
	serves := []func() error{}
	for transport, port := range map[string]int{
		ListenerTransportH3:     options.H3Port,
		ListenerTransportWhodis: options.DnsPort,
	} {
		transportPacketConns, err := listenPacket(runCtx, port)
		if err != nil {
			return fmt.Errorf("bind alt %s %d: %w", transport, port, err)
		}
		packetConns = append(packetConns, transportPacketConns...)
		for _, packetConn := range transportPacketConns {
			serves = append(serves, altServer.AddListener(transport, packetConn))
		}
	}

	server.Warmup(altWarmupTargets()...)
	flushStats := startStatsPusher(runCtx)

	// a listener is alt's only client ingress, so losing one makes the
	// process useless: it reports the failure and exits for warp to replace
	listenErrs := make(chan error, len(serves)+1)
	for _, serve := range serves {
		go server.HandleError(func() {
			listenErrs <- serve()
		})
	}
	statusIpv4, _, statusPort := server.RequireListenIpPort(options.Port)
	go server.HandleError(func() {
		listenErrs <- listenAndServe(
			runCtx,
			net.JoinHostPort(statusIpv4, strconv.Itoa(statusPort)),
			router.NewRouter(runCtx, []*router.Route{
				router.NewRoute("GET", "/status", altServer.Status),
			}),
			false,
			server.HttpServerOptions{
				ReadTimeout:     15 * time.Second,
				WriteTimeout:    30 * time.Second,
				IdleTimeout:     5 * time.Minute,
				ShutdownTimeout: 30 * time.Second,
			},
		)
	})

	draining := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			router.SetWarpStatusDrainingIfReady()
			exchange.Drain()
			cancel()
		case <-draining:
		}
	}()
	defer close(draining)

	go server.HandleError(func() {
		for {
			select {
			case <-runCtx.Done():
				return
			case <-time.After(30 * time.Second):
			}
			if glog.V(1) {
				glog.Infof("[alt]goroutines=%d/%d\n", runtime.NumGoroutine(), runtime.GOMAXPROCS(0))
			}
		}
	})

	glog.Infof(
		"[alt]serving %s %s on *:%d (h3), *:%d (whodis) and *:%d (status)\n",
		server.RequireEnv(),
		server.RequireVersion(),
		options.H3Port,
		options.DnsPort,
		options.Port,
	)
	var runErr error
	select {
	case <-runCtx.Done():
	case listenErr := <-listenErrs:
		if runCtx.Err() == nil {
			runErr = listenErr
		}
		cancel()
	}
	altServer.Close()
	apiRouter.FlushStats()
	flushStats()
	glog.Infof("[alt]close\n")
	return runErr
}
