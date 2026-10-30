package main

import (
	"testing"

	"github.com/docopt/docopt-go"
)

// The command is a dry run unless --apply is given, and parses its group bound.
func TestPayoutsReconcileLegacyAttemptsUsageDefaultsToDryRun(t *testing.T) {
	parser := &docopt.Parser{HelpHandler: docopt.NoHelpHandler}
	for _, c := range []struct {
		args       []string
		apply      bool
		groupLimit string
	}{
		{args: []string{"payouts", "reconcile-legacy-attempts"}, apply: false, groupLimit: ""},
		{args: []string{"payouts", "reconcile-legacy-attempts", "--apply", "--group_limit=25"}, apply: true, groupLimit: "25"},
	} {
		opts, err := parser.ParseArgs(bringyourctlUsage, c.args, "test")
		if err != nil {
			t.Fatalf("%v: %v", c.args, err)
		}
		if reconcile, _ := opts.Bool("reconcile-legacy-attempts"); !reconcile {
			t.Fatalf("%v did not select the command", c.args)
		}
		apply, _ := opts.Bool("--apply")
		groupLimit, _ := opts.String("--group_limit")
		if apply != c.apply || groupLimit != c.groupLimit {
			t.Fatalf("%v: apply=%t group_limit=%q, want %t %q", c.args, apply, groupLimit, c.apply, c.groupLimit)
		}
	}
}
