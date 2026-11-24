// server/cli/mxedge (MAIL-M5-04, MAIL-M7-19; §15.4, §15.15, §19.4): the mxedge engine's one binary in two roles
// on TCP 25, each its own warp service on its own lb interfaces (W4, plan-OPS-small-readings #6): --role platform
// for mxedge-platform, rpt.bringyour.com's listener, whose image server/cli/mxedge-platform builds from this
// main, and --role router for mxedge, whose image this directory builds. Each is a block run by
// mxedgehost.RunHost through sidecarhost.Run, with GET /status on port 80, TCP 25 behind the lb's PROXY header (and
// for the router mxedge.yml's lan_port, smtpedge's LAN way to an MX on the router's own interfaces, D-SMTP-13),
// {vault: no, config: yes, site: no} and user 65532. It reads config/<env>/mxedge.yml at start, the platform
// role's report intake reads smtpedge.yml beside it (report_intake.go, SMTP-M5-06), and the host rereads
// sidecar.yml's mxedge gate every 60 s.
//
// The router holds no TLS key and mounts nothing of the platform role's: its secret_files hold redis.yml, the
// Postgres role its session records are written under (pg-mxedge.yml), its internal gate bearer for the quota
// surface (internal-gate-mxedge.yml), the edge bearer its platform clients are minted on (internal-gate-edge.yml)
// and the feed credential (MAIL-M7-20). It routes by SNI through {mxr_<net>} and splices to receivers found at
// the sockets service (-sockets_url), its quota and clients at api (-api_url), each Find on its first client's
// sockets token, which api's edge client route answers beside the client's jwt (QD-1054). Its direct-MX prober reads
// the rows it probes from Postgres and posts each outcome to api on the same edge bearer, through the same source,
// so the router writes neither mail_direct_mx nor the zone (QD-1103, owner decision 639).
//
// The platform role files role mail's notices at api (-api_url) on a gate bearer of its own,
// internal-gate-mxedge-platform.yml (report_intake.go; QD-1070, owner decision 612; QD-1121), which its block
// mounts. Without the url or the file it files none, says so at start, and answers role mail as before; it stays
// ready either way, since the intake's reports need neither. On the same bearer its intake reads each network's mail
// key from api's report-key route and seals each report to it (plan-SMTP-report-seal-key); without the url or the
// file it seals none, says so at start, and keeps every report's fields. Its intake ties a report at a network's token
// to that network by network_dns_zone's index over its own Postgres (report_token_source.go), where its binary links
// MG6; without it every such report is dropped unread, and it says so at start. Its intake attributes ARF reports and
// role mail by SMTP-M9-15's join and halts by SMTP-M9-16's ladder (report_join.go), over the send log, the X-UR-Ref
// keys its block mounts from M9 (smtpedge-x-ur-ref.yml, warp's mxedge-platform-x-ur-ref.yml) and a Quota over its own
// Postgres, where its binary links MG9; without them it attributes nothing, says so at start, and serves the rest as
// before.
//
// Each URL flag's default is the variable the role's fragment, mxedge.yml or mxedge-platform.yml, gives a deployed
// block in its env_vars, $UR_API_URL and for the router $UR_SOCKETS_URL (sidecarhost.ServiceUrlFlag).
//
// Which Postgres file each role reads: server's main pool reads the file of the process's own service where its
// vault holds one, and the platform's pg.yml only where it holds none (QD-992; server/db.go's
// pgServiceVaultResourceName). The router's service is mxedge, so it reads pg-mxedge.yml and never pg.yml, which
// it does not hold; until the owner creates that role and its file, the router's store cannot connect and the
// block stays not ready. The platform role's service is mxedge-platform, whose secret_files hold pg.yml and no
// file of its own, so it reads pg.yml as before. The local stack's vault holds only pg.yml, which both read
// there.
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
	"github.com/urnetwork/sidecar/mxedgeservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/core"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/mxedgehost"
	"github.com/urnetwork/server/quotahost"
	"github.com/urnetwork/server/sidecarhost"
)

