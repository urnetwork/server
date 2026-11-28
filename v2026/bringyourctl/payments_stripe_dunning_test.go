package main

import (
	"testing"

	"github.com/docopt/docopt-go"
)

// The failed-payment follow-up's backfill (controller.StripeDunningReport) is a
// dry run unless --send is given, and takes no other option.
func TestPaymentsStripeDunningCommandLine(t *testing.T) {
	parse := func(args []string) (docopt.Opts, error) {
		return (&docopt.Parser{HelpHandler: docopt.NoHelpHandler}).ParseArgs(bringyourctlUsage, args, "test")
	}
	for _, c := range []struct {
		args []string
		send bool
	}{
		{args: []string{"payments", "stripe-dunning"}, send: false},
		{args: []string{"payments", "stripe-dunning", "--send"}, send: true},
	} {
		opts, err := parse(c.args)
		if err != nil {
			t.Fatal(c.args, err)
		}
		payments, _ := opts.Bool("payments")
		stripeDunning, _ := opts.Bool("stripe-dunning")
		reconcile, _ := opts.Bool("reconcile")
		send, _ := opts.Bool("--send")
		if !payments || !stripeDunning || reconcile || send != c.send {
			t.Fatal("command line", c.args, opts)
		}
	}
	for _, args := range [][]string{
		{"payments", "stripe-dunning", "--dry-run"},
		{"payments", "stripe-dunning", "--store=stripe"},
	} {
		if _, err := parse(args); err == nil {
			t.Fatal("parsed", args)
		}
	}
}
