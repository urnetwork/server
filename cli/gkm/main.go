// server/cli/gkm (KEYS-M2-17; D-OPS-2, §10.14, §19.4): the gkm engine at gkm.bringyour.com, one warp block run by
// gkm.Run through sidecarhost.Run, with GET /status and gkm's routes on port 80, {vault: no, config: yes, site: no},
// user 65532 and no Postgres. Its secret_files hold the audience's public JWKS, redis.yml, file-clusters-gkm.yml and
// grafana-push.yml.
package main

import (
	"context"
	"errors"
	"flag"
	"os"
	"os/signal"
	"syscall"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/gkm"
)

var (
	port = flag.Int("port", 80, "the block's status port, which also serves gkm's routes")
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

	// the block's own stores; what server has no implementation of yet keeps it not ready (gkm's package header)
	if err := gkm.Run(ctx, gkm.RunOptions{Port: *port}); err != nil {
		panic(err)
	}
}
