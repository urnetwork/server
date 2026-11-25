// `sidecarctl compute provider ls|gate|suspend|resume` (PROV-M12-01; DESIGN-COMPUTE §4.7, §14.10): the compute rota's
// record of each provider's gates and status, in the env's compute_provider table, which every broker and job's
// registry reads before it lets a provider serve (sidecar's computeservice/provider_registry.go).
//
//   - ls prints every provider this build knows, by its slug and fixed code: its kind, its status and since when, its
//     own gate's latest result, G-PROV-any's, and when G-PROV-any is next due, 90 days after its latest pass, marked
//     overdue past it. A provider with no row yet reads listed.
//   - gate records one gate's result: the provider's own gate (G-PROV-engy, G-SRCH-parallel) or G-PROV-any. A pass of
//     its own gate admits it; a failure takes a gated or suspended provider out for new work, as a failed G-PROV-any
//     must (§4.7). A gate with no result is refused, so nothing is recorded that no one judged.
//   - suspend takes a gated provider out for new work with its reason, as when the provider suspends URnetwork's
//     account (§14.10's provider adapters row); resume returns a suspended one to gated.
//
// Every change is the store's one transaction, entered in the row's history with its time and the operator named, and
// reaches every registry within its status age. The commands reach one env, the one WARP_ENV names, through server's
// Postgres as the env's services do, so each refuses an <env> that is not it before reading or writing anything: the
// operator names the env twice, and a slip in either changes nothing. A binary that does not link MG-C2, which every
// release build is until M12's Gn-out registers it, holds no compute_provider table, and every command refuses there.

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"regexp"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/urnetwork/sidecar/computeservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/core"
)

// An env's name: one directory under config/ and vault/, as warp lays them out.
var computeProviderEnvRegex = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// The commands' registration. It is the only change to the binary they need.
func init() {
	registerSidecarctlCommand(&sidecarctlCommand{
		words:   "compute provider ls",
		usage:   "<env>",
		summary: "List every compute provider's status in the env WARP_ENV names, its own gate's latest result and G-PROV-any's, and when G-PROV-any is next due.",
		run:     runComputeProviderLs,
	})
	registerSidecarctlCommand(&sidecarctlCommand{
		words:   "compute provider gate",
		usage:   "<env> <provider> --gate=<G-PROV-provider|G-SRCH-provider|G-PROV-any> --result=pass|fail --by=<operator> [--evidence=<reference>] [--note=<text>]",
		summary: "Record one gate's result for a compute provider: a pass of its own gate admits it, and a failed gate takes it out for new work (§4.7).",
		run:     runComputeProviderGate,
	})
	registerSidecarctlCommand(&sidecarctlCommand{
		words:   "compute provider suspend",
		usage:   "<env> <provider> --by=<operator> --reason=<text>",
		summary: "Suspend a gated compute provider for new work, as when it suspends URnetwork's account (§14.10); resume lifts it.",
		run:     runComputeProviderSuspend,
	})
	registerSidecarctlCommand(&sidecarctlCommand{
		words:   "compute provider resume",
		usage:   "<env> <provider> --by=<operator> [--note=<text>]",
		summary: "Return a suspended compute provider to gated.",
		run:     runComputeProviderResume,
	})
}

// What the commands read and write: computeservice.ComputeProviderStore over the env's Postgres, or a test's.
type computeProviderCommandStore interface {
	Providers(ctx context.Context) ([]*computeservice.ComputeProviderRow, error)
	RecordGate(ctx context.Context, id computeservice.ComputeProviderId, result *computeservice.ComputeProviderGateResult, at time.Time) (*computeservice.ComputeProviderRow, error)
	Suspend(ctx context.Context, id computeservice.ComputeProviderId, change *computeservice.ComputeProviderStatusChange, at time.Time) (*computeservice.ComputeProviderRow, error)
	Resume(ctx context.Context, id computeservice.ComputeProviderId, change *computeservice.ComputeProviderStatusChange, at time.Time) (*computeservice.ComputeProviderRow, error)
}

