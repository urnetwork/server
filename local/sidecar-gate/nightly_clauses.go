package main

// Nightly T3 runs the same named clause tests as the milestone gates. Their packages supply the real
// dependencies, including API and model fixtures that a generic sidecart3 Net cannot supply.

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/urnetwork/sidecar/sidecartest/fault"
)

// Each clause must actually pass, including the Go process. In list mode only its declaration is checked.
func runNightlyClauses(ctx context.Context, options *gateOptions, clauses []*fault.ClauseRegistration, out io.Writer, progress io.Writer) bool {
	timeout := options.settings.TestTimeout
	if value := gateLookup(options.environ, "TEST_TIMEOUT"); !options.list && value != "" {
		var err error
		timeout, err = time.ParseDuration(value)
		if err != nil || timeout <= 0 {
			fmt.Fprintf(progress, "nightly-clauses: invalid TEST_TIMEOUT %q\n", value)
			return false
		}
	}
	report := &gateReport{list: options.list}
	required := gatePass
	if options.list {
		required = gateListed
	}
	count := 0
	for _, clause := range clauses {
		for _, test := range fault.GateClauseTests(clause) {
			if test.Repo != "server" {
				continue
			}
			count++
			entry := gateTest{Repo: test.Repo, Package: test.Package, Tags: test.Tags, Stores: test.Stores, Names: test.Names, Timeout: gateTimeout(timeout)}
			entry.requireStoreClock = fault.DefaultCatalog().Case(clause.Row, clause.Case).StoreClock
			if !options.list {
				fmt.Fprintf(progress, "nightly-clauses: running %s in %s\n", test.Names[0], test.Package)
			}
			outcome, details := runGateTest(ctx, options, report, entry)
			fmt.Fprintf(out, "%s: %s\n", clause.Id(), outcome)
			for _, detail := range details {
				fmt.Fprintln(out, detail)
			}
			if outcome != required {
				return false
			}
		}
	}
	if count == 0 {
		fmt.Fprintln(progress, "nightly-clauses: no registered T3 clause")
		return false
	}
	return true
}
