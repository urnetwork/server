package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/docopt/docopt-go"

	"github.com/urnetwork/server"
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
  taskworker [--port=<port>] [--count=<count>] [--batch_size=<batch_size>] [--feed_dir=<feed_dir>]
  taskworker init-tasks
  taskworker -h | --help
  taskworker --version

Options:
  -h --help     Show this screen.
  --version     Show version.
  -p --port=<port>  Listen port [default: 80].
  -n --count=<count>  Number of worker processes [default: 8].
  -b --batch_size=<batch_size>  Batch size [default: 4].
  --feed_dir=<feed_dir>  §21.5's breakers: this host's copy of the IP listing feed's mirror directory; none leaves the listing unknown.`

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
	// what the tasks run on, before the worker runs them: §21's effects,
	// sealed off-site records, notice pager and standing (a binary that does
	// not link MG5 installs none of them), §21.5's breakers' outside measures
	// over this host's copy of the feed mirror, and certs' chains while
	// sidecar.yml's certs switch is on. all runs the worker beside api and
	// connect and installs the same parts, each once
	// (taskworker/work/taskworker_tasks_install.go)
	feedDir, _ := opts.String("--feed_dir")
	defer work.InstallTaskworkerTasks(ctx, &work.TaskworkerTasksInstallArgs{FeedDirectory: feedDir})()
	if err := taskworker.Run(ctx, taskworker.RunOptions{Port: port, Count: count, BatchSize: batchSize}); err != nil {
		panic(err)
	}
}
