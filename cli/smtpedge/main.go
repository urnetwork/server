// server/cli/smtpedge (SMTP-M9-20; D-SMTP-11, R-SMTP-13, R-SMTP-14, D-SMTP-5, D-OPS-10, I-13, X7, §14.9, §19.4): the
// smtpedge engine on the two sending hosts, run by smtpedgehost.Run through sidecarhost.Run with GET /status on port 80
// and no other port (smtpedgehost's header says what the block builds and when it is ready).
//
// This main reads config/<env>/smtpedge.yml, with the block's WARP_FWMARK and the X-UR-Ref keys from the vault secret
// file smtpedge-x-ur-ref.yml, into smtpedgehost.HostSettings, and what the block's secret_files hold into
// smtpedgehost.HostDeps: Quota over api's internal gate at -api_url on internal-gate-smtpedge.yml
// (quotahost.NewBlockQuota), the send authorization source on the same bearer, the store and the AbuseGate on the
// block's Postgres pool, the proof verifier over smtpedge.yml's proof_key_sets, and the platform client on the edge
// bearer (internal-gate-edge.yml), registering ur/smtpedge at the sockets service at -sockets_url. Each URL flag's
// default is the variable smtpedge.yml's env_vars give a deployed block, $UR_API_URL and $UR_SOCKETS_URL
// (sidecarhost.ServiceUrlFlag). It sets the process's Env, operator's global one (QD-1027), before the block serves.
package main

import (
	"context"
	"errors"
	"flag"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/glog"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/smtpedgeservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/core"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/quotahost"
	"github.com/urnetwork/server/sidecarhost"
	"github.com/urnetwork/server/smtpedgehost"
)

// The service as urnetwork_<svc>_* and the block's log name it.
const hostService = "smtpedge"

// smtpedge.yml from config, with the X-UR-Ref keys from the vault, and the directory the file sits in.
func loadSmtpEdgeConfig() (*smtpedgeservice.SmtpEdgeConfig, string, error) {
	resource, err := server.Config.SimpleResource(smtpedgehost.HostSettingsResourceName)
	if err != nil {
		return nil, "", err
	}
	data, err := resource.BytesE()
	if err != nil {
		return nil, "", err
	}
	config, err := smtpedgeservice.LoadSmtpEdgeConfig(data, server.RequireFwMark())
	if err != nil {
		return nil, "", err
	}
	// the X-UR-Ref keys, from the vault and never from the repository; a refusal names the part, never a key
	if err := model.LoadSmtpXUrRefKeyFile(config.Settings); err != nil {
		return nil, "", err
	}
	configHome := ""
	if path, err := server.Config.ResourcePath(smtpedgehost.HostSettingsResourceName); err == nil {
		configHome = filepath.Dir(path)
	}
	return config, configHome, nil
}

