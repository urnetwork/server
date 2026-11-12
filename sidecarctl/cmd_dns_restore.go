// `sidecarctl dns restoring on|off` and `sidecarctl dns restore --incident=<id>` are §19.10's dns step as a runbook
// runs it by hand (§12.6's restore; D-DNS-10, D-OPS-14; DNS-M5-15). Both are short commands over
// dnsservice.RestoreStep, which dnshost.NewRestoreStep builds on the env's Postgres and Redis, needing neither api
// nor the token exchange. OPS's restore frame calls the same step itself, so these are the runbook's reruns.
//
//   - `dns restoring on` sets dns_root.restoring as the dumps load, before any block reads them: from then every
//     dns write answers 503 and takes no serial. `dns restoring off` is by hand only to abandon a restore, since
//     the step clears it itself at its end.
//   - `dns restore --incident=<id>` runs the step for the incident from wherever an earlier run left it: each NS's
//     SOA, the one transaction that lands the root's serial above every serial the lost primary issued, the wait
//     for every running NS to serve it and post its diff, the lists, and restoring cleared. It is idempotent by
//     incident id. A run that ends before the step does, at its bound or a signal, prints where it stood and exits
//     1, and a rerun with the same incident goes on from there.
//
// The root is dns.yml's when it lists one, and --root otherwise. What a restore listed is in dns_restore_incident
// and each network's admin events; this prints counts and never an owner name, since those are user data.
//
// Both reach the env WARP_ENV names, as bringyourctl's commands do, and refuse with no WARP_ENV. The dns tables
// are MG5's, which a binary links only once the group is registered, or under the sidecar_pending tag in local,
// canary and tests (server/db_migrations_sidecar.go): a binary without them refuses every call with
// model.ErrDnsUnlinked, changing nothing.

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/dnsservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/dnshost"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/sidecarhost"
)

// The two commands' registrations.
func init() {
	registerSidecarctlCommand(&sidecarctlCommand{
		words:   "dns restoring",
		usage:   "on|off [--root=<root>]",
		summary: "Set or clear dns_root.restoring in the env WARP_ENV names (§19.10): on as the dumps load, so every dns write answers 503; off only to abandon a restore, since dns restore clears it at its end.",
		run:     runDnsRestoring,
	})
	registerSidecarctlCommand(&sidecarctlCommand{
		words:   "dns restore",
		usage:   "--incident=<id> [--root=<root>] [--timeout=<duration>]",
		summary: "Run §12.6's restore step for an incident in the env WARP_ENV names, from wherever an earlier run left it: land the serial above every serial the lost primary issued, wait for each running NS to serve it, list what changed, and clear restoring.",
		run:     runDnsRestore,
	})
}

// The restore step as the commands use it, which dnsservice.RestoreStep is.
type dnsRestoreStep interface {
	// Sets or clears restoring for root.
	SetRestoring(ctx context.Context, root string, restoring bool) error
	// Runs the step for incidentId on root, answering where it stood even when it returns an error.
	Run(ctx context.Context, root string, incidentId sidecar.Id) (*dnsservice.DnsRestoreProgress, error)
}

// One env as the commands reach it.
type dnsRestoreEnv struct {
	// the env's name, for what the commands print
	env string
	// the roots dns.yml serves, canonical, each ending in a dot
	roots []string
	step  dnsRestoreStep
	// releases what the step holds
	close func()
}

// How a run reaches its env. Tests replace the env.
type dnsRestoreSettings struct {
	// Bounds `dns restoring`: one row's write. Non-positive expires at once, so nothing is written.
	restoringTimeout time.Duration
	// Bounds `dns restore` unless --timeout says otherwise. The step waits for every running NS to reload and
	// serve the restore serial, each at its next poll, then restore.diff_wait_s for their diffs; half an hour
	// covers that with an NS slow to come back, and a run that ends first is rerun. Non-positive expires at once.
	restoreTimeout time.Duration
	// Opens the env WARP_ENV names, with the step's log going to log.
	openEnv func(ctx context.Context, log sidecar.Logger) (*dnsRestoreEnv, error)
}

// The settings of a run. A caller changes its own copy.
func defaultDnsRestoreSettings() *dnsRestoreSettings {
	return &dnsRestoreSettings{
		restoringTimeout: 30 * time.Second,
		restoreTimeout:   30 * time.Minute,
		openEnv:          openServerDnsRestoreEnv,
	}
}

