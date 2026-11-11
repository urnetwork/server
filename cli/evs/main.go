// server/cli/evs (EVS1-M2-21; §11.20, §19.4): the evs engine at evs.bringyour.com, one warp block run by
// evshost.RunHost through sidecarhost.Run, with GET /status and evs's routes on port 80, {vault: no, config: yes,
// site: no}, user 65532 and no Postgres. Its secret_files hold the audience's public JWKS, redis.yml,
// file-clusters-evs.yml, the receipt keys, its internal gate bearer (internal-gate-evs.yml) and grafana-push.yml;
// segments arrive on the block's streamable_paths. ServiceHost.Quota reaches api's internal gate at -api_url on
// that bearer (QUOTA-M2-11, QD-055); a block without either has no Quota and stays not ready.
package main

import (
	"context"
	"errors"
	"flag"
	"os"
	"os/signal"
	"syscall"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/evshost"
	"github.com/urnetwork/server/quotahost"
)

var (
	port          = flag.Int("port", 80, "the block's status port, which also serves evs's routes")
	chronyAddress = flag.String("chrony_address", "", "chronyd's NTP address as host:port; empty is the services network's")
	apiUrl        = flag.String("api_url", "", "api's base URL, whose internal gate ServiceHost.Quota reads and posts usage to; empty leaves the block with no Quota, not ready")
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

	settings := evshost.DefaultHostSettings()
	if *chronyAddress != "" {
		settings.Chrony.ChronyAddress = *chronyAddress
	}
	deps := &evshost.HostDeps{}
	// on a context of its own, so a SIGTERM's drain can still send the open usage window, drained after the block
	quotaCtx, quotaCancel := context.WithCancel(context.WithoutCancel(ctx))
	defer quotaCancel()
	if quota, err := quotahost.NewBlockQuota(quotaCtx, server.InternalGateCallerEvs, *apiUrl, nil); err != nil {
		// the error names what is missing, never the bearer
		glog.Errorf("[evs]no ServiceHost.Quota: %v\n", err)
	} else {
		deps.Quota = quota
		defer quota.Drain(context.Background())
	}
	// the block's own stores for the rest; what server has no implementation of yet keeps it not ready (evshost's
	// header)
	if err := evshost.RunHost(ctx, settings, deps, *port); err != nil {
		panic(err)
	}
}
