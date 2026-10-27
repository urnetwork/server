package main

import (
	"testing"

	"github.com/docopt/docopt-go"
)

// Each step of the egress health history cutover parses to its option, the
// copy window defaults to two days, and a malformed window is refused.
func TestProviderEgressHealthPartitionCommandParses(t *testing.T) {
	for _, test := range []struct {
		args     []string
		copyDays int
		dryRun   bool
		swap     bool
		verify   bool
		rollback bool
		abort    bool
		dropOld  bool
	}{
		{args: []string{}, copyDays: 2},
		{args: []string{"--dry-run"}, copyDays: 2, dryRun: true},
		{args: []string{"--copy-days=3"}, copyDays: 3},
		{args: []string{"--swap"}, copyDays: 2, swap: true},
		{args: []string{"--swap", "--dry-run"}, copyDays: 2, swap: true, dryRun: true},
		{args: []string{"--verify"}, copyDays: 2, verify: true},
		{args: []string{"--rollback"}, copyDays: 2, rollback: true},
		{args: []string{"--abort"}, copyDays: 2, abort: true},
		{args: []string{"--drop-old"}, copyDays: 2, dropOld: true},
	} {
		opts, err := docopt.ParseArgs(bringyourctlUsage, append([]string{"model", "migrate", "provider-egress-health-partition"}, test.args...), "synthetic")
		if err != nil {
			t.Fatal(test.args, err)
		}
		if partition, _ := opts.Bool("provider-egress-health-partition"); !partition {
			t.Fatal(test.args, "did not parse as the egress health partition command")
		}
		options, err := providerEgressHealthPartitionOptions(opts)
		if err != nil {
			t.Fatal(test.args, err)
		}
		if options.CopyDays != test.copyDays || options.DryRun != test.dryRun || options.Swap != test.swap || options.Verify != test.verify ||
			options.Rollback != test.rollback || options.Abort != test.abort || options.DropOld != test.dropOld {
			t.Fatal(test.args, options)
		}
	}
	opts, err := docopt.ParseArgs(bringyourctlUsage, []string{"model", "migrate", "provider-egress-health-partition", "--copy-days=two"}, "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := providerEgressHealthPartitionOptions(opts); err == nil {
		t.Fatal("a malformed copy window parsed")
	}
	// the contract cutover's steps parse as before
	opts, err = docopt.ParseArgs(bringyourctlUsage, []string{"model", "migrate", "contract-partition", "--rollback"}, "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	if rollback, _ := opts.Bool("--rollback"); !rollback {
		t.Fatal("the contract rollback did not parse")
	}
}
