package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/docopt/docopt-go"

	"github.com/urnetwork/server"
	gossipserver "github.com/urnetwork/server/gossip"
)

func main() {
	usage := `BringYour extender gossip server.

Usage:
  gossip [--port=<port>]
  gossip -h | --help
  gossip --version

Options:
  -h --help     Show this screen.
  --version     Show version.
  -p --port=<port>  Listen port [default: 80].`

	opts, err := docopt.ParseArgs(usage, os.Args[1:], server.RequireVersion())
	if err != nil {
		panic(err)
	}
	port, err := opts.Int("--port")
	if err != nil {
		panic(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGQUIT, syscall.SIGTERM)
	defer stop()
	if err := gossipserver.Run(ctx, gossipserver.RunOptions{Port: port}); err != nil {
		panic(err)
	}
}
