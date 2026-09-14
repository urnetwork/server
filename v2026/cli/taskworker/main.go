package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/docopt/docopt-go"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/taskworker"
)

func main() {
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

	opts, err := docopt.ParseArgs(usage, os.Args[1:], server.RequireVersion())
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
	if err := taskworker.Run(ctx, taskworker.RunOptions{Port: port, Count: count, BatchSize: batchSize}); err != nil {
		panic(err)
	}
}