// How a run reaches its env. Tests replace the clock and the store.
type computeProviderCommandSettings struct {
	// Bounds the whole run, the open, the read and the write, so an env whose Postgres does not answer fails the
	// runbook step rather than holding it. Non-positive expires at once, so nothing is read or written.
	timeout time.Duration
	// The time a change is recorded at.
	now func() time.Time
	// The providers' settings: the history's bounds, G-PROV-any's interval, and how long registries take to read a
	// change.
	providers *computeservice.ComputeProviderSettings
	// Opens env's store, refusing an env this process does not run as and a binary without MG-C2's tables.
	openStore func(ctx context.Context, env string, providers *computeservice.ComputeProviderSettings) (computeProviderCommandStore, error)
}

// The settings of a run. A caller changes its own copy.
func defaultComputeProviderCommandSettings() *computeProviderCommandSettings {
	return &computeProviderCommandSettings{
		// a row's read and one transaction, well under a second on a healthy env, with room for a pool's first connect
		timeout:   30 * time.Second,
		now:       time.Now,
		providers: computeservice.DefaultComputeProviderSettings(),
		openStore: openServerComputeProviderStore,
	}
}

// The env as server reaches it, which is the one WARP_ENV names: any other env, no WARP_ENV, or a binary that does not
// link MG-C2 is refused before anything is read.
func openServerComputeProviderStore(ctx context.Context, env string, providers *computeservice.ComputeProviderSettings) (computeProviderCommandStore, error) {
	processEnv, err := server.Env()
	if err != nil {
		return nil, fmt.Errorf("WARP_ENV is not set, and this reaches the env it names; run it as that env's services run, with WARP_ENV=%s", env)
	}
	if processEnv != env {
		return nil, fmt.Errorf("this process runs as WARP_ENV=%s and was asked for %s; run it with WARP_ENV=%s", processEnv, env, env)
	}
	if !server.SidecarMgC2Linked() {
		return nil, errors.New("this binary does not link MG-C2, which holds compute_provider: until M12's Gn-out registers the group, only a sidecar_pending build of a local or canary env has the table")
	}
	return computeservice.NewComputeProviderStore(core.NewDb(), providers), nil
}

// `sidecarctl compute provider ls`, against the env as server reaches it.
func runComputeProviderLs(args []string, out io.Writer) error {
	return runComputeProviderLsWithSettings(args, defaultComputeProviderCommandSettings(), out)
}

// `sidecarctl compute provider gate`, against the env as server reaches it.
func runComputeProviderGate(args []string, out io.Writer) error {
	return runComputeProviderGateWithSettings(args, defaultComputeProviderCommandSettings(), out)
}

// `sidecarctl compute provider suspend`, against the env as server reaches it.
func runComputeProviderSuspend(args []string, out io.Writer) error {
	return runComputeProviderChangeWithSettings(args, computeservice.ComputeProviderHistorySuspend, defaultComputeProviderCommandSettings(), out)
}

// `sidecarctl compute provider resume`, against the env as server reaches it.
func runComputeProviderResume(args []string, out io.Writer) error {
	return runComputeProviderChangeWithSettings(args, computeservice.ComputeProviderHistoryResume, defaultComputeProviderCommandSettings(), out)
}

// The env, and the provider where want is 2, from positionals: a usage error for any other count, an env that is not
// a directory's name, or a slug this build does not know.
func computeProviderPositionals(positionals []string, want int) (string, computeservice.ComputeProviderId, error) {
	if len(positionals) != want {
		return "", "", sidecarctlUsageErrorf("want %d arguments, got %q", want, positionals)
	}
	env := positionals[0]
	if !computeProviderEnvRegex.MatchString(env) {
		return "", "", sidecarctlUsageErrorf("%q is not an env's name, one directory under config/", env)
	}
	if want == 1 {
		return env, "", nil
	}
	id := computeservice.ComputeProviderId(positionals[1])
	if _, known := computeservice.ComputeProviderKindOf(id); !known {
		return "", "", sidecarctlUsageErrorf("%q is no compute provider; the providers are %s", id, computeProviderSlugList())
	}
	return env, id, nil
}

// Every slug, as a usage error lists them.
func computeProviderSlugList() string {
	slugs := []string{}
	for _, id := range computeservice.ComputeProviderIds() {
		slugs = append(slugs, string(id))
	}
	return strings.Join(slugs, ", ")
}

