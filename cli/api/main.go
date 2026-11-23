package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/docopt/docopt-go"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/api"
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

	usage := `BringYour API server.

Usage:
  api [--port=<port>]
  api -h | --help
  api --version

Options:
  -h --help     Show this screen.
  --version     Show version.
  -p --port=<port>  Listen port [default: 80].`

	opts, err := (&docopt.Parser{HelpHandler: logs.DocoptHelp}).ParseArgs(usage, os.Args[1:], server.RequireVersion())
	if err != nil {
		panic(err)
	}
	port, err := opts.Int("--port")
	if err != nil {
		panic(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGQUIT, syscall.SIGTERM)
	defer stop()
	// the process's Env, operator's global one, which sidecar/handler reads
	// (QD-1027), set before anything serves: over the smallest ServiceHost
	// api's own services fill, so the db, Redis, metrics, the service token
	// authenticator, sidecar.yml's gates and task scheduling answer, and
	// every service api builds none of here refuses
	host := sidecarhost.NewServiceHost(ctx, &sidecarhost.ServiceHostOptions{Postgres: true})
	if _, err := core.SetProcessEnv("api", host); err != nil {
		panic(err)
	}
	// what the route set runs on, before it serves: §21's effects, records,
	// hold dispatch, notice pager and standing, the issuer's rev/ store and
	// worker, the projection's sequencer, gate cache and what reads it (the
	// approval gate's states, the dns scopes, the smtp routes' checks, the
	// edge logins, the contract gate and the exchange's scopes), ur-cli's
	// device grant and the /tls routes. alt serves the same set and installs
	// it through the same call, so the two answer every route alike
	// (controller/api_routes_install.go)
	defer controller.InstallApiRoutes(ctx, host, nil)()
	if err := api.Run(ctx, api.RunOptions{Port: port}); err != nil {
		panic(err)
	}
}
