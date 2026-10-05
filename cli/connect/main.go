package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/docopt/docopt-go"

	"github.com/urnetwork/server"
	connectserver "github.com/urnetwork/server/connect"
)

func main() {
	// Address scrubbing for everything this process writes to stdout and
	// stderr, installed before anything can log. A failure here is not fatal:
	// it degrades to the previous unscrubbed behavior rather than losing
	// logging entirely. See server.ScrubProcessLogs.
	server.ScrubProcessLogs()

	usage := `BringYour connect server.

Usage:
  connect [--port=<port>] [--memory-owner-ledger] [--private-heap-profile-target=<target>]
  connect -h | --help
  connect --version

Options:
  -h --help     Show this screen.
  --version     Show version.
  -p --port=<port>  Listen port [default: 80].
  --memory-owner-ledger  Enable fixed resident transfer-owner metrics.
  --private-heap-profile-target=<target>  Root-only host/block, disabled, or optional startup Config [default: config].`

	opts, err := docopt.ParseArgs(usage, os.Args[1:], server.RequireVersion())
	if err != nil {
		panic(err)
	}
	port, err := opts.Int("--port")
	if err != nil {
		panic(err)
	}
	memoryOwnerLedger, err := opts.Bool("--memory-owner-ledger")
	if err != nil {
		panic(err)
	}
	privateHeapTarget, err := opts.String("--private-heap-profile-target")
	if err != nil {
		panic(err)
	}
	if privateHeapTarget == "config" {
		privateHeapTarget = ""
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGQUIT, syscall.SIGTERM)
	defer stop()
	if err := connectserver.Run(ctx, connectserver.RunOptions{Port: port, MemoryOwnerLedger: memoryOwnerLedger, PrivateHeapProfileTarget: privateHeapTarget}); err != nil {
		panic(err)
	}
}
