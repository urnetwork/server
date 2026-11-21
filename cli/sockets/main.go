// server/cli/sockets (SOCK-M5-08; D-SOCK-10, D-SOCK-14, R-SOCK-6, I-12, I-13, §9.8, §19.4): the sockets engine at
// sockets.<root>, one warp block on every lb host, run by socketshost.Run through sidecarhost.Run with GET /status and
// §9.15's /socket/* routes on port 80, {vault: no, config: yes, site: no}, user 65532 and no Postgres (socketshost's
// header says what the block builds and when it is ready).
//
// This main reads what the block's secret_files hold into socketshost.HostDeps: Quota over api's internal gate at
// -api_url on internal-gate-sockets.yml (quotahost.NewBlockQuota), the projection and listen facts sources on the same
// bearer, and the first metadata cluster's reader for rev/ (file-clusters-sockets.yml). The ticket key file
// (socket-ticket-keys.yml) and the svc- key set's public jwks (oauth-jwks-svc.json) are read by the host itself,
// from the same vault. The flag's default is the $UR_API_URL sockets.yml's env_vars give a deployed block
// (sidecarhost.ServiceUrlFlag). It sets the process's Env, operator's global one (QD-1027), before the block serves.
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

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/core"
	"github.com/urnetwork/server/quotahost"
	"github.com/urnetwork/server/sidecarhost"
	"github.com/urnetwork/server/socketshost"
)

// The service as urnetwork_<svc>_* and the block's log name it.
const hostService = "sockets"

// The block's own deps: quota, projections and listen facts over api's internal gate on the block's bearer,
// and the first metadata cluster's reader for rev/ (vault's file-clusters-sockets.yml), each left out, and said,
// when it is missing. quotaCtx outlives the drain, so the open usage window is sent; the clusters close with it.
func blockDeps(quotaCtx context.Context, apiUrl string, settings *socketshost.HostSettings) (*socketshost.HostDeps, func()) {
	deps := &socketshost.HostDeps{}
	ends := []func(){}
	drain := func() {
		for _, end := range ends {
			end()
		}
	}
	if quota, err := quotahost.NewBlockQuota(quotaCtx, server.InternalGateCallerSockets, apiUrl, nil); err != nil {
		// the error names what is missing, never the bearer
		glog.Errorf("[%s]no ServiceHost.Quota: %v\n", hostService, err)
	} else {
		deps.Quota = quota
		ends = append(ends, func() { quota.Drain(context.Background()) })
	}
	if clusters, err := server.LoadFileClusters(quotaCtx, socketshost.HostFileClustersService, nil); err != nil {
		// the error names the file, never a credential
		glog.Errorf("[%s]no rev/ reader: file-clusters-%s.yml: %v\n", hostService, socketshost.HostFileClustersService, err)
	} else {
		deps.Blobs = clusters
		ends = append(ends, clusters.Close)
	}
	bearer, err := server.LoadInternalGateCredential(server.InternalGateCallerSockets)
	if err != nil {
		glog.Errorf("[%s]no projection source: %v\n", hostService, err)
		return deps, drain
	}
	if err := deps.SetGateApiSources(apiUrl, bearer, nil, settings.ListenFactsMaxByteCount); err != nil {
		glog.Errorf("[%s]no internal gate sources: %v\n", hostService, err)
	}
	return deps, drain
}

// Sets the process's Env, operator's global one (QD-1027), before the block serves: over the engine's own
// ServiceHost, or an empty one for a block that could not start, which holds no service, so every gate reads
// closed and the rest refuses. sockets holds no Postgres, so its Env refuses the db either way.
func setProcessEnv(host *sidecar.ServiceHost) {
	if _, err := core.SetProcessEnv(hostService, host); err != nil {
		panic(err)
	}
}

var (
	port   = flag.Int("port", 80, "the block's status port, which also serves the /socket/* routes")
	apiUrl = sidecarhost.NewServiceUrlFlag(flag.CommandLine, "api_url", sidecarhost.ApiUrlEnvName, "api's base URL, whose internal gate the projection and the over-limit list are read from; empty leaves the block not ready")
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

	// on a context of its own, so a SIGTERM's drain can still send the open usage window, drained after the block
	quotaCtx, quotaCancel := context.WithCancel(context.WithoutCancel(ctx))
	defer quotaCancel()
	settings := socketshost.DefaultHostSettings()
	deps, drainQuota := blockDeps(quotaCtx, apiUrl.Url(), settings)
	defer drainQuota()
	options := &socketshost.RunOptions{
		Port:       *port,
		Settings:   settings,
		Deps:       deps,
		ProcessEnv: setProcessEnv,
	}
	if err := socketshost.Run(ctx, options); err != nil {
		panic(err)
	}
}
