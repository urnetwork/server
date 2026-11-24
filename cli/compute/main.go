// server/cli/compute (CSRV-M12-03; D-CSRV-1, DESIGN-COMPUTE §14.2): the compute engine, the broker and orchestrator,
// at compute.bringyour.com, one warp block run by computehost.RunHost through sidecarhost.Run, with GET /status and
// the compute routes on port 80, {vault: no, config: yes, site: no} and user 65532. It holds a Postgres pool for
// §14.4's groups, so its readiness waits for MG-C1 and MG-C2 at the schema head. Its secret_files hold the audience's
// public JWKS, redis.yml, pg.yml, the first metadata cluster's read-only credentials (file-clusters-compute.yml), the
// {cmpa} instance's, the areas' own files and grafana-push-compute.yml (warp's sidecar-blocks/compute.yml); the
// tunnel's WebSocket rides the block's websocket mode.
package main

import (
	"context"
	"errors"
	"flag"
	"os"
	"os/signal"
	"syscall"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/computehost"
	"github.com/urnetwork/server/core"
	"github.com/urnetwork/server/sidecarhost"
)

var port = flag.Int("port", 80, "the block's status port, which also serves the compute routes")

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

	// the process's Env, operator's global one (QD-1027), set before the block serves: over the smallest ServiceHost
	// this main's own services fill, Redis, metrics, the service token authenticator, sidecar.yml's gates and task
	// scheduling, with the Postgres pool the compute process is granted, through which the engine's handlers reach
	// the ledger (owner decision 594). The clusters and the gate cache the engine's own host builds inside
	// computehost are not in it and refuse
	if _, err := core.SetProcessEnv("compute", sidecarhost.NewServiceHost(ctx, &sidecarhost.ServiceHostOptions{Postgres: true})); err != nil {
		panic(err)
	}
	// the block's own stores for the rest; what server has no implementation of yet keeps it not ready (computehost's
	// header)
	if err := computehost.RunHost(ctx, computehost.DefaultHostSettings(), nil, *port); err != nil {
		panic(err)
	}
}