// The env as server reaches it: dns.yml through server's config, and the step over dnshost.Store on the env's
// Postgres and Redis, reading each NS's SOA over TCP at dns.yml's glue.
func openServerDnsRestoreEnv(ctx context.Context, log sidecar.Logger) (*dnsRestoreEnv, error) {
	env, err := sidecarctlProcessEnv()
	if err != nil {
		return nil, err
	}
	var opened *dnsRestoreEnv
	err = sidecarctlServerCall(func() error {
		resource, err := server.Config.SimpleResource("dns.yml")
		if err != nil {
			return fmt.Errorf("dns.yml: %w", err)
		}
		data, err := resource.BytesE()
		if err != nil {
			return fmt.Errorf("dns.yml: %w", err)
		}
		settings, err := dnsservice.LoadDnsSettings(data)
		if err != nil {
			return fmt.Errorf("dns.yml: %w", err)
		}
		store, err := dnshost.NewStore(sidecarhost.NewRedis(nil))
		if err != nil {
			return err
		}
		step, err := dnshost.NewRestoreStep(store, settings, nil, log)
		if err != nil {
			store.Close()
			return err
		}
		opened = &dnsRestoreEnv{env: env, roots: slices.Clone(settings.Roots), step: step, close: store.Close}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return opened, nil
}

// The step's log, written to a command's output a line at a time. Safe for concurrent use.
type dnsRestoreLogger struct {
	stateLock sync.Mutex
	out       io.Writer
}

// One line, ended once.
func (self *dnsRestoreLogger) line(text string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	fmt.Fprintln(self.out, strings.TrimSuffix(text, "\n"))
}

// See sidecar.Logger.
func (self *dnsRestoreLogger) Info(args ...any) {
	self.line(fmt.Sprint(args...))
}

// See sidecar.Logger.
func (self *dnsRestoreLogger) Infof(format string, args ...any) {
	self.line(fmt.Sprintf(format, args...))
}

// See sidecar.Logger.
func (self *dnsRestoreLogger) Warningf(format string, args ...any) {
	self.line("warning: " + fmt.Sprintf(format, args...))
}

// See sidecar.Logger.
func (self *dnsRestoreLogger) Errorf(format string, args ...any) {
	self.line("error: " + fmt.Sprintf(format, args...))
}

// No verbose level is on: the step's own lines are what a runbook reads.
func (self *dnsRestoreLogger) V(level int32) sidecar.Verbose {
	return sidecar.NewNoopLogger().V(level)
}

// The root a command works on: --root when given, in any case and with or without its final dot, which must be
// one dns.yml serves, and otherwise dns.yml's root when it lists exactly one.
func dnsYmlRoot(roots []string, rootFlag string) (string, error) {
	if rootFlag == "" {
		if len(roots) != 1 {
			return "", sidecarctlUsageErrorf("dns.yml serves %d roots %q, so --root must name one", len(roots), roots)
		}
		return roots[0], nil
	}
	root := strings.ToLower(strings.TrimSuffix(rootFlag, ".")) + "."
	if !slices.Contains(roots, root) {
		return "", sidecarctlUsageErrorf("%s is not a root dns.yml serves, which are %q", root, roots)
	}
	return root, nil
}

// `sidecarctl dns restoring on|off`, against the env as server reaches it.
func runDnsRestoring(args []string, out io.Writer) error {
	return runDnsRestoringWithSettings(args, defaultDnsRestoreSettings(), out)
}

// Parses the command line and sets or clears restoring. Only a write the store took prints anything.
func runDnsRestoringWithSettings(args []string, settings *dnsRestoreSettings, out io.Writer) error {
	flags := flag.NewFlagSet("dns restoring", flag.ContinueOnError)
	rootFlag := flags.String("root", "", "")
	positionals, err := parseSidecarctlArgs(flags, args)
	if err != nil {
		return err
	}
	if len(positionals) != 1 || (positionals[0] != "on" && positionals[0] != "off") {
		return sidecarctlUsageErrorf("want on or off, got %q", positionals)
	}
	restoring := positionals[0] == "on"

	ctx, cancel := context.WithTimeout(context.Background(), settings.restoringTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("restoring was not changed: the run's %v bound passed: %w", settings.restoringTimeout, err)
	}
	env, err := settings.openEnv(ctx, &dnsRestoreLogger{out: out})
	if err != nil {
		return fmt.Errorf("restoring was not changed: %w", err)
	}
	defer env.close()
	root, err := dnsYmlRoot(env.roots, *rootFlag)
	if err != nil {
		return err
	}
	if err := sidecarctlServerCall(func() error { return env.step.SetRestoring(ctx, root, restoring) }); err != nil {
		return fmt.Errorf("restoring was not changed for %s in %s: %w", root, env.env, err)
	}
	if restoring {
		fmt.Fprintf(out, "restoring is on for %s in %s: every dns write answers 503 and takes no serial until `sidecarctl dns restore --incident=<id>` ends the restore\n", root, env.env)
	} else {
		fmt.Fprintf(out, "restoring is off for %s in %s: dns writes commit again, and a restore that had not ended is abandoned\n", root, env.env)
	}
	return nil
}

// `sidecarctl dns restore --incident=<id>`, against the env as server reaches it, ending early on SIGINT or SIGTERM.
func runDnsRestore(args []string, out io.Writer) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runDnsRestoreWithSettings(ctx, args, defaultDnsRestoreSettings(), out)
}

// Parses the command line and runs the step under ctx and the run's bound, printing where it stood. Only a step
// that ended returns nil.
func runDnsRestoreWithSettings(ctx context.Context, args []string, settings *dnsRestoreSettings, out io.Writer) error {
	flags := flag.NewFlagSet("dns restore", flag.ContinueOnError)
	incidentFlag := flags.String("incident", "", "")
	rootFlag := flags.String("root", "", "")
	timeoutFlag := flags.Duration("timeout", settings.restoreTimeout, "")
	positionals, err := parseSidecarctlArgs(flags, args)
	if err != nil {
		return err
	}
	if len(positionals) != 0 {
		return sidecarctlUsageErrorf("takes flags only, got %q", positionals)
	}
	if *incidentFlag == "" {
		return sidecarctlUsageErrorf("--incident=<id> names the incident the restore is recorded under")
	}
	incidentId, err := server.ParseId(*incidentFlag)
	if err != nil || incidentId == (server.Id{}) {
		return sidecarctlUsageErrorf("--incident=%q is not an incident id", *incidentFlag)
	}

	ctx, cancel := context.WithTimeout(ctx, *timeoutFlag)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("the restore step did not run: the run's %v bound passed: %w", *timeoutFlag, err)
	}
	env, err := settings.openEnv(ctx, &dnsRestoreLogger{out: out})
	if err != nil {
		return fmt.Errorf("the restore step did not run: %w", err)
	}
	defer env.close()
	root, err := dnsYmlRoot(env.roots, *rootFlag)
	if err != nil {
		return err
	}
	var progress *dnsservice.DnsRestoreProgress
	err = sidecarctlServerCall(func() error {
		var err error
		progress, err = env.step.Run(ctx, root, sidecar.Id(incidentId))
		return err
	})
	if progress != nil {
		printDnsRestoreProgress(out, env.env, progress)
	}
	switch {
	case errors.Is(err, dnsservice.ErrDnsNotRestoring):
		return fmt.Errorf("%s is not restoring in %s, and no restore ran under incident %s: run `sidecarctl dns restoring on` as the dumps load, then this: %w", root, env.env, incidentId, err)
	case errors.Is(err, model.ErrDnsUnlinked):
		return fmt.Errorf("the restore step for %s in %s did not run and changed nothing: %w", root, env.env, err)
	case errors.Is(err, dnsservice.ErrDnsRootUnknown):
		return fmt.Errorf("%s has no dns_root row in %s, so there is nothing to restore: the dumps hold the row of a root that was ever served: %w", root, env.env, err)
	case errors.Is(err, dnsservice.ErrDnsRestoreIncident):
		return fmt.Errorf("%s's restore in %s was committed under another incident, so this one changed nothing: %w", root, env.env, err)
	case err != nil:
		return fmt.Errorf("the restore step for %s in %s has not ended; rerun with --incident=%s to go on from where it stood: %w", root, env.env, incidentId, err)
	case progress == nil || !progress.Done:
		return fmt.Errorf("the restore step for %s in %s answered no end; rerun with --incident=%s", root, env.env, incidentId)
	}
	return nil
}

