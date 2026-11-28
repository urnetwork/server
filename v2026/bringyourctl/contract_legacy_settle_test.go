package main

import (
	"testing"
	"time"

	"github.com/docopt/docopt-go"
)

// The legacy settle command parses to its settings: none takes the defaults
// (a dry run, 48 h of close age, 50 contracts a second, live grants held);
// each option sets its setting, --apply needs --expect, and values that do not
// parse or do not bound the run are refused. The flags it prints for the
// apply line plan with the same settings.
func TestContractLegacySettleCommandParses(t *testing.T) {
	parse := func(args ...string) (docopt.Opts, error) {
		return docopt.ParseArgs(bringyourctlUsage, append([]string{"db", "contract-legacy-settle"}, args...), "synthetic")
	}
	opts, err := parse()
	if err != nil {
		t.Fatal(err)
	}
	if command, _ := opts.Bool("contract-legacy-settle"); !command {
		t.Fatal("not the legacy settle command")
	}
	settings, err := contractLegacySettleOptions(opts)
	if apply, _ := opts.Bool("--apply"); err != nil || apply || settings.MinCloseAge != 48*time.Hour || settings.MaxContractsPerSecond != 50 ||
		settings.ChargeLiveGrants || settings.MaxContractId != nil || settings.MaxContracts != 0 {
		t.Fatalf("defaults: %+v %v", settings, err)
	}
	if flags := contractLegacySettleFlags(settings); flags != "" {
		t.Fatalf("default flags %q", flags)
	}
	opts, err = parse("--min-close-age=72h", "--max-contract-id=01a21000-8800-0000-0000-000000000000", "--max-contracts-per-second=20",
		"--max-contracts=1000", "--charge-live-grants", "--apply", "--expect=0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	settings, err = contractLegacySettleOptions(opts)
	expect, _ := opts.String("--expect")
	if apply, _ := opts.Bool("--apply"); err != nil || !apply || expect != "0123456789abcdef" || settings.MinCloseAge != 72*time.Hour ||
		settings.MaxContractId == nil || settings.MaxContractId.String() != "01a21000-8800-0000-0000-000000000000" ||
		settings.MaxContractsPerSecond != 20 || settings.MaxContracts != 1000 || !settings.ChargeLiveGrants {
		t.Fatalf("options: %+v %v", settings, err)
	}
	if flags := contractLegacySettleFlags(settings); flags != " --min-close-age=72h0m0s --max-contract-id=01a21000-8800-0000-0000-000000000000 --charge-live-grants --max-contracts-per-second=20 --max-contracts=1000" {
		t.Fatalf("flags %q", flags)
	}
	strict := &docopt.Parser{HelpHandler: docopt.NoHelpHandler}
	if _, err := strict.ParseArgs(bringyourctlUsage, []string{"db", "contract-legacy-settle", "--apply"}, "synthetic"); err == nil {
		t.Fatal("--apply parsed without --expect")
	}
	for _, args := range [][]string{
		{"--min-close-age=-1h"},
		{"--min-close-age=soon"},
		{"--max-contract-id=contract"},
		{"--max-contracts-per-second=0"},
		{"--max-contracts-per-second=fast"},
		{"--max-contracts=many"},
	} {
		opts, err := parse(args...)
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if _, err := contractLegacySettleOptions(opts); err == nil {
			t.Fatalf("%v was accepted", args)
		}
	}
}