// The block's own deps: Quota over api's internal gate, the store and the AbuseGate on the block's Postgres
// pool, the gate, the dialer, and the client with its registrations at socketsUrl. Each is left out, and said,
// when what it needs is missing. The returned call ends what it started.
func blockDeps(ctx context.Context, settings *smtpedgehost.HostSettings, apiUrl string, socketsUrl string) (*smtpedgehost.HostDeps, func()) {
	doh := connect.NewDohCache(connect.DefaultDohSettings())
	resolver := sidecar.NewSmtpMxDnsResolver(doh, nil, nil, nil)
	deps := &smtpedgehost.HostDeps{
		Store:    model.NewSmtpEdgeStore(nil),
		Resolver: resolver,
		Ptr:      resolver,
		Dialer:   smtpedgeservice.NewSmtpMxNetDialer(settings.Config.Settings),
		Abuse:    model.NewAbuseGate(ctx, model.NewAbuseHardDenySnapshot(ctx, nil), nil),
	}
	if verifier, err := smtpedgehost.NewProofVerifier(settings.Config, settings.ConfigHome, time.Now()); err != nil {
		glog.Errorf("[%s]no proof verifier: %v\n", hostService, err)
	} else {
		deps.Verifier = verifier
		if live := len(settings.Config.ProofKeySetsAt(time.Now())); live == 0 {
			glog.Errorf("[%s]%s lists no proof key set to accept: every preamble answers KEYS\n", hostService, smtpedgehost.HostSettingsResourceName)
		} else {
			glog.Infof("[%s]the proof verifier holds %d key sets\n", hostService, live)
		}
	}
	ends := []func(){doh.Close}
	end := func() {
		for _, end := range ends {
			end()
		}
	}
	if quota, err := quotahost.NewBlockQuota(ctx, server.InternalGateCallerSmtpedge, apiUrl, nil); err != nil {
		glog.Errorf("[%s]no ServiceHost.Quota: %v\n", hostService, err)
	} else {
		deps.Quota = quota
		ends = append(ends, func() { quota.Drain(context.Background()) })
	}
	deps.Facts = smtpedgehost.NewModelGateSource(settings.Config.Settings.Root, deps.Quota)
	if credential, err := server.LoadInternalGateCredential(server.InternalGateCallerSmtpedge); err != nil {
		glog.Errorf("[%s]no send authorization source: %v\n", hostService, err)
	} else if source, err := sidecar.NewAclGateApiSource(apiUrl, nil, func() (string, error) { return credential, nil }, nil); err != nil {
		glog.Errorf("[%s]no send authorization source: %v\n", hostService, err)
	} else {
		deps.Projection = source
	}
	bearer, err := server.LoadInternalGateCredential(server.InternalGateCallerEdge)
	if err != nil {
		glog.Errorf("[%s]no platform client: %v\n", hostService, err)
		return deps, end
	}
	blockHost, err := server.Host()
	if err != nil {
		glog.Errorf("[%s]no platform client: %v\n", hostService, err)
		return deps, end
	}
	sourceSettings := server.DefaultPlatformClientSourceSettings()
	sourceSettings.ClientCount = settings.Clients.ClientCount
	// the deploy slot warp hands the process (QD-873, QD-1011), one no other running smtpedge process on the host
	// holds, so a deploy's new process holds other clients than the old one draining beside it
	slot, err := server.LoadEdgeClientSlot(sourceSettings)
	if err != nil {
		glog.Errorf("[%s]no platform client: %v\n", hostService, err)
		return deps, end
	}
	source, err := server.NewPlatformClientSource(ctx, &server.PlatformClientSourceArgs{
		ApiUrl:   apiUrl,
		Block:    hostService,
		Host:     blockHost,
		Slot:     slot,
		Bearer:   bearer,
		Settings: sourceSettings,
	})
	if err != nil {
		glog.Errorf("[%s]no platform client: %v\n", hostService, err)
		return deps, end
	}
	ends = append(ends, source.Close)
	if socketsUrl == "" {
		glog.Errorf("[%s]no platform client: no sockets url for its registration\n", hostService)
		return deps, end
	}
	deps.Source = source
	deps.NewClients = func(ctx context.Context) (smtpedgehost.HostClients, error) {
		return smtpedgehost.NewEdgeClients(ctx, sidecar.EdgeClientPoolArgs{
			Source:   source,
			Settings: settings.Clients,
			Log:      smtpedgehost.HostLogger(),
		})
	}
	// a registration is the client's own call, on its own sockets token, the service token of audience sockets
	// api's edge client route answers beside its jwt (QD-1054); a client the source holds no answer for has none
	deps.Registrations = source.SocketPlatformRegistrationsArgs(socketsUrl)
	return deps, end
}

// Sets the process's Env, operator's global one (QD-1027), before the block serves: over the engine's own
// ServiceHost, or an empty one for a block that could not start, which holds no service, so every gate reads
// closed and the rest refuses but the db. smtpedge holds a Postgres pool, so its Env grants the db either way.
func setProcessEnv(host *sidecar.ServiceHost) {
	if _, err := core.SetProcessEnv(hostService, host); err != nil {
		panic(err)
	}
}

var (
	port       = flag.Int("port", 80, "the block's status port")
	apiUrl     = sidecarhost.NewServiceUrlFlag(flag.CommandLine, "api_url", sidecarhost.ApiUrlEnvName, "api's base URL, whose internal gate the quota surface and the edge client route are read from; empty leaves the block not ready")
	socketsUrl = sidecarhost.NewServiceUrlFlag(flag.CommandLine, "sockets_url", sidecarhost.SocketsUrlEnvName, "the sockets service's base URL, which ur/smtpedge registers with; empty leaves the block not ready")
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

	options := &smtpedgehost.RunOptions{Port: *port, ProcessEnv: setProcessEnv}
	config, configHome, err := loadSmtpEdgeConfig()
	if err != nil {
		// a refused file names the key, never a value, and the key file never a key
		glog.Errorf("[%s]%s: %v\n", hostService, smtpedgehost.HostSettingsResourceName, err)
	} else {
		options.Settings = smtpedgehost.DefaultHostSettings(config)
		options.Settings.ConfigHome = configHome
		// on a context of its own, so a SIGTERM's drain can still send the open usage window
		depsCtx, depsCancel := context.WithCancel(context.WithoutCancel(ctx))
		defer depsCancel()
		deps, end := blockDeps(depsCtx, options.Settings, apiUrl.Url(), socketsUrl.Url())
		defer end()
		defer depsCancel()
		options.Deps = deps
	}
	if err := smtpedgehost.Run(ctx, options); err != nil {
		panic(err)
	}
}
