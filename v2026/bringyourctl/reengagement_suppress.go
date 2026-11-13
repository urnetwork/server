// `reengagement suppress` (controller/reengagement_suppress.go; owner,
// 2026-11-13): stop every marketing mail to a network, or to each network of
// a list, for good. A dry run unless --apply, and --apply writes only the
// networks and reason of the dry run whose digest is --expect.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/docopt/docopt-go"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/controller"
)

func reengagementSuppressCommand(opts docopt.Opts) int {
	networkId, _ := opts.String("--network_id")
	networkIdsFile, _ := opts.String("--network_ids_file")
	reason, _ := opts.String("--reason")
	apply, _ := opts.Bool("--apply")
	expect, _ := opts.String("--expect")
	// The dry run writes nothing: its process is read only, so PostgreSQL
	// refuses any write on its path.
	if !apply {
		server.RequirePgReadOnly()
	}
	ctx := context.Background()
	settings := controller.DefaultMarketingSuppressionSettings()
	return runReengagementSuppress(
		os.Stdout,
		os.Stderr,
		reengagementSuppressArgs{
			networkId:      networkId,
			networkIdsFile: networkIdsFile,
			reason:         reason,
			apply:          apply,
			expect:         expect,
		},
		settings,
		os.ReadFile,
		func(networkIds []server.Id) (*controller.MarketingSuppressionPlan, error) {
			return controller.PlanNetworksMarketingSuppression(ctx, networkIds, reason, settings)
		},
		func(networkIds []server.Id) (*controller.MarketingSuppressionResult, error) {
			return controller.SuppressNetworksMarketing(ctx, networkIds, reason, expect, settings)
		},
	)
}

// The command line of one run.
type reengagementSuppressArgs struct {
	// exactly one of networkId and networkIdsFile
	networkId      string
	networkIdsFile string
	reason         string
	apply          bool
	expect         string
}

// The flags that name the networks and the reason, as the dry run prints them
// in the command that applies it. The dry run has parsed the network id and
// checked the reason by then.
func (self *reengagementSuppressArgs) flags() string {
	if self.networkIdsFile != "" {
		return fmt.Sprintf("--network_ids_file=%s --reason=%s", shellWordQuote(self.networkIdsFile), self.reason)
	}
	return fmt.Sprintf("--network_id=%s --reason=%s", strings.TrimSpace(self.networkId), self.reason)
}

// Reads the networks, runs the dry run or the apply, prints it and returns the
// exit code. A refusal or an apply that stopped prints what it can, then its
// error, and exits non-zero. A dry run ends with the command that applies
// exactly it.
func runReengagementSuppress(
	stdout io.Writer,
	stderr io.Writer,
	args reengagementSuppressArgs,
	settings *controller.MarketingSuppressionSettings,
	readFile func(string) ([]byte, error),
	plan func([]server.Id) (*controller.MarketingSuppressionPlan, error),
	suppress func([]server.Id) (*controller.MarketingSuppressionResult, error),
) int {
	networkIds, err := readReengagementSuppressNetworkIds(args, settings, readFile)
	if err != nil {
		fmt.Fprintf(stderr, "reengagement suppress: %s\n", err)
		return 1
	}

	if !args.apply {
		p, err := plan(networkIds)
		if err != nil {
			fmt.Fprintf(stderr, "%s\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "reengagement suppress DRY RUN: nothing was written\n")
		fmt.Fprintf(stdout, "reason: %s (no connection lifts it)\n", p.Reason)
		fmt.Fprintf(stdout, "requested: %d networks (%d repeated ids dropped)\n", len(p.NetworkIds), p.Duplicates)
		writeReengagementSuppressCounts(stdout, p.Counts)
		fmt.Fprintf(stdout, "digest: %s\n", p.Digest)
		fmt.Fprintf(stdout, "to apply exactly this: bringyourctl reengagement suppress %s --apply --expect=%s\n", args.flags(), p.Digest)
		return 0
	}

	result, err := suppress(networkIds)
	if result != nil {
		fmt.Fprintf(stdout, "reengagement suppress APPLY: suppressed %d networks in %d batches, each with the Brevo list sync of its admin queued\n", result.Suppressed, result.Batches)
		writeReengagementSuppressCounts(stdout, result.Counts)
		fmt.Fprintf(stdout, "[audit]reengagement suppress digest=%s reason=%s requested=%d suppressed=%d already=%d missing=%d batches=%d\n",
			args.expect, result.Reason, result.Requested, result.Suppressed, result.Counts.AlreadySuppressed, result.Counts.Missing, result.Batches)
	} else if err != nil {
		fmt.Fprintf(stdout, "reengagement suppress APPLY REFUSED: nothing was written\n")
	}
	if err != nil {
		fmt.Fprintf(stderr, "%s\n", err)
		return 1
	}
	return 0
}

func writeReengagementSuppressCounts(out io.Writer, counts controller.MarketingSuppressionCounts) {
	fmt.Fprintf(out, "found: %d\n", counts.Found)
	fmt.Fprintf(out, "missing: %d\n", counts.Missing)
	fmt.Fprintf(out, "already suppressed for good: %d\n", counts.AlreadySuppressed)
	fmt.Fprintf(out, "to suppress: %d (%d of them replace a suppression a connection would lift)\n", counts.ToSuppress, counts.ReplacesLiftable)
}

// The networks the command line names: --network_id, or the lines of
// --network_ids_file, one network id each. Blank lines and lines that start
// with # are skipped, and space around an id is trimmed. A line that is not a
// network id is refused with its line number, never its text (a list pasted
// from elsewhere can hold an address), and so is a file larger than the
// settings allow.
func readReengagementSuppressNetworkIds(
	args reengagementSuppressArgs,
	settings *controller.MarketingSuppressionSettings,
	readFile func(string) ([]byte, error),
) ([]server.Id, error) {
	if args.networkIdsFile == "" {
		networkId, err := server.ParseId(strings.TrimSpace(args.networkId))
		if err != nil {
			return nil, errors.New("--network_id is not a network id")
		}
		return []server.Id{networkId}, nil
	}
	data, err := readFile(args.networkIdsFile)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %s", args.networkIdsFile, err)
	}
	if settings.MaxNetworkIdsBytes() < len(data) {
		return nil, fmt.Errorf("%s is larger than %d bytes; split the list", args.networkIdsFile, settings.MaxNetworkIdsBytes())
	}
	networkIds := []server.Id{}
	for i, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		networkId, err := server.ParseId(line)
		if err != nil {
			return nil, fmt.Errorf("%s line %d is not a network id", args.networkIdsFile, i+1)
		}
		networkIds = append(networkIds, networkId)
	}
	if len(networkIds) == 0 {
		return nil, fmt.Errorf("%s names no network id", args.networkIdsFile)
	}
	return networkIds, nil
}

// One POSIX shell word.
func shellWordQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}