// Runs callback on env's store within the run's bound, refusing a run past it before each step.
func withComputeProviderStore(env string, settings *computeProviderCommandSettings, callback func(ctx context.Context, store computeProviderCommandStore) error) error {
	ctx, cancel := context.WithTimeout(context.Background(), settings.timeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("nothing was read or written in %s: the run's %v bound passed: %w", env, settings.timeout, err)
	}
	store, err := settings.openStore(ctx, env, settings.providers)
	if err == nil && store == nil {
		err = errors.New("it opened no store")
	}
	if err != nil {
		return fmt.Errorf("nothing was read or written in %s: %w", env, err)
	}
	return callback(ctx, store)
}

// Parses ls's command line and prints every provider's row.
func runComputeProviderLsWithSettings(args []string, settings *computeProviderCommandSettings, out io.Writer) error {
	flags := flag.NewFlagSet("compute provider ls", flag.ContinueOnError)
	positionals, err := parseSidecarctlArgs(flags, args)
	if err != nil {
		return err
	}
	env, _, err := computeProviderPositionals(positionals, 1)
	if err != nil {
		return err
	}
	return withComputeProviderStore(env, settings, func(ctx context.Context, store computeProviderCommandStore) error {
		rows, err := store.Providers(ctx)
		if err != nil {
			return fmt.Errorf("compute_provider in %s does not read: %w", env, err)
		}
		writeComputeProviderTable(out, env, rows, settings.now(), settings.providers.GateRecheckInterval)
		return nil
	})
}

// Every provider this build knows, one line each, with rows standing for those that have one.
func writeComputeProviderTable(out io.Writer, env string, rows []*computeservice.ComputeProviderRow, now time.Time, interval time.Duration) {
	rowsById := map[computeservice.ComputeProviderId]*computeservice.ComputeProviderRow{}
	for _, row := range rows {
		rowsById[row.Id] = row
	}
	fmt.Fprintf(out, "compute providers in %s; G-PROV-any falls due %d days after its latest pass\n", env, int(interval/(24*time.Hour)))
	table := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(table, "provider\tcode\tkind\tstatus\tsince\tgate\tG-PROV-any\tG-PROV-any due")
	for _, id := range computeservice.ComputeProviderIds() {
		code, _ := computeservice.ComputeProviderCodeOf(id)
		kind, _ := computeservice.ComputeProviderKindOf(id)
		gate, _ := computeservice.ComputeProviderGateOf(id)
		row := rowsById[id]
		status, since, gateResult, anyResult, due := "listed", "no row", gate+" none", "none", "-"
		if kind == computeservice.ComputeProviderKindSearch {
			anyResult = "-"
		}
		if row != nil {
			status = row.Status.String()
			since = row.StatusTime.UTC().Format(time.RFC3339)
			gateResult = computeProviderGateText(gate, row.LastGate(gate))
			if kind != computeservice.ComputeProviderKindSearch {
				anyResult = computeProviderGateText("", row.LastGate(computeservice.ComputeProviderGateAny))
				if dueTime := row.RecheckDue(interval); !dueTime.IsZero() {
					due = dueTime.UTC().Format(time.RFC3339)
					if dueTime.Before(now) {
						due += " overdue"
					}
				}
			}
		}
		fmt.Fprintf(table, "%s\t%d\t%s\t%s\t%s\t%s\t%s\t%s\n", id, code, kind, status, since, gateResult, anyResult, due)
	}
	table.Flush()
}

// A gate's latest result as ls prints it, after name where it is given: pass or fail, its time and who recorded it.
func computeProviderGateText(name string, entry *computeservice.ComputeProviderHistoryEntry) string {
	text := "none"
	if entry != nil {
		text = fmt.Sprintf("%s %s by %s", entry.Result, entry.Time().Format(time.RFC3339), entry.By)
	}
	if name == "" {
		return text
	}
	return name + " " + text
}

