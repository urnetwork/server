// `sidecarctl sidecar restore` drives §19.10's restore frame (D-OPS-14, D-OPS-18; OPS-M3-03) for one incident, in the
// env WARP_ENV names, after xops's bringyour-pg-sidecar-restore.sh has loaded the dumps and started the incident's list:
//
//   - `sidecar restore <step>` runs one step, once every step before it in §19.10's order has ended under the
//     incident: ops, abuse, dns, api, labels, rows and billing, and after a site loss hardware, authority, groups,
//     repos, drops and close. A step that ended may run again: every step is idempotent and keyed by the incident.
//   - `sidecar restore run` runs every step that has not ended, in order, stopping at the first that fails or that no
//     task has registered yet.
//   - `sidecar restore end <step> --by-hand=<how>` records that the operator ran a step no task has registered, as the
//     statement says, once every step before it ended.
//   - `sidecar restore status` prints where the incident stands, reading only the list.
//
// --incident names the incident by its id, the same id the restore script ran under, and --list its list,
// incident-<id>.tsv. --kind, postgres or site, is recorded once, on the first command that gives it, and every later
// command must agree with it. --statement carries what a step asks the operator to state by hand, such as how
// restore_hold is held where sidecar.yml does not hold it. What the steps do is sidecarrestore's; this prints their
// progress, ids and counts only, never a name or an address.

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
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/sidecarrestore"
)

// The command's registration.
func init() {
	registerSidecarctlCommand(&sidecarctlCommand{
		words:   "sidecar restore",
		usage:   "<step>|run|status|end <step> --incident=<id> --list=<incident list> [--kind=postgres|site] [--statement=<text>] [--by-hand=<how>] [--timeout=<duration>]",
		summary: "Drive §19.10's restore frame for an incident in the env WARP_ENV names: run one step once the steps before it ended, run every step left in order, record a step run by hand, or print where the incident stands.",
		run:     runSidecarRestore,
	})
}

// How a run reaches its frame and env. Tests replace both.
type sidecarRestoreSettings struct {
	// The frame whose steps run: the process's, where each step's file registers its function.
	frame *sidecarrestore.Frame
	// Refuses a run outside an env. The status command reads only the list and skips it.
	requireEnv func() (string, error)
	// Bounds a run unless --timeout says otherwise: the api step walks every network's log and effects, which takes
	// minutes at year two's network count, and a run that ends first is run again. Non-positive expires at once.
	timeout time.Duration
}

// The settings of a run. A caller changes its own copy.
func defaultSidecarRestoreSettings() *sidecarRestoreSettings {
	return &sidecarRestoreSettings{
		frame:      sidecarrestore.DefaultFrame(),
		requireEnv: sidecarctlProcessEnv,
		timeout:    2 * time.Hour,
	}
}

// `sidecarctl sidecar restore`, ending early on SIGINT or SIGTERM.
func runSidecarRestore(args []string, out io.Writer) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runSidecarRestoreWithSettings(ctx, args, defaultSidecarRestoreSettings(), out)
}

