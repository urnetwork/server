// server/cli/gkm (KEYS-M2-17; D-OPS-2, §10.14, §19.4): the gkm engine at gkm.bringyour.com, one warp block run by
// gkm.Run through sidecarhost.Run, with GET /status and gkm's routes on port 80, {vault: no, config: yes, site: no},
// user 65532 and no Postgres. Its secret_files hold the audience's public JWKS, redis.yml, file-clusters-gkm.yml, its
// internal gate bearer (internal-gate-gkm.yml) and grafana-push.yml. ServiceHost.Quota reaches api's internal gate at
// -api_url on that bearer for the write budget's weights (QUOTA-M2-11, QD-055); without either, every network weighs
// as Community's.
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
	"github.com/urnetwork/server/gkm"
	"github.com/urnetwork/server/quotahost"
)

var (
	port   = flag.Int("port", 80, "the block's status port, which also serves gkm's routes")
	apiUrl = flag.String("api_url", "", "api's base URL, whose internal gate ServiceHost.Quota reads limits from; empty weighs every network's writes as Community's")
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
	// on a context of its own, which the drain does not end, drained after the block
	quotaCtx, quotaCancel := context.WithCancel(context.WithoutCancel(ctx))
	defer quotaCancel()
	if quota, err := quotahost.NewBlockQuota(quotaCtx, server.InternalGateCallerGkm, *apiUrl, nil); err != nil {
		// the error names what is missing, never the bearer
		glog.Errorf("[gkm]no ServiceHost.Quota, so every network's writes weigh as Community's: %v\n", err)
	} else {
		deps.Quota = quota
		defer quota.Drain(context.Background())
	}
	// the block's own stores and authorization state for the rest (gkm's package header)
	if err := gkm.Run(ctx, gkm.RunOptions{Port: *port, Deps: deps}); err != nil {
		panic(err)
	}
}
