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
	"github.com/urnetwork/server/oauth"
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
	if _, err := core.SetProcessEnv("api", sidecarhost.NewServiceHost(ctx, &sidecarhost.ServiceHostOptions{Postgres: true})); err != nil {
		panic(err)
	}
	// §21's effects for the actions this process applies (ABUSE-M5-06), and
	// the sealed off-site record of each, from abuse-mailbox.yml (QD-726); a
	// binary that does not link MG5 installs neither
	controller.InstallAbuseEffects()
	controller.InstallAbuseRecordMailer()
	controller.InstallLegalHoldRecordMailer()
	controller.InstallLegalHoldDispatch()
	// §21.8's notice pager: the monitor's webhook from monitor.yml under a
	// retrying sink (QD-1070); none built leaves every notice's page owed
	controller.InstallAbuseNoticePager()
	// §21.7's standing and network_verified for this process's gates and
	// routes, and the hard deny its control route's CreateContract reads
	// (ABUSE-M5-07)
	controller.InstallAbuseStanding(ctx)
	// RFC 8628's device grant for ur-cli: device authorizations wait in
	// Redis (QD-830), so POST /oauth/device_authorization serves and the
	// issuer's metadata names it
	oauth.InstallDeviceAuthorizationStore()
	// §13.15's /tls routes (QD-846): certs' engine, store, authenticator and
	// the subject read from the projection, installed only while sidecar.yml's
	// certs switch is on; off, or with certs.yml or file-clusters-api.yml
	// missing, every /tls route answers 503
	controller.InstallTlsCerts(ctx)
	if err := api.Run(ctx, api.RunOptions{Port: port}); err != nil {
		panic(err)
	}
}
