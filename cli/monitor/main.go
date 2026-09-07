// Command monitor runs the reusable probes in server/monitor.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	servermonitor "github.com/urnetwork/server/monitor"
)

type stringFlags []string

func (f *stringFlags) String() string { return fmt.Sprint([]string(*f)) }
func (f *stringFlags) Set(value string) error {
	*f = append(*f, value)
	return nil
}

const (
	alertFormatMarkdown = "markdown"
	alertFormatJSONL    = "jsonl"
)

type monitorOptions struct {
	once                  bool
	listSignals           bool
	mode                  string
	format                string
	keys                  stringFlags
	includedSignals       stringFlags
	excludedSignals       stringFlags
	excludedEdgeIPv6Hosts stringFlags
}

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, stdout io.Writer) error {
	return runWithSettingsLoader(args, stdout, servermonitor.LoadSignalSettings)
}

func runWithSettingsLoader(args []string, stdout io.Writer, loadSettings func() (servermonitor.SignalSettings, error)) error {
	opts, err := parseMonitorOptions(args)
	if err != nil {
		return err
	}

	signals := servermonitor.NewSignals()
	if opts.listSignals {
		return writeSignalList(stdout, signals)
	}
	if len(opts.includedSignals) > 0 {
		signals, err = servermonitor.IncludeSignals(signals, opts.includedSignals...)
	} else {
		signals, err = servermonitor.ExcludeSignals(signals, opts.excludedSignals...)
	}
	if err != nil {
		return err
	}

	settings, err := loadSettings()
	if err != nil {
		return err
	}
	if opts.mode != "" {
		settings.AddressMode = servermonitor.AddressMode(opts.mode)
	}
	if len(opts.keys) > 0 {
		settings.SSHKeyPaths = append([]string(nil), opts.keys...)
	}
	settings, err = servermonitor.ExcludeEdgeIPv6Hosts(settings, opts.excludedEdgeIPv6Hosts...)
	if err != nil {
		return err
	}
	if err := settings.Validate(); err != nil {
		return err
	}
	monitor := servermonitor.NewWithSignals(settings, signals...)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
	defer stop()
	if opts.once {
		alerts, runErr := monitor.Run(ctx)
		if err := writeAlerts(stdout, opts.format, alerts); err != nil {
			return err
		}
		return runErr
	}
	return monitor.RunLoop(ctx, func(ctx context.Context, signal servermonitor.Signal, alerts servermonitor.Alerts) error {
		if len(alerts) == 0 {
			return nil
		}
		return writeAlerts(stdout, opts.format, alerts)
	})
}

func parseMonitorOptions(args []string) (monitorOptions, error) {
	flags := flag.NewFlagSet("monitor", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var opts monitorOptions
	flags.BoolVar(&opts.once, "once", false, "run every selected signal once and exit")
	flags.BoolVar(&opts.listSignals, "list-signals", false, "list registered signal identifiers and exit without loading settings")
	flags.StringVar(&opts.mode, "mode", "", "SSH address mode: lan or overlay")
	flags.StringVar(&opts.format, "format", alertFormatMarkdown, "alert output format: markdown or jsonl")
	flags.Var(&opts.keys, "ssh-key", "SSH identity path; may be repeated")
	flags.Var(&opts.includedSignals, "include-signal", "signal key, number, or ID to run; may be repeated")
	flags.Var(&opts.excludedSignals, "exclude-signal", "signal key, number, or ID to omit; may be repeated")
	flags.Var(&opts.excludedEdgeIPv6Hosts, "exclude-edge-ipv6-host", "host whose exact public IPv6 paths should be paused; may be repeated")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return monitorOptions{}, errors.New("usage: monitor [-once] [-list-signals] [-format markdown|jsonl] [-include-signal IDENTIFIER | -exclude-signal IDENTIFIER]")
	}
	if len(opts.includedSignals) > 0 && len(opts.excludedSignals) > 0 {
		return monitorOptions{}, errors.New("monitor: -include-signal and -exclude-signal are mutually exclusive")
	}
	if opts.format != alertFormatMarkdown && opts.format != alertFormatJSONL {
		return monitorOptions{}, fmt.Errorf("monitor: format %q is not supported; use markdown or jsonl", opts.format)
	}
	return opts, nil
}

func writeSignalList(w io.Writer, signals []servermonitor.Signal) error {
	if _, err := fmt.Fprintln(w, "NUMBER\tKEY\tID\tNAME"); err != nil {
		return err
	}
	for _, signal := range signals {
		if _, err := fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", signal.Number(), signal.Key(), signal.ID(), signal.Name()); err != nil {
			return err
		}
	}
	return nil
}

func writeAlerts(w io.Writer, format string, alerts servermonitor.Alerts) error {
	switch format {
	case alertFormatMarkdown:
		return servermonitor.WriteAlertsMarkdown(w, alerts)
	case alertFormatJSONL:
		return servermonitor.WriteAlertsJSONL(w, alerts)
	default:
		return fmt.Errorf("monitor: format %q is not supported", format)
	}
}
