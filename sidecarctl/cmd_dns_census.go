// `sidecarctl dns census` runs §12.5's census (DNS-M0-01): before labels and aliases exist, how many of the env's
// network names the label rules would refuse. It is model.CensusDnsNetworkNames, the design's one SELECT, behind
// server.RequirePgReadOnly(), so every session the process opens can only read and PostgreSQL refuses anything
// else. A person runs it once against a read replica; its production run is outside this build.
//
// It prints four counts and never a name, since network names are user data. A name may be in several counts, and
// one in none is a name the rules allow unless a brand rule refuses it, which no SQL regex holds.
//
// It reaches the env its WARP_ENV names, as bringyourctl's commands do, and refuses with no WARP_ENV.

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The command's registration. It is the first command of the dns group.
func init() {
	registerSidecarctlCommand(&sidecarctlCommand{
		words:   "dns census",
		usage:   "",
		summary: "Count, read-only, the network names of the env WARP_ENV names that §12.5's label rules would refuse: four counts and never a name.",
		run:     runDnsCensus,
	})
}

// How one run reaches its env. Tests replace the census.
type dnsCensusSettings struct {
	// Bounds the run: one SELECT over every network row. Non-positive expires at once, so nothing is read.
	timeout time.Duration
	// Makes the process read-only and counts the env's names, answering the env it counted in.
	census func(ctx context.Context) (string, *model.DnsNetworkNameCensus, error)
}

// The settings of a run. A caller changes its own copy.
func defaultDnsCensusSettings() *dnsCensusSettings {
	return &dnsCensusSettings{
		// a sequential scan of the network table with four filters, minutes at most on a replica
		timeout: 10 * time.Minute,
		census:  censusServerDnsNetworkNames,
	}
}

// The census as server runs it. The process turns read-only before its first session opens, and stays so: it exits
// when the command returns.
func censusServerDnsNetworkNames(ctx context.Context) (string, *model.DnsNetworkNameCensus, error) {
	env, err := sidecarctlProcessEnv()
	if err != nil {
		return "", nil, err
	}
	var census *model.DnsNetworkNameCensus
	err = sidecarctlServerCall(func() error {
		server.RequirePgReadOnly()
		census = model.CensusDnsNetworkNames(ctx)
		return nil
	})
	if err != nil {
		return "", nil, err
	}
	return env, census, nil
}

// `sidecarctl dns census`, against the env as server reaches it.
func runDnsCensus(args []string, out io.Writer) error {
	return runDnsCensusWithSettings(args, defaultDnsCensusSettings(), out)
}

// Parses the command line, runs the census and prints its counts to out. A failure prints no count.
func runDnsCensusWithSettings(args []string, settings *dnsCensusSettings, out io.Writer) error {
	flags := flag.NewFlagSet("dns census", flag.ContinueOnError)
	positionals, err := parseSidecarctlArgs(flags, args)
	if err != nil {
		return err
	}
	if len(positionals) != 0 {
		return sidecarctlUsageErrorf("takes no arguments, got %q", positionals)
	}
	ctx, cancel := context.WithTimeout(context.Background(), settings.timeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("nothing was counted: the run's %v bound passed: %w", settings.timeout, err)
	}
	env, census, err := settings.census(ctx)
	if err == nil && census == nil {
		err = errors.New("the census answered no counts")
	}
	if err != nil {
		return fmt.Errorf("the census did not run: %w", err)
	}
	fmt.Fprintf(out, "network names in %s that §12.5's label rules would refuse, counted read-only; a name may be in several counts\n", env)
	fmt.Fprintf(out, "pre_rule %d\n", census.PreRule)
	fmt.Fprintf(out, "edge_hyphen %d\n", census.EdgeHyphen)
	fmt.Fprintf(out, "dash34 %d\n", census.Dash34)
	fmt.Fprintf(out, "reserved %d\n", census.Reserved)
	return nil
}
