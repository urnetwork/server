package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"

	"github.com/docopt/docopt-go"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/competition"
)

func main() {
	usage := `Secure sim-latency competition evaluator worker.

Usage:
  competitionworker [--worker_id=<id>]
  competitionworker -h | --help
  competitionworker --version

Options:
  -h --help          Show this screen.
  --version          Show version.
  --worker_id=<id>   Stable evaluator identity; defaults to hostname.`

	opts, err := docopt.ParseArgs(usage, os.Args[1:], server.RequireVersion())
	if err != nil {
		panic(err)
	}
	workerId, _ := opts.String("--worker_id")
	if workerId == "" {
		workerId, err = os.Hostname()
		if err != nil {
			panic(err)
		}
	}
	settings, err := competition.LoadSettings()
	if err != nil {
		panic(err)
	}
	worker, err := competition.NewWorker(
		settings,
		competition.PostgresStore{},
		competition.CommandEvaluator{},
		workerId,
	)
	if err != nil {
		panic(err)
	}
	quit := server.NewEventWithContext(context.Background())
	closeSignals := quit.SetOnSignals(syscall.SIGQUIT, syscall.SIGTERM, syscall.SIGINT)
	defer closeSignals()
	if err := worker.Run(quit.Ctx); err != nil && !errors.Is(err, context.Canceled) {
		panic(fmt.Errorf("competition worker: %w", err))
	}
}