// Parses the command line and runs what it names.
func runSidecarRestoreWithSettings(ctx context.Context, args []string, settings *sidecarRestoreSettings, out io.Writer) error {
	flags := flag.NewFlagSet("sidecar restore", flag.ContinueOnError)
	incidentFlag := flags.String("incident", "", "")
	listFlag := flags.String("list", "", "")
	kindFlag := flags.String("kind", "", "")
	statementFlag := flags.String("statement", "", "")
	byHandFlag := flags.String("by-hand", "", "")
	timeoutFlag := flags.Duration("timeout", settings.timeout, "")
	positionals, err := parseSidecarctlArgs(flags, args)
	if err != nil {
		return err
	}
	if len(positionals) == 0 {
		return sidecarctlUsageErrorf("name a step, run, status or end <step>")
	}
	action := positionals[0]
	stepName := ""
	switch {
	case action == "end":
		if len(positionals) != 2 {
			return sidecarctlUsageErrorf("end takes the step it records, got %q", positionals[1:])
		}
		if *byHandFlag == "" {
			return sidecarctlUsageErrorf("end needs --by-hand=<how the step ran>")
		}
		stepName = positionals[1]
	case len(positionals) != 1:
		return sidecarctlUsageErrorf("takes one step, run or status, got %q", positionals)
	case action != "run" && action != "status":
		stepName = action
		action = "step"
	}
	if *byHandFlag != "" && action != "end" {
		return sidecarctlUsageErrorf("--by-hand goes with end only")
	}
	if *incidentFlag == "" || *listFlag == "" {
		return sidecarctlUsageErrorf("--incident=<id> and --list=<incident list> name the incident")
	}
	incidentId, err := server.ParseId(*incidentFlag)
	if err != nil || incidentId == (server.Id{}) {
		return sidecarctlUsageErrorf("--incident=%q is not an incident id", *incidentFlag)
	}
	var kind sidecarrestore.IncidentKind
	if *kindFlag != "" {
		kind, err = sidecarrestore.ParseIncidentKind(*kindFlag)
		if err != nil {
			return sidecarctlUsageErrorf("%v", err)
		}
	}

	incident, err := sidecarrestore.OpenIncident(incidentId, *listFlag, nil)
	if err != nil {
		return err
	}
	if action == "status" {
		printSidecarRestoreStatus(out, settings.frame, incident)
		return nil
	}
	env, err := settings.requireEnv()
	if err != nil {
		return err
	}
	if kind != "" {
		if err := incident.SetKind(kind); err != nil {
			return err
		}
	} else if _, ok, err := incident.Kind(); err != nil {
		return err
	} else if !ok {
		return sidecarctlUsageErrorf("the list records no kind yet: give --kind=postgres or --kind=site")
	}

	ctx, cancel := context.WithTimeout(ctx, *timeoutFlag)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("nothing ran: the run's %v bound passed: %w", *timeoutFlag, err)
	}
	switch action {
	case "end":
		if err := settings.frame.EndStepByHand(incident, stepName, *byHandFlag); err != nil {
			return sidecarRestoreError(err)
		}
		fmt.Fprintf(out, "%s recorded as ended by hand under incident %s in %s\n", stepName, incidentId, env)
	case "run":
		err = sidecarctlServerCall(func() error { return settings.frame.Run(ctx, incident, out, *statementFlag) })
		if err != nil {
			return sidecarRestoreError(err)
		}
	default:
		err = sidecarctlServerCall(func() error {
			return settings.frame.RunStep(ctx, incident, stepName, out, *statementFlag)
		})
		if err != nil {
			return sidecarRestoreError(err)
		}
		fmt.Fprintf(out, "%s ended under incident %s in %s\n", stepName, incidentId, env)
	}
	return nil
}

// A frame refusal with what the operator does next, or the error as it is.
func sidecarRestoreError(err error) error {
	switch {
	case errors.Is(err, sidecarrestore.ErrStepUnknown):
		return sidecarctlUsageErrorf("%v", err)
	case errors.Is(err, sidecarrestore.ErrStepOutOfOrder):
		return fmt.Errorf("%w; sidecar restore status lists what has ended", err)
	}
	return err
}

// Each step of the incident and whether it ended, how, and whether a function is registered.
func printSidecarRestoreStatus(out io.Writer, frame *sidecarrestore.Frame, incident *sidecarrestore.Incident) {
	sidecarTime, mainTime, _ := incident.SnapshotTimes()
	fmt.Fprintf(out, "incident %s\n", incident.IncidentId())
	fmt.Fprintf(out, "sidecar dump %s, main dump %s\n", sidecarTime.Format(time.RFC3339Nano), mainTime.Format(time.RFC3339Nano))
	kind, ok, err := incident.Kind()
	switch {
	case err != nil:
		fmt.Fprintf(out, "kind: %v\n", err)
	case !ok:
		fmt.Fprintf(out, "kind: not recorded\n")
	default:
		fmt.Fprintf(out, "kind: %s\n", kind)
	}
	ended := map[string]string{}
	for _, row := range incident.RowsOfKind(sidecarrestore.ListKindStepEnded) {
		ended[row.Table] = row.Detail
	}
	for _, step := range frame.Steps() {
		if step.SiteOnly && kind != sidecarrestore.IncidentKindSite {
			continue
		}
		registered := "registered"
		if step.Function == nil {
			registered = "by hand"
		}
		if detail, ok := ended[step.Name]; ok {
			fmt.Fprintf(out, "%-10s ended %s\n", step.Name, detail)
		} else {
			fmt.Fprintf(out, "%-10s open (%s)\n", step.Name, registered)
		}
	}
}
