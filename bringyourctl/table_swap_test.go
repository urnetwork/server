package main

import (
	"testing"

	"github.com/docopt/docopt-go"
)

// Each step of the table swap parses to its option; the table is required and
// rollback and drop old exclude each other.
func TestTableSwapCommandParses(t *testing.T) {
	for _, test := range []struct {
		args     []string
		dryRun   bool
		rollback bool
		dropOld  bool
	}{
		{args: []string{"--table=pending_task"}},
		{args: []string{"--table=pending_task", "--dry-run"}, dryRun: true},
		{args: []string{"--table=pending_task", "--rollback"}, rollback: true},
		{args: []string{"--table=pending_task", "--rollback", "--dry-run"}, rollback: true, dryRun: true},
		{args: []string{"--table=pending_task", "--drop-old"}, dropOld: true},
		{args: []string{"--table=pending_task", "--drop-old", "--dry-run"}, dropOld: true, dryRun: true},
	} {
		opts, err := docopt.ParseArgs(bringyourctlUsage, append([]string{"model", "migrate", "table-swap"}, test.args...), "synthetic")
		if err != nil {
			t.Fatal(test.args, err)
		}
		if tableSwap, _ := opts.Bool("table-swap"); !tableSwap {
			t.Fatal(test.args, "did not parse as the table swap command")
		}
		options, err := tableSwapOptions(opts)
		if err != nil {
			t.Fatal(test.args, err)
		}
		if options.Table != "pending_task" || options.DryRun != test.dryRun || options.Rollback != test.rollback || options.DropOld != test.dropOld {
			t.Fatal(test.args, options)
		}
	}
	opts, err := docopt.ParseArgs(bringyourctlUsage, []string{"model", "migrate", "table-swap", "--table=pending_task", "--rollback", "--drop-old"}, "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tableSwapOptions(opts); err == nil {
		t.Fatal("rollback and drop old together parsed")
	}
	parser := &docopt.Parser{HelpHandler: docopt.NoHelpHandler}
	if opts, err := parser.ParseArgs(bringyourctlUsage, []string{"model", "migrate", "table-swap"}, "synthetic"); err == nil {
		if tableSwap, _ := opts.Bool("table-swap"); tableSwap {
			t.Fatal("the table swap parsed without its table")
		}
	}
}