var (
	role       = flag.String("role", "", "platform or router")
	port       = flag.Int("port", 80, "the block's status port")
	smtpPort   = flag.Int("smtp_port", 25, "the block's TCP 25 port, behind the lb's PROXY header")
	secretDir  = flag.String("secret_dir", sidecarhost.ScopedVaultHome, "where the block's secret_files mount")
	apiUrl     = sidecarhost.NewServiceUrlFlag(flag.CommandLine, "api_url", sidecarhost.ApiUrlEnvName, "api's base URL: the router reads its quota surface and edge client route from api's internal gate, and empty leaves it not ready; the platform listener files role mail's notices there, and empty files none")
	socketsUrl = sidecarhost.NewServiceUrlFlag(flag.CommandLine, "sockets_url", sidecarhost.SocketsUrlEnvName, "router: the sockets service's base URL, which every Find is sent to; empty leaves the router not ready")
	feedDir    = flag.String("feed_dir", "", "router: the IP listing feed's mirror directory; empty rejects no sender at the banner")
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
	if *role != mxedgehost.HostRolePlatform && *role != mxedgehost.HostRoleRouter {
		glog.Errorf("[mxedge]--role must be %s or %s\n", mxedgehost.HostRolePlatform, mxedgehost.HostRoleRouter)
		logs.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGQUIT, syscall.SIGTERM)
	defer stop()

	mxEdgeSettings, err := loadMxEdgeSettings()
	if err != nil {
		// an mxedge.yml that cannot be served from never serves (mxedgeservice.LoadMxEdgeSettings)
		glog.Errorf("[mxedge]mxedge.yml: %v\n", err)
		logs.Exit(1)
	}
	settings := mxedgehost.DefaultHostSettings()
	settings.MxEdge = mxEdgeSettings
	settings.SecretDirectory = *secretDir
	deps := &mxedgehost.HostDeps{}
	var routerDeps *mxedgehost.RouterDeps
	switch *role {
	case mxedgehost.HostRolePlatform:
		// the block's own certificate and stores, and the report intake on smtpedge.yml (report_intake.go),
		// ready only in a binary that links the groups it reads; without one every recipient gets 451. The url
		// the refusal of unread role mail names is abuse.yml's form (abuse_auto_reply.go); two files that
		// disagree on it leave the block without an intake
		intake, err := newReportIntakeWithAutoReply(mxEdgeSettings, server.GetAbuseConfig().FormUrl())
		if err != nil {
			glog.Errorf("[mxedge]no report intake, so every recipient the listener answers for gets 451 4.3.0: %v\n", err)
		} else {
			// role mail's notices at api, on the platform listener's own bearer (QD-1070)
			if gate, err := newPlatformNoticeGate(apiUrl.Url()); err != nil {
				// the error names what is missing, never the bearer
				glog.Errorf("[mxedge]no role mail pages the rota, since role mail's notices cannot reach api: %v\n", err)
			} else {
				intake.fileNoticesThrough(gate, nil)
			}
			// each network's mail key at api, on the same bearer (plan-SMTP-report-seal-key)
			if keys, err := newPlatformReportKeySource(apiUrl.Url()); err != nil {
				// the error names what is missing, never the bearer
				glog.Errorf("[mxedge]no report is sealed, so each one's raw bytes are dropped unread and its fields kept: %v\n", err)
			} else if err := intake.sealThrough(keys); err != nil {
				glog.Errorf("[mxedge]no report is sealed, so each one's raw bytes are dropped unread and its fields kept: %v\n", err)
			}
			deps.IntakeGroups = reportIntakeGroups()
			// each report at a network's token tied to that network by network_dns_zone's index over the block's own
			// Postgres (report_token_source.go), where the binary links MG6, the column's group
			if err := resolveBlockReportTokens(intake); err != nil {
				glog.Errorf("[mxedge]no report at a network's token is tied to its network, so each is dropped unread: %v\n", err)
			} else {
				deps.IntakeGroups = append(deps.IntakeGroups, reportTokenGroups()...)
			}
			// SMTP-M9-15's join and SMTP-M9-16's ladder (report_join.go), their Quota on a context of its own,
			// drained after the block
			joinCtx, joinCancel := context.WithCancel(context.WithoutCancel(ctx))
			defer joinCancel()
			if end, err := attributeBlockReports(joinCtx, intake); err != nil {
				// the error names what is missing, never a key
				glog.Errorf("[mxedge]no ARF report or role mail is attributed, so none halts a network or reaches §21: %v\n", err)
			} else {
				defer end()
				deps.IntakeGroups = append(deps.IntakeGroups, reportJoinGroups()...)
			}
			deps.Intake = intake
		}
	case mxedgehost.HostRoleRouter:
		// on a context of its own, so a SIGTERM's drain can still send the open usage window, ended after the block
		quotaCtx, quotaCancel := context.WithCancel(context.WithoutCancel(ctx))
		defer quotaCancel()
		var end func()
		routerDeps, end = routerBlockDeps(quotaCtx, mxEdgeSettings, apiUrl.Url(), socketsUrl.Url())
		defer end()
		if *feedDir == "" {
			glog.Infof("[mxedge]no listing feed mirror, so no sender is rejected at the banner\n")
		} else {
			mirrorSettings := mxedgehost.DefaultFeedMirrorSettings()
			mirrorSettings.Directory = *feedDir
			if source, err := mxedgehost.NewFeedMirrorSource(mirrorSettings); err != nil {
				glog.Errorf("[mxedge]no listing feed mirror: %v\n", err)
			} else {
				routerDeps.Listings = source
			}
		}
	}
	setProcessEnv(ctx, *role, routerDeps)
	ports := &mxedgehost.HostPorts{Status: *port, Smtp: *smtpPort}
	if *role == mxedgehost.HostRoleRouter {
		// D-SMTP-13's LAN listener, mxedge.yml's lan_port, which smtpedge reaches for an MX on the router's own
		// interfaces (MAIL-M9-05)
		ports.Lan = int(mxEdgeSettings.LanPort)
	}
	if err := mxedgehost.RunHost(ctx, *role, settings, deps, routerDeps, ports); err != nil {
		panic(err)
	}
}