// Parses gate's command line and records the result.
func runComputeProviderGateWithSettings(args []string, settings *computeProviderCommandSettings, out io.Writer) error {
	flags := flag.NewFlagSet("compute provider gate", flag.ContinueOnError)
	gate := flags.String("gate", "", "")
	result := flags.String("result", "", "")
	by := flags.String("by", "", "")
	evidence := flags.String("evidence", "", "")
	note := flags.String("note", "", "")
	positionals, err := parseSidecarctlArgs(flags, args)
	if err != nil {
		return err
	}
	env, id, err := computeProviderPositionals(positionals, 2)
	if err != nil {
		return err
	}
	switch {
	case *gate == "":
		return sidecarctlUsageErrorf("--gate names the gate: %s's own or %s", id, computeservice.ComputeProviderGateAny)
	case *result != computeservice.ComputeProviderGatePass && *result != computeservice.ComputeProviderGateFail:
		return sidecarctlUsageErrorf("--result is pass or fail: a gate is recorded only with what its checks found")
	case *by == "":
		return sidecarctlUsageErrorf("--by names the operator recording it")
	}
	gateResult := &computeservice.ComputeProviderGateResult{
		Gate:     *gate,
		Passed:   *result == computeservice.ComputeProviderGatePass,
		By:       *by,
		Evidence: *evidence,
		Note:     *note,
	}
	return withComputeProviderStore(env, settings, func(ctx context.Context, store computeProviderCommandStore) error {
		row, err := store.RecordGate(ctx, id, gateResult, settings.now())
		if err == nil && row == nil {
			err = errors.New("the store answered no row")
		}
		if err != nil {
			return fmt.Errorf("%s's %s result was not recorded in %s: %w", id, *gate, env, err)
		}
		writeComputeProviderChange(out, env, row, settings.providers)
		return nil
	})
}

// Parses suspend's or resume's command line and makes the change.
func runComputeProviderChangeWithSettings(args []string, kind string, settings *computeProviderCommandSettings, out io.Writer) error {
	flags := flag.NewFlagSet("compute provider "+kind, flag.ContinueOnError)
	by := flags.String("by", "", "")
	reason := flags.String("reason", "", "")
	note := flags.String("note", "", "")
	positionals, err := parseSidecarctlArgs(flags, args)
	if err != nil {
		return err
	}
	env, id, err := computeProviderPositionals(positionals, 2)
	if err != nil {
		return err
	}
	change := &computeservice.ComputeProviderStatusChange{By: *by}
	switch {
	case *by == "":
		return sidecarctlUsageErrorf("--by names the operator recording it")
	case kind == computeservice.ComputeProviderHistorySuspend && (*reason == "" || *note != ""):
		return sidecarctlUsageErrorf("--reason says why it is suspended, which its history keeps")
	case kind == computeservice.ComputeProviderHistorySuspend:
		change.Reason = *reason
	case *reason != "":
		return sidecarctlUsageErrorf("resume takes --note, not --reason")
	default:
		change.Reason = *note
	}
	return withComputeProviderStore(env, settings, func(ctx context.Context, store computeProviderCommandStore) error {
		var row *computeservice.ComputeProviderRow
		var err error
		if kind == computeservice.ComputeProviderHistorySuspend {
			row, err = store.Suspend(ctx, id, change, settings.now())
		} else {
			row, err = store.Resume(ctx, id, change, settings.now())
		}
		if err == nil && row == nil {
			err = errors.New("the store answered no row")
		}
		if err != nil {
			return fmt.Errorf("%s was not changed in %s: %w", id, env, err)
		}
		writeComputeProviderChange(out, env, row, settings.providers)
		return nil
	})
}

// What one change did, from the row it left: the entry it recorded, and what the status means for new work.
func writeComputeProviderChange(out io.Writer, env string, row *computeservice.ComputeProviderRow, providers *computeservice.ComputeProviderSettings) {
	var entry *computeservice.ComputeProviderHistoryEntry
	if 0 < len(row.History) {
		entry = row.History[len(row.History)-1]
	}
	if entry != nil && entry.Kind == computeservice.ComputeProviderHistoryGate {
		fmt.Fprintf(out, "recorded %s %s for %s in %s: %s, then %s\n", entry.Gate, entry.Result, row.Id, env, entry.From, entry.To)
	} else if entry != nil {
		fmt.Fprintf(out, "recorded %s of %s in %s: %s, then %s\n", entry.Kind, row.Id, env, entry.From, entry.To)
	}
	switch row.Status {
	case computeservice.ComputeProviderStatusGated:
		fmt.Fprintf(out, "%s serves wherever a release's settings enable it in %s; every broker and job reads this within %v\n", row.Id, env, providers.StatusMaxAge)
	case computeservice.ComputeProviderStatusListed:
		fmt.Fprintf(out, "%s stays listed, out of the catalog, until its own gate passes\n", row.Id)
	default:
		fmt.Fprintf(out, "%s takes no new work: every broker and job reads this within %v, and the catalog leaves out its models and shapes from its next build\n", row.Id, providers.StatusMaxAge)
	}
}
