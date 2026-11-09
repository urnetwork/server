// server/cli/evs (EVS1-M2-21; §11.20, §19.4): the evs engine at evs.bringyour.com, one warp block run by
// evshost.RunHost through sidecarhost.Run, with GET /status and evs's routes on port 80, {vault: no, config: yes,
// site: no}, user 65532 and no Postgres. Its secret_files hold the audience's public JWKS, redis.yml,
// file-clusters-evs.yml, the receipt keys and grafana-push.yml; segments arrive on the block's streamable_paths.
package main

import (
	"context"
	"errors"
	"flag"
	"os"
	"os/signal"
	"syscall"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/evshost"
)

var (
	port          = flag.Int("port", 80, "the block's status port, which also serves evs's routes")
	chronyAddress = flag.String("chrony_address", "", "chronyd's NTP address as host:port; empty is the services network's")
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
	// the block's own stores; what server has no implementation of yet keeps it not ready (evshost's header)
	if err := evshost.RunHost(ctx, settings, nil, *port); err != nil {
		panic(err)
	}
}
