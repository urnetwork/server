package main

// `monitor check-burn --service=<svc>` reads one sidecar service's SLO burn now (SIGNALS.md §30.1;
// OPS-M3-02, D-OPS-9), for build/all's warp_rollout_gate before each rollout wave. It prints a line for each
// objective, or for the dns canary where the service is dns or a dns edge (Q-OPS-5), then the service's
// state, and exits 0 only when that is clear: burning, and unknown (an objective with no budget, a service
// whose row has not joined monitor.yml's sidecar_services, or settings that do not load), exit 1, which
// stops the step as a burn does (plan-OPS-small-readings #11). An operator overrides a stop by naming the
// service in the rollout's ROLLOUT_STOP_OVERRIDE, never here.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	servermonitor "github.com/urnetwork/server/monitor"
)

// The check behind the command; tests replace it.
type checkBurnFunction func(ctx context.Context, settings servermonitor.SignalSettings, service string) (*servermonitor.SloBurnCheck, error)

// Runs the command on args, which follow check-burn.
func runCheckBurnCommand(ctx context.Context, args []string, stdout io.Writer, loadSettings func() (servermonitor.SignalSettings, error), check checkBurnFunction) error {
	flags := flag.NewFlagSet("check-burn", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	service := flags.String("service", "", "the service whose burn to read, as warp names it")
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("usage: monitor check-burn --service=<svc>: %w", err)
	}
	if *service == "" || flags.NArg() != 0 {
		return errors.New("usage: monitor check-burn --service=<svc>")
	}
	settings, err := loadSettings()
	if err != nil {
		fmt.Fprintf(stdout, "%s: %s\n", *service, servermonitor.SloBurnUnknown)
		return fmt.Errorf("%s: %s: the monitor's settings did not load: %w", *service, servermonitor.SloBurnUnknown, err)
	}
	result, err := check(ctx, settings, *service)
	if err != nil {
		fmt.Fprintf(stdout, "%s: %s\n", *service, servermonitor.SloBurnUnknown)
		return fmt.Errorf("%s: %s: %w", *service, servermonitor.SloBurnUnknown, err)
	}
	for _, line := range result.Lines {
		fmt.Fprintf(stdout, "%s %s\n", *service, line)
	}
	fmt.Fprintf(stdout, "%s: %s\n", *service, result.State)
	if result.State != servermonitor.SloBurnClear {
		return fmt.Errorf("%s: %s", *service, result.State)
	}
	return nil
}
