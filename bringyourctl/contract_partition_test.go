package main

import (
	"testing"
	"time"

	"github.com/docopt/docopt-go"
)

// Each step of the contract partition cutover parses to its option.
func TestContractPartitionCommandParses(t *testing.T) {
	for _, test := range []struct {
		args        []string
		cutoverWeek time.Time
		dryRun      bool
		finalize    bool
		rollback    bool
		abort       bool
	}{
		{args: []string{"--cutover-week=2026-11-02"}, cutoverWeek: time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)},
		{args: []string{"--cutover-week=2026-11-02", "--dry-run"}, cutoverWeek: time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC), dryRun: true},
		{args: []string{"--finalize"}, finalize: true},
		{args: []string{"--finalize", "--dry-run"}, finalize: true, dryRun: true},
		{args: []string{"--rollback"}, rollback: true},
		{args: []string{"--abort"}, abort: true},
	} {
		opts, err := docopt.ParseArgs(bringyourctlUsage, append([]string{"model", "migrate", "contract-partition"}, test.args...), "synthetic")
		if err != nil {
			t.Fatal(test.args, err)
		}
		if partition, _ := opts.Bool("contract-partition"); !partition {
			t.Fatal(test.args, "did not parse as the contract partition command")
		}
		options, err := contractPartitionMigrateOptions(opts)
		if err != nil {
			t.Fatal(test.args, err)
		}
		if !options.CutoverWeek.Equal(test.cutoverWeek) || options.DryRun != test.dryRun || options.Finalize != test.finalize ||
			options.Rollback != test.rollback || options.Abort != test.abort {
			t.Fatal(test.args, options)
		}
	}
	opts, err := docopt.ParseArgs(bringyourctlUsage, []string{"model", "migrate", "contract-partition", "--cutover-week=11/02/2026"}, "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := contractPartitionMigrateOptions(opts); err == nil {
		t.Fatal("a malformed cutover week parsed")
	}
}

// Each step of the close evidence partition cutover parses to its option, on
// its own command.
func TestCloseEvidencePartitionCommandParses(t *testing.T) {
	for _, test := range []struct {
		args        []string
		cutoverWeek time.Time
		dryRun      bool
		finalize    bool
		rollback    bool
		abort       bool
	}{
		{args: []string{"--cutover-week=2026-11-02"}, cutoverWeek: time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)},
		{args: []string{"--cutover-week=2026-11-02", "--dry-run"}, cutoverWeek: time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC), dryRun: true},
		{args: []string{"--finalize"}, finalize: true},
		{args: []string{"--finalize", "--dry-run"}, finalize: true, dryRun: true},
		{args: []string{"--rollback"}, rollback: true},
		{args: []string{"--rollback", "--dry-run"}, rollback: true, dryRun: true},
		{args: []string{"--abort"}, abort: true},
	} {
		opts, err := docopt.ParseArgs(bringyourctlUsage, append([]string{"model", "migrate", "close-evidence-partition"}, test.args...), "synthetic")
		if err != nil {
			t.Fatal(test.args, err)
		}
		if partition, _ := opts.Bool("close-evidence-partition"); !partition {
			t.Fatal(test.args, "did not parse as the close evidence partition command")
		}
		if partition, _ := opts.Bool("contract-partition"); partition {
			t.Fatal(test.args, "parsed as the contract partition command")
		}
		options, err := closeEvidencePartitionMigrateOptions(opts)
		if err != nil {
			t.Fatal(test.args, err)
		}
		if !options.CutoverWeek.Equal(test.cutoverWeek) || options.DryRun != test.dryRun || options.Finalize != test.finalize ||
			options.Rollback != test.rollback || options.Abort != test.abort || options.Settings != nil {
			t.Fatal(test.args, options)
		}
	}
	opts, err := docopt.ParseArgs(bringyourctlUsage, []string{"model", "migrate", "close-evidence-partition", "--cutover-week=11/02/2026"}, "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := closeEvidencePartitionMigrateOptions(opts); err == nil {
		t.Fatal("a malformed cutover week parsed")
	}
}
