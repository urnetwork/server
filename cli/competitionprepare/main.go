// Runs only the trusted fenced preparation protocol. Configuration and tokens
// remain on the control plane; no candidate command or environment is accepted.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/urnetwork/server/controller"
)

// The supervisor invokes this executable without arguments and supplies one
// strict request on stdin. A private inventory can be selected explicitly.
func main() {
	defaultConfig := os.Getenv("SIM_LATENCY_PREPARATION_CONFIG")
	if defaultConfig == "" {
		defaultConfig = "/etc/urnetwork/competition-preparation.json"
	}
	configPath := flag.String("config", defaultConfig, "private preparation inventory JSON")
	flag.Parse()
	if flag.NArg() != 0 {
		panic("competitionprepare accepts no positional arguments")
	}
	request, err := controller.DecodeCompetitionPreparationRequest(os.Stdin)
	if err != nil {
		panic(err)
	}
	config, err := controller.LoadCompetitionPreparationConfig(*configPath)
	if err != nil {
		panic(err)
	}
	settings, err := controller.LoadSettings()
	if err != nil {
		panic(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT, syscall.SIGQUIT)
	defer stop()
	result, err := controller.RunCompetitionPreparation(ctx, settings, *config, *request)
	if err != nil {
		panic(fmt.Errorf("competition source preparation: %w", err))
	}
	if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
		panic(err)
	}
}
