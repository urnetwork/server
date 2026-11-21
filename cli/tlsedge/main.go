// server/cli/tlsedge (TLS-M5-11; R-TLS-2, D-TLS-1, D-TLS-3, D-TLS-4, D-TLS-9, I-3, I-12, I-13, §13.5, §13.6, §19.4):
// the tlsedge engine, §13.5's layer-4 router by SNI, one warp block on every lb host, run by tlsedgehost.Run through
// sidecarhost.Run with GET /status on port 80 and TCP 443, 8443, 8883 and 5671 behind the lb's PROXY header,
// {vault: no, config: yes, site: no} and user 65532 (tlsedgehost's header says what the block builds and when it is
// ready).
//
// This main reads config/<env>/tlsedge.yml into tlsedgehost.HostSettings and what the block's secret_files hold
// into tlsedgehost.HostDeps: Quota over api's internal gate at -api_url on internal-gate-tlsedge.yml
// (quotahost.NewBlockQuota), the store on the block's Postgres role (pg-tlsedge.yml, which server's main pool reads
// for the process's own service), and the platform clients on the edge bearer (internal-gate-edge.yml), with every
// Find sent to the sockets service at -sockets_url. Each URL flag's default is the variable tlsedge.yml's env_vars
// give a deployed block, $UR_API_URL and $UR_SOCKETS_URL (sidecarhost.ServiceUrlFlag). It sets the process's Env,
// operator's global one (QD-1027), before the block serves.
package main

import (
	"context"
	"errors"
	"flag"
	"os"
	"os/signal"
	"syscall"

	"github.com/urnetwork/glog"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/tlsedgeservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/core"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/quotahost"
	"github.com/urnetwork/server/sidecarhost"
	"github.com/urnetwork/server/tlsedgehost"
)

// The service as urnetwork_<svc>_* and the block's log name it, and the block as config/<env>/edge.yml lists it.
const hostService = "tlsedge"

// The block's file under config/<env>.
const hostSettingsResourceName = "tlsedge.yml"

// tlsedge.yml from config. A file the engine refuses, main's without its blocks among them, starts no engine.
func loadTlsEdgeSettings() (*tlsedgeservice.TlsEdgeSettings, error) {
	resource, err := server.Config.SimpleResource(hostSettingsResourceName)
	if err != nil {
		return nil, err
	}
	data, err := resource.BytesE()
	if err != nil {
		return nil, err
	}
	return tlsedgeservice.LoadTlsEdgeSettings(data)
}

// The pool as the block's clients.
type poolClients struct {
	*sidecar.EdgeClientPool
}

