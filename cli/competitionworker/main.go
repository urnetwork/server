// The long-running worker and its early-return host probes retain signal
// ownership until evaluator process groups have been canceled and joined.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/docopt/docopt-go"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
)

// Dispatches exact-generation probes before starting ordinary worker services.
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
  competitionworker [--worker_id=<id>] [--baseline-preparation-config=<path> --baseline-preparation-sha256=<sha>] [--future-baseline-preparation-config=<path> --future-baseline-preparation-sha256=<sha>]
  competitionworker --check [--worker_id=<id>] [--baseline-preparation-config=<path> --baseline-preparation-sha256=<sha>] [--future-baseline-preparation-config=<path> --future-baseline-preparation-sha256=<sha>]
  competitionworker --supervisor-request [--worker_id=<id>] [--baseline-preparation-config=<path> --baseline-preparation-sha256=<sha>] [--future-baseline-preparation-config=<path> --future-baseline-preparation-sha256=<sha>]
  competitionworker --source-readiness=<generation> [--worker_id=<id>] [--baseline-preparation-config=<path> --baseline-preparation-sha256=<sha>] [--future-baseline-preparation-config=<path> --future-baseline-preparation-sha256=<sha>]
  competitionworker -h | --help
  competitionworker --version

Options:
  -h --help          Show this screen.
  --version          Show version.
  --check            Verify the host and database without claiming work.
  --supervisor-request  Read strict fenced v2 round identity from stdin; never select another round.
  --source-readiness=<generation>  Probe and acknowledge this exact installed v2 source generation.
  --baseline-preparation-config=<path>  Root-pinned full-R9 preparation role; does not change API or submission budget.
  --baseline-preparation-sha256=<sha>  Exact immutable preparation configuration SHA-256.
  --future-baseline-preparation-config=<path>  Separately pinned future-policy role; inactive for current rounds.
  --future-baseline-preparation-sha256=<sha>  Exact immutable future preparation configuration SHA-256.
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
	baselineConfig, _ := opts.String("--baseline-preparation-config")
	baselineDigest, _ := opts.String("--baseline-preparation-sha256")
	if (baselineConfig == "") != (baselineDigest == "") {
		panic("baseline preparation requires both the config path and its exact SHA-256")
	}
	if baselineConfig != "" {
		if err := controller.ConfigureCompetitionRoundBaselinePreparation(settings, baselineConfig, baselineDigest); err != nil {
			panic(err)
		}
	}
	futureBaselineConfig, _ := opts.String("--future-baseline-preparation-config")
	futureBaselineDigest, _ := opts.String("--future-baseline-preparation-sha256")
	if (futureBaselineConfig == "") != (futureBaselineDigest == "") {
		panic("future baseline preparation requires both the config path and its exact SHA-256")
	}
	if futureBaselineConfig != "" {
		if settings.FutureEpochPolicy == nil {
			panic("future baseline preparation requires an explicitly approved future epoch policy")
		}
		if err := controller.ConfigureCompetitionFutureRoundBaselinePreparation(settings, futureBaselineConfig, futureBaselineDigest); err != nil {
			panic(err)
		}
	}
	supervised, _ := opts.Bool("--supervisor-request")
	readinessGeneration, _ := opts.String("--source-readiness")
	if readinessGeneration != "" {
		generation, err := strconv.ParseInt(readinessGeneration, 10, 64)
		if err != nil || generation < 1 || server.RequireEnv() != "main" {
			panic("source readiness requires an exact positive generation and main environment")
		}
		if err := runWorkerProbe(func(ctx context.Context) error {
			return controller.AcknowledgeCompetitionSourceWorker(ctx, settings, generation, workerId)
		}); err != nil {
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
		if err := runWorkerProbe(worker.Preflight); err != nil {
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

// A probe owns its cancellation through synchronous cleanup. Exiting directly
// on a signal would orphan the self-check's separate evaluator process group.
func runWorkerProbe(probe func(context.Context) error) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGQUIT, syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	return probe(ctx)
}