// Where a run stood, in counts and NS labels: never an owner name or a network.
func printDnsRestoreProgress(out io.Writer, env string, progress *dnsservice.DnsRestoreProgress) {
	fmt.Fprintf(out, "%s in %s, incident %s\n", progress.Root, env, progress.IncidentId)
	if progress.RestoreSerial == 0 {
		fmt.Fprintf(out, "restore serial: not landed\n")
	} else {
		fmt.Fprintf(out, "restore serial: %d\n", progress.RestoreSerial)
	}
	fmt.Fprintf(out, "ns stopped until they serve it: %s\n", dnsRestoreNsText(progress.StoppedNs))
	fmt.Fprintf(out, "ns that did not answer and are still counted: %s\n", dnsRestoreNsText(progress.CountedNs))
	fmt.Fprintf(out, "ns not yet serving it: %s\n", dnsRestoreNsText(progress.WaitingNs))
	fmt.Fprintf(out, "diffs posted at or above it: %d\n", progress.DiffCount)
	if progress.Listing != nil {
		flaggedCount := 0
		for _, entry := range progress.Listing.Entries {
			if entry.Flagged && entry.FlagClearTime == nil {
				flaggedCount += 1
			}
		}
		fmt.Fprintf(out, "rrsets listed under the incident: %d, %d of them flagged for an open abuse case\n", len(progress.Listing.Entries), flaggedCount)
	}
	if progress.Done {
		fmt.Fprintf(out, "done: restoring is clear, and dns writes commit above the restore serial\n")
	}
}

// NS labels on one line, or none.
func dnsRestoreNsText(ns []string) string {
	if len(ns) == 0 {
		return "none"
	}
	return strings.Join(ns, " ")
}