// The block's own deps: Quota over api's internal gate at apiUrl on the tlsedge bearer, the store on the block's
// Postgres role, the source on the edge bearer, and the pool and the dialer over it with Find at socketsUrl. Each
// is left out, and said, when what it needs is missing. quotaCtx outlives the drain, so the open usage window is
// sent. The returned call drains the quota and ends the source.
func blockDeps(quotaCtx context.Context, settings *tlsedgehost.HostSettings, apiUrl string, socketsUrl string) (*tlsedgehost.HostDeps, func()) {
	deps := &tlsedgehost.HostDeps{Store: model.NewTlsEdgeStore(nil)}
	ends := []func(){}
	end := func() {
		for _, end := range ends {
			end()
		}
	}
	if quota, err := quotahost.NewBlockQuota(quotaCtx, server.InternalGateCallerTlsedge, apiUrl, nil); err != nil {
		// the error names what is missing, never the bearer
		glog.Errorf("[%s]no ServiceHost.Quota: %v\n", hostService, err)
	} else {
		deps.Quota = quota
		ends = append(ends, func() { quota.Drain(context.Background()) })
	}
	bearer, err := server.LoadInternalGateCredential(server.InternalGateCallerEdge)
	if err != nil {
		glog.Errorf("[%s]no platform clients: %v\n", hostService, err)
		return deps, end
	}
	blockHost, err := server.Host()
	if err != nil {
		glog.Errorf("[%s]no platform clients: %v\n", hostService, err)
		return deps, end
	}
	sourceSettings := server.DefaultPlatformClientSourceSettings()
	sourceSettings.ClientCount = settings.TlsEdge.Clients
	slot, err := server.LoadEdgeClientSlot(sourceSettings)
	if err != nil {
		glog.Errorf("[%s]no platform clients: %v\n", hostService, err)
		return deps, end
	}
	source, err := server.NewPlatformClientSource(quotaCtx, &server.PlatformClientSourceArgs{
		ApiUrl:   apiUrl,
		Block:    hostService,
		Host:     blockHost,
		Slot:     slot,
		Bearer:   bearer,
		Settings: sourceSettings,
	})
	if err != nil {
		glog.Errorf("[%s]no platform clients: %v\n", hostService, err)
		return deps, end
	}
	ends = append(ends, source.Close)
	if socketsUrl == "" {
		glog.Errorf("[%s]no platform clients: no sockets url for Find\n", hostService)
		return deps, end
	}
	deps.Source = source
	deps.NewClients = func(ctx context.Context) (tlsedgehost.HostClients, error) {
		pool, err := sidecar.NewEdgeClientPool(ctx, &sidecar.EdgeClientPoolArgs{
			Source:   source,
			Settings: settings.Clients,
			Log:      tlsedgehost.HostLogger(),
		})
		if err != nil {
			return nil, err
		}
		return poolClients{EdgeClientPool: pool}, nil
	}
	deps.NewDial = func(clients tlsedgehost.HostClients) (tlsedgehost.HostSocketDial, error) {
		dialer, err := sidecar.NewSocketEdgeDialer(&sidecar.SocketEdgeDialerArgs{
			Clients: clients.Clients(),
			// a Find is the block's own call, on the first of its K clients' sockets tokens (QD-1054)
			Find: source.SocketFindHttpArgs(socketsUrl),
			Settings: &sidecar.SocketEdgeDialSettings{
				FindTimeout:   settings.TlsEdge.FindTimeout,
				FindCount:     settings.TlsEdge.FindMaxCount,
				AcceptTimeout: settings.TlsEdge.AcceptTicketTimeout,
			},
		})
		if err != nil {
			return nil, err
		}
		return func(ctx context.Context, request *sidecar.SocketEdgeDialRequest) (tlsedgeservice.TlsEdgeStream, error) {
			stream, err := dialer.Dial(ctx, request)
			if err != nil {
				// never a typed nil as the engine's stream
				return nil, err
			}
			return stream, nil
		}, nil
	}
	return deps, end
}

// Sets the process's Env, operator's global one (QD-1027), before the block serves: over the engine's own
// ServiceHost, or an empty one for a block that could not start, which holds no service, so every gate reads
// closed and the rest refuses. tlsedge's pool is under its restricted role for the engine's store alone, so its
// Env refuses the db either way.
func setProcessEnv(host *sidecar.ServiceHost) {
	if _, err := core.SetProcessEnv(hostService, host); err != nil {
		panic(err)
	}
}

var (
	port       = flag.Int("port", 80, "the block's status port")
	apiUrl     = sidecarhost.NewServiceUrlFlag(flag.CommandLine, "api_url", sidecarhost.ApiUrlEnvName, "api's base URL, whose internal gate the quota surface and the edge client route are read from; empty leaves the block not ready")
	socketsUrl = sidecarhost.NewServiceUrlFlag(flag.CommandLine, "sockets_url", sidecarhost.SocketsUrlEnvName, "the sockets service's base URL, which every Find is sent to; empty leaves the block not ready")
)

func main() {
	// before anything can log (server.ScrubCommandLogs)
	logs := server.ScrubCommandLogs()
	defer logs.Flush()

	flag.CommandLine.Init(os.Args[0], flag.ContinueOnError)
	if err := flag.CommandLine.Parse(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			logs.Exit(0)
		}
		logs.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGQUIT, syscall.SIGTERM)
	defer stop()

	options := &tlsedgehost.RunOptions{Port: *port, ProcessEnv: setProcessEnv}
	tlsEdgeSettings, err := loadTlsEdgeSettings()
	if err != nil {
		// a tlsedge.yml the engine refuses names the key, never a value
		glog.Errorf("[%s]%s: %v\n", hostService, hostSettingsResourceName, err)
	} else {
		options.Settings = tlsedgehost.DefaultHostSettings(tlsEdgeSettings)
		// on a context of its own, so a SIGTERM's drain can still send the open usage window, ended after the block
		quotaCtx, quotaCancel := context.WithCancel(context.WithoutCancel(ctx))
		defer quotaCancel()
		deps, end := blockDeps(quotaCtx, options.Settings, apiUrl.Url(), socketsUrl.Url())
		defer end()
		options.Deps = deps
	}
	if err := tlsedgehost.Run(ctx, options); err != nil {
		panic(err)
	}
}
