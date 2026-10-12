package main

import (
	"testing"

	"github.com/docopt/docopt-go"
)

// The correction is an anchored dry run unless --apply or --move_all_sweeps is
// given, and parses its bounds.
func TestPayoutsCorrectLegacyAttemptsUsageDefaultsToDryRun(t *testing.T) {
	parser := &docopt.Parser{HelpHandler: docopt.NoHelpHandler}
	for _, c := range []struct {
		args            []string
		apply           bool
		moveAllSweeps   bool
		countSweeps     bool
		groupLimit      string
		sweepBatchSize  string
		sweepBatchLimit string
	}{
		{args: []string{"payouts", "correct-legacy-attempts"}},
		{args: []string{"payouts", "correct-legacy-attempts", "--count_sweeps"}, countSweeps: true},
		{args: []string{"payouts", "correct-legacy-attempts", "--apply", "--group_limit=25"}, apply: true, groupLimit: "25"},
		{args: []string{"payouts", "correct-legacy-attempts", "--apply", "--move_all_sweeps", "--sweep_batch_size=5000", "--sweep_batch_limit=100"},
			apply: true, moveAllSweeps: true, sweepBatchSize: "5000", sweepBatchLimit: "100"},
	} {
		opts, err := parser.ParseArgs(bringyourctlUsage, c.args, "test")
		if err != nil {
			t.Fatalf("%v: %v", c.args, err)
		}
		if correct, _ := opts.Bool("correct-legacy-attempts"); !correct {
			t.Fatalf("%v did not select the command", c.args)
		}
		if reconcile, _ := opts.Bool("reconcile-legacy-attempts"); reconcile {
			t.Fatalf("%v selected the reconciliation", c.args)
		}
		apply, _ := opts.Bool("--apply")
		moveAllSweeps, _ := opts.Bool("--move_all_sweeps")
		countSweeps, _ := opts.Bool("--count_sweeps")
		groupLimit, _ := opts.String("--group_limit")
		sweepBatchSize, _ := opts.String("--sweep_batch_size")
		sweepBatchLimit, _ := opts.String("--sweep_batch_limit")
		if apply != c.apply || moveAllSweeps != c.moveAllSweeps || countSweeps != c.countSweeps || groupLimit != c.groupLimit ||
			sweepBatchSize != c.sweepBatchSize || sweepBatchLimit != c.sweepBatchLimit {
			t.Fatalf("%v: apply=%t move_all_sweeps=%t count_sweeps=%t group_limit=%q sweep_batch_size=%q sweep_batch_limit=%q",
				c.args, apply, moveAllSweeps, countSweeps, groupLimit, sweepBatchSize, sweepBatchLimit)
		}
	}
}
