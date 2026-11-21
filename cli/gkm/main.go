// server/cli/gkm (KEYS-M2-17; D-OPS-2, §10.14, §19.4): the gkm engine at gkm.bringyour.com, one warp block run by
// gkm.Run through sidecarhost.Run, with GET /status and gkm's routes on port 80, {vault: no, config: yes, site: no},
// user 65532 and no Postgres. Its secret_files hold the audience's public JWKS, redis.yml, file-clusters-gkm.yml, its
// internal gate bearer (internal-gate-gkm.yml) and grafana-push.yml. ServiceHost.Quota reaches api's internal gate at
// -api_url on that bearer for the write budget's weights (QUOTA-M2-11, QD-055), the flag's default the $UR_API_URL
// gkm.yml's env_vars give a deployed block (sidecarhost.ServiceUrlFlag); without either, every network weighs as
// Community's. The hint writer resolves sidecar ids through the same internal gate on that bearer
// (server.SidecarIdentityClientsResolver; plan-KEYS-hint-addressing, QD-1171); without either it resolves none, and
// every hint by sidecar id waits for its clients' hourly resync.
package main

import (
	"context"
	"errors"
	"flag"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/core"
	"github.com/urnetwork/server/gkm"
	"github.com/urnetwork/server/quotahost"
	"github.com/urnetwork/server/sidecarhost"
)

var (
	port   = flag.Int("port", 80, "the block's status port, which also serves gkm's routes")
	apiUrl = sidecarhost.NewServiceUrlFlag(flag.CommandLine, "api_url", sidecarhost.ApiUrlEnvName, "api's base URL, whose internal gate ServiceHost.Quota reads limits from; empty weighs every network's writes as Community's")
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

	deps := &gkm.HostDeps{}
	if resolver, err := newPushResolver(apiUrl.Url()); err != nil {
		// the error names what is missing, never the bearer
		glog.Errorf("[gkm]no sidecar_id resolver, so every hint by sidecar id waits for its clients' resync: %v\n", err)
	} else {
		deps.PushResolver = resolver
	}
	// on a context of its own, which the drain does not end, drained after the block
	quotaCtx, quotaCancel := context.WithCancel(context.WithoutCancel(ctx))
	defer quotaCancel()
	if quota, err := quotahost.NewBlockQuota(quotaCtx, server.InternalGateCallerGkm, apiUrl.Url(), nil); err != nil {
		// the error names what is missing, never the bearer
		glog.Errorf("[gkm]no ServiceHost.Quota, so every network's writes weigh as Community's: %v\n", err)
	} else {
		deps.Quota = quota
		defer quota.Drain(context.Background())
	}
	// the process's Env, operator's global one (QD-1027), set before the block serves: over the smallest ServiceHost
	// this main's own services fill, Redis, metrics, the service token authenticator, sidecar.yml's gates and the
	// Quota above, on the quota's context so the gates are still read through the drain. gkm holds no Postgres, so
	// the Env refuses the db, and the file clusters, write budget and push the engine's own host builds inside gkm
	// are not in it and refuse
	if _, err := core.SetProcessEnv("gkm", sidecarhost.NewServiceHost(quotaCtx, &sidecarhost.ServiceHostOptions{Quota: deps.Quota})); err != nil {
		panic(err)
	}
	// the block's own stores and authorization state for the rest (gkm's package header)
	if err := gkm.Run(ctx, gkm.RunOptions{Port: *port, Deps: deps}); err != nil {
		panic(err)
	}
}

// The hint writer's resolver: api's internal sidecar identity clients route at apiUrl on the block's gkm bearer.
func newPushResolver(apiUrl string) (*server.SidecarIdentityClientsResolver, error) {
	if apiUrl == "" {
		return nil, errors.New("no api url for the internal gate")
	}
	bearer, err := server.LoadInternalGateCredential(server.InternalGateCallerGkm)
	if err != nil {
		return nil, err
	}
	return server.NewSidecarIdentityClientsResolver(&server.SidecarIdentityClientsResolverArgs{ApiUrl: strings.TrimSuffix(apiUrl, "/"), Bearer: bearer})
}
