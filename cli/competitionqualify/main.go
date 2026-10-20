// Runs the root-owned real-host qualification broker. It never accepts a
// caller's successful evaluation, source approval, or readiness claim.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/urnetwork/server/controller"
)

// Strict parsing precedes configuration and credential access. Signals cancel
// the bounded evaluator and join its process-group cleanup before returning.
func run() error {
	flags := flag.NewFlagSet("competitionqualify", flag.ContinueOnError)
	configPath := os.Getenv("SIM_LATENCY_QUALIFICATION_CONFIG")
	if configPath == "" {
		configPath = "/etc/urnetwork/competition-qualification.json"
	}
	flags.StringVar(&configPath, "config", configPath, "root-owned qualification configuration")
	if err := flags.Parse(os.Args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("qualification accepts only --config and a strict preparation request on stdin")
	}
	request, err := controller.DecodeCompetitionPreparationRequest(os.Stdin)
	if err != nil {
		return err
	}
	config, err := controller.LoadCompetitionHostQualificationConfig(configPath)
	if err != nil {
		return err
	}
	settings, err := controller.LoadSettings()
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	result, err := controller.RunCompetitionHostQualification(ctx, settings, *config, *request)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(result)
}

// Errors never include the private request body, source-promoter token, or
// evaluator stderr. Failed evidence remains in the restricted host directory.
func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "competition host qualification:", err)
		os.Exit(1)
	}
}
