package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"syscall"
	"time"

	"github.com/docopt/docopt-go"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
)

func main() {
	usage := `Secure one-epoch sim-latency competition evaluator worker.

The process exits successfully after admission closes and the FIFO drains.
Legacy production significant candidates remain embargoed until the operator's
honesty-review harness finalizes the epoch; the external controller promotes
an approved winner (or records no winner) and starts the next epoch.
Explicit v2 rounds seal/elect provisional rewards independently of integrity
review; a fenced supervisor separately selects reviewed source and advances.
Staging automatically finalizes with its highest-ranked placeable candidate
passing every gate, or no winner. It never pauses for honesty review or
promotes source.

Usage:
  competitionworker [--worker_id=<id>]
  competitionworker --check [--worker_id=<id>]
  competitionworker --supervisor-request [--worker_id=<id>]
  competitionworker --source-readiness=<generation> [--worker_id=<id>]
  competitionworker -h | --help
  competitionworker --version

Options:
  -h --help          Show this screen.
  --version          Show version.
  --check            Verify the host and database without claiming work.
  --supervisor-request  Read strict fenced v2 round identity from stdin; never select another round.
  --source-readiness=<generation>  Probe and acknowledge this exact installed v2 source generation.
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
	settings, err := controller.LoadSettings()
	if err != nil {
		panic(err)
	}
	supervised, _ := opts.Bool("--supervisor-request")
	readinessGeneration, _ := opts.String("--source-readiness")
	if readinessGeneration != "" {
		generation, err := strconv.ParseInt(readinessGeneration, 10, 64)
		if err != nil || generation < 1 || server.RequireEnv() != "main" {
			panic("source readiness requires an exact positive generation and main environment")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if err := controller.AcknowledgeCompetitionSourceWorker(ctx, settings, generation, workerId); err != nil {
			panic(err)
		}
		return
	}
	var supervisorRequest *controller.CompetitionSupervisorWorkerRequest
	if supervised {
		if server.RequireEnv() != "main" {
			panic("supervised competition worker requires the main environment")
		}
		supervisorRequest, err = controller.DecodeCompetitionSupervisorWorkerRequest(os.Stdin)
		if err != nil {
			panic(err)
		}
	}
	check, _ := opts.Bool("--check")
	if check {
		worker, err := controller.NewWorker(settings, controller.PostgresStore{}, controller.CommandEvaluator{}, workerId)
		if err != nil {
			panic(err)
		}
		if err := worker.Preflight(context.Background()); err != nil {
			panic(fmt.Errorf("competition worker preflight: %w", err))
		}
		return
	}
	quit := server.NewEventWithContext(context.Background())
	closeSignals := quit.SetOnSignals(syscall.SIGQUIT, syscall.SIGTERM, syscall.SIGINT)
	defer closeSignals()
	controller.StartRunnerHeartbeat(quit.Ctx)
	controller.StartMetrics(quit.Ctx)
	flushStats := server.StartStatsPusher(quit.Ctx)
	defer flushStats()
	if supervised {
		err = controller.RunCompetitionSupervisorWorker(quit.Ctx, settings, *supervisorRequest, workerId)
	} else {
		var worker *controller.Worker
		worker, err = controller.NewWorker(settings, controller.PostgresStore{}, controller.CommandEvaluator{}, workerId)
		if err == nil {
			err = worker.Run(quit.Ctx)
		}
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		panic(fmt.Errorf("competition worker: %w", err))
	}
}