// Sets the process's Env, operator's global one (QD-1027), before the block serves, over the smallest ServiceHost
// role's own services fill, as mxedgehost builds each role's: the platform listener's, the block mxedge-platform,
// with Postgres, Redis, metrics, the service token authenticator, sidecar.yml's gates and task scheduling; the
// router's, the block mxedge, with those and its Quota and platform client source where routerBlockDeps made
// them. Each role holds a Postgres pool, the router's under its own restricted role, so each Env grants the db.
func setProcessEnv(ctx context.Context, role string, routerDeps *mxedgehost.RouterDeps) {
	process := "mxedge-platform"
	options := &sidecarhost.ServiceHostOptions{Postgres: true}
	if role == mxedgehost.HostRoleRouter {
		process = "mxedge"
		if routerDeps != nil {
			options.Quota = routerDeps.Quota
			options.ServiceClient = routerDeps.Source
		}
	}
	if _, err := core.SetProcessEnv(process, sidecarhost.NewServiceHost(ctx, options)); err != nil {
		panic(err)
	}
}

// The pool as the router's clients.
type routerPoolClients struct {
	*sidecar.EdgeClientPool
}

// The router's own deps: Quota over api's internal gate at apiUrl on the mxedge bearer, the store on the
// block's Postgres role, the source on the edge bearer, the direct-MX prober's store recording through that
// source, and the pool and the dialer over it with Find at socketsUrl. Each is left out, and said, when what it
// needs is missing, which leaves the block not ready; with no source no prober runs, as it could record nothing.
// quotaCtx outlives the drain, so the open usage window is sent. The returned call ends the quota's and
// the source's loops.
func routerBlockDeps(quotaCtx context.Context, mxEdgeSettings *mxedgeservice.MxEdgeSettings, apiUrl string, socketsUrl string) (*mxedgehost.RouterDeps, func()) {
	deps := &mxedgehost.RouterDeps{
		Store:       model.NewMxEdgeStore(nil),
		StoreGroups: mxedgehost.RouterStoreGroups(),
	}
	ends := []func(){}
	end := func() {
		for _, end := range ends {
			end()
		}
	}
	if quota, err := quotahost.NewBlockQuota(quotaCtx, server.InternalGateCallerMxedge, apiUrl, nil); err != nil {
		// the error names what is missing, never the bearer
		glog.Errorf("[mxedge]no ServiceHost.Quota: %v\n", err)
	} else {
		deps.Quota = quota
		ends = append(ends, func() { quota.Drain(context.Background()) })
	}
	bearer, err := server.LoadInternalGateCredential(server.InternalGateCallerEdge)
	if err != nil {
		glog.Errorf("[mxedge]no platform clients: %v\n", err)
		return deps, end
	}
	blockHost, err := server.Host()
	if err != nil {
		glog.Errorf("[mxedge]no platform clients: %v\n", err)
		return deps, end
	}
	sourceSettings := server.DefaultPlatformClientSourceSettings()
	sourceSettings.ClientCount = mxEdgeSettings.Clients
	// the deploy slot warp hands the process (QD-873, QD-1011), one no other running router on the host holds, so
	// beta's and g1's routers and a deploy's new router beside either each hold clients of their own
	slot, err := server.LoadEdgeClientSlot(sourceSettings)
	if err != nil {
		glog.Errorf("[mxedge]no platform clients: %v\n", err)
		return deps, end
	}
	source, err := server.NewPlatformClientSource(quotaCtx, &server.PlatformClientSourceArgs{
		ApiUrl:   apiUrl,
		Block:    "mxedge",
		Host:     blockHost,
		Slot:     slot,
		Bearer:   bearer,
		Settings: sourceSettings,
	})
	if err != nil {
		glog.Errorf("[mxedge]no platform clients: %v\n", err)
		return deps, end
	}
	ends = append(ends, source.Close)
	// a Team direct MX's rows, read by the router's prober, each outcome posted to api's internal route on the edge
	// bearer through the source (MAIL-M7-23; QD-1103)
	deps.DirectMxStore = model.NewMxDirectMxStore(mxEdgeSettings.RouteRootDomainName, source)
	if socketsUrl == "" {
		glog.Errorf("[mxedge]no platform clients: no sockets url for Find\n")
		return deps, end
	}
	deps.Source = source
	poolSettings := mxedgehost.DefaultRouterSettings(mxEdgeSettings).Clients
	deps.NewClients = func(ctx context.Context) (mxedgehost.RouterClients, error) {
		pool, err := sidecar.NewEdgeClientPool(ctx, &sidecar.EdgeClientPoolArgs{
			Source:   source,
			Settings: poolSettings,
			Log:      mxedgehost.HostLogger(),
		})
		if err != nil {
			return nil, err
		}
		return routerPoolClients{EdgeClientPool: pool}, nil
	}
	deps.NewDial = func(clients mxedgehost.RouterClients) (mxedgehost.RouterSocketDial, error) {
		dialer, err := sidecar.NewSocketEdgeDialer(&sidecar.SocketEdgeDialerArgs{
			Clients: clients.Clients(),
			// a Find is the block's own call, on the first of its K clients' sockets tokens, the service tokens of
			// audience sockets api's edge client route answers beside their jwts (QD-1054)
			Find: source.SocketFindHttpArgs(socketsUrl),
			Settings: &sidecar.SocketEdgeDialSettings{
				FindTimeout:   mxEdgeSettings.FindTimeout,
				FindCount:     mxEdgeSettings.FindCount,
				AcceptTimeout: mxEdgeSettings.AcceptTicketTimeout,
			},
		})
		if err != nil {
			return nil, err
		}
		return dialer.Dial, nil
	}
	return deps, end
}

// mxedge.yml from config.
func loadMxEdgeSettings() (*mxedgeservice.MxEdgeSettings, error) {
	resource, err := server.Config.SimpleResource("mxedge.yml")
	if err != nil {
		return nil, err
	}
	data, err := resource.BytesE()
	if err != nil {
		return nil, err
	}
	return mxedgeservice.LoadMxEdgeSettings(data)
}
