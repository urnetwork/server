package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/docopt/docopt-go"

	"github.com/urnetwork/server"
	connectserver "github.com/urnetwork/server/connect"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/core"
	"github.com/urnetwork/server/sidecarhost"
)

func main() {
	// Address scrubbing for everything this process writes to stdout and
	// stderr, installed before anything can log. A failure here is not fatal:
	// it degrades to the previous unscrubbed behavior rather than losing
	// logging entirely. Every way out of main flushes it first, so the last
	// lines written, a failure's report included, are not lost: the deferred
	// Flush for a return or a panic, logs.Exit for an exit, and
	// logs.DocoptHelp for docopt's own. See server.ScrubCommandLogs.
	logs := server.ScrubCommandLogs()
	defer logs.Flush()

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

	opts, err := (&docopt.Parser{HelpHandler: logs.DocoptHelp}).ParseArgs(usage, os.Args[1:], server.RequireVersion())
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
	// the process's Env, operator's global one (QD-1027), set before anything
	// serves: over the smallest ServiceHost connect's own services fill, the
	// db, Redis, metrics, the service token authenticator, sidecar.yml's
	// gates and task scheduling, every other service refusing
	host := sidecarhost.NewServiceHost(ctx, &sidecarhost.ServiceHostOptions{Postgres: true})
	if _, err := core.SetProcessEnv("connect", host); err != nil {
		panic(err)
	}
	// §21's hard deny for the residents' CreateContract (ABUSE-M5-07); a binary
	// that does not link MG5 installs none
	defer controller.InstallAbuseHardDeny(ctx)()
	defer controller.InstallConnectAuthorization(ctx, host)()
	if err := connectserver.Run(ctx, connectserver.RunOptions{Port: port, MemoryOwnerLedger: memoryOwnerLedger, PrivateHeapProfileTarget: privateHeapTarget}); err != nil {
		panic(err)
	}
}
