package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/docopt/docopt-go"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/core"
	"github.com/urnetwork/server/sidecarhost"
	"github.com/urnetwork/server/taskworker"
	"github.com/urnetwork/server/taskworker/work"
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

	usage := `BringYour task worker.

Usage:
  taskworker [--port=<port>] [--count=<count>] [--batch_size=<batch_size>]
  taskworker init-tasks
  taskworker -h | --help
  taskworker --version

Options:
  -h --help     Show this screen.
  --version     Show version.
  -p --port=<port>  Listen port [default: 80].
  -n --count=<count>  Number of worker processes [default: 8].
  -b --batch_size=<batch_size>  Batch size [default: 4].`

	opts, err := (&docopt.Parser{HelpHandler: logs.DocoptHelp}).ParseArgs(usage, os.Args[1:], server.RequireVersion())
	if err != nil {
		panic(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGQUIT, syscall.SIGTERM)
	defer stop()
	initTasks, err := opts.Bool("init-tasks")
	if err != nil {
		panic(err)
	}
	if initTasks {
		taskworker.InitTasks(ctx)
		return
	}
	port, err := opts.Int("--port")
	if err != nil {
		panic(err)
	}
	count, err := opts.Int("--count")
	if err != nil {
		panic(err)
	}
	batchSize, err := opts.Int("--batch_size")
	if err != nil {
		panic(err)
	}
	// the process's Env, operator's global one, which sidecar/work reads
	// (QD-1027), set before anything serves: over the smallest ServiceHost
	// taskworker's own services fill, so the db, Redis, metrics, the service
	// token authenticator, sidecar.yml's gates and the worker's own sidecar
	// task registry answer, a job bound through it being one this worker
	// claims, and every service taskworker builds none of here refuses
	if _, err := core.SetProcessEnv("taskworker", sidecarhost.NewServiceHost(ctx, &sidecarhost.ServiceHostOptions{Postgres: true, Tasks: taskworker.SidecarTasks()})); err != nil {
		panic(err)
	}
	// §21's effects and sealed off-site records for the owed actions the
	// abuse pass finishes (ABUSE-M5-06, QD-726); a binary that does not link
	// MG5 installs neither
	controller.InstallAbuseEffects()
	controller.InstallAbuseRecordMailer()
	controller.InstallLegalHoldRecordMailer()
	// §21.8's notice pager: the monitor's webhook from monitor.yml under a
	// retrying sink (QD-1070); none built leaves every notice's page owed
	controller.InstallAbuseNoticePager()
	// §21.7's standing and network_verified for the reports, record writes and
	// grant activations this process runs (ABUSE-M5-07)
	controller.InstallAbuseStanding(ctx)
	// certs' order, renewal and revocation chains (QD-846): the engine with
	// its DNS client, ACME client and hint writer, installed only while
	// sidecar.yml's certs switch is on and certs.yml, its account files and
	// internal-certs.yml are there; otherwise each chain runs empty passes
	work.InstallTlsCertChains(ctx)
	if err := taskworker.Run(ctx, taskworker.RunOptions{Port: port, Count: count, BatchSize: batchSize}); err != nil {
		panic(err)
	}
}
