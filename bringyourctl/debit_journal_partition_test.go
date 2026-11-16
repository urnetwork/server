package main

import (
	"testing"
	"time"

	"github.com/docopt/docopt-go"
)

// Each step of the transfer debit journal partition cutover parses to its
// option.
func TestDebitJournalPartitionCommandParses(t *testing.T) {
	for _, test := range []struct {
		args       []string
		cutoverDay time.Time
		dryRun     bool
		finalize   bool
		rollback   bool
		abort      bool
	}{
		{args: []string{"--cutover-day=2026-11-21"}, cutoverDay: time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC)},
		{args: []string{"--cutover-day=2026-11-21", "--dry-run"}, cutoverDay: time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC), dryRun: true},
		{args: []string{"--finalize"}, finalize: true},
		{args: []string{"--finalize", "--dry-run"}, finalize: true, dryRun: true},
		{args: []string{"--rollback"}, rollback: true},
		{args: []string{"--rollback", "--dry-run"}, rollback: true, dryRun: true},
		{args: []string{"--abort"}, abort: true},
		{args: []string{"--abort", "--dry-run"}, abort: true, dryRun: true},
	} {
		opts, err := docopt.ParseArgs(bringyourctlUsage, append([]string{"model", "migrate", "debit-journal-partition"}, test.args...), "synthetic")
		if err != nil {
			t.Fatal(test.args, err)
		}
		if partition, _ := opts.Bool("debit-journal-partition"); !partition {
			t.Fatal(test.args, "did not parse as the debit journal partition command")
		}
		options, err := debitJournalPartitionMigrateOptions(opts)
		if err != nil {
			t.Fatal(test.args, err)
		}
		if !options.CutoverDay.Equal(test.cutoverDay) || options.DryRun != test.dryRun || options.Finalize != test.finalize ||
			options.Rollback != test.rollback || options.Abort != test.abort {
			t.Fatal(test.args, options)
		}
	}
	opts, err := docopt.ParseArgs(bringyourctlUsage, []string{"model", "migrate", "debit-journal-partition", "--cutover-day=11/21/2026"}, "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := debitJournalPartitionMigrateOptions(opts); err == nil {
		t.Fatal("a malformed cutover day parsed")
	}
}
