// Command monitor runs the reusable probes in server/monitor.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	servermonitor "github.com/urnetwork/server/v2026/monitor"
)

type stringFlags []string

func (f *stringFlags) String() string { return fmt.Sprint([]string(*f)) }
func (f *stringFlags) Set(value string) error {
	*f = append(*f, value)
	return nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	once := flag.Bool("once", false, "run every registered signal once and exit")
	mode := flag.String("mode", "", "SSH address mode: lan or overlay")
	keys := stringFlags{}
	excludedSignals := stringFlags{}
	excludedEdgeIPv6Hosts := stringFlags{}
	flag.Var(&keys, "ssh-key", "SSH identity path; may be repeated")
	flag.Var(&excludedSignals, "exclude-signal", "signal key, number, or ID to omit; may be repeated")
	flag.Var(&excludedEdgeIPv6Hosts, "exclude-edge-ipv6-host", "host whose exact public IPv6 paths should be paused; may be repeated")
	flag.Parse()

	settings, err := servermonitor.LoadSignalSettings()
	if err != nil {
		return err
	}
	if *mode != "" {
		settings.AddressMode = servermonitor.AddressMode(*mode)
	}
	if len(keys) > 0 {
		settings.SSHKeyPaths = append([]string(nil), keys...)
	}
	settings, err = servermonitor.ExcludeEdgeIPv6Hosts(settings, excludedEdgeIPv6Hosts...)
	if err != nil {
		return err
	}
	if err := settings.Validate(); err != nil {
		return err
	}
	signals, err := servermonitor.ExcludeSignals(servermonitor.NewSignals(), excludedSignals...)
	if err != nil {
		return err
	}
	monitor := servermonitor.NewWithSignals(settings, signals...)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
	defer stop()
	if *once {
		alerts, runErr := monitor.Run(ctx)
		if err := servermonitor.WriteAlertsMarkdown(os.Stdout, alerts); err != nil {
			return err
		}
		return runErr
	}
	return monitor.RunLoop(ctx, func(ctx context.Context, signal servermonitor.Signal, alerts servermonitor.Alerts) error {
		if len(alerts) == 0 {
			return nil
		}
		return servermonitor.WriteAlertsMarkdown(os.Stdout, alerts)
	})
}
