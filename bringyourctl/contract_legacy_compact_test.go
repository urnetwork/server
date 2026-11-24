package main

import (
	"slices"
	"testing"
	"time"

	"github.com/docopt/docopt-go"
)

// The legacy compaction command parses to its bounds: none takes the
// defaults (100,000 rows, a 5 s count, a 3 s rewrite, every table); each
// option sets its bound; bounds past a few seconds of lock, another table and
// values that do not parse are refused. The options it shares, --table and
// --dry-run, still parse for the commands that had them, and --max-rows
// leaves --max-rows-per-second alone.
func TestContractLegacyCompactCommandParses(t *testing.T) {
	parse := func(args ...string) docopt.Opts {
		t.Helper()
		opts, err := docopt.ParseArgs(bringyourctlUsage, append([]string{"db", "contract-legacy-compact"}, args...), "synthetic")
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if command, _ := opts.Bool("contract-legacy-compact"); !command {
			t.Fatalf("%v: not the legacy compaction command", args)
		}
		return opts
	}
	settings, err := contractLegacyCompactOptions(parse())
	if err != nil || settings.MaxCompactRows != 100000 || settings.CountTimeout != 5*time.Second || settings.CompactTimeout != 3*time.Second ||
		settings.DryRun || len(settings.Tables) != 0 {
		t.Fatalf("defaults: %+v %v", settings, err)
	}
	settings, err = contractLegacyCompactOptions(parse("--dry-run", "--table=contract_close", "--max-rows=150000", "--count-timeout=20s", "--compact-timeout=5s"))
	if err != nil || !settings.DryRun || !slices.Equal(settings.Tables, []string{"contract_close"}) || settings.MaxCompactRows != 150000 ||
		settings.CountTimeout != 20*time.Second || settings.CompactTimeout != 5*time.Second {
		t.Fatalf("options: %+v %v", settings, err)
	}
	for _, args := range [][]string{
		{"--max-rows=0"},
		{"--max-rows=300000"},
		{"--max-rows=many"},
		{"--compact-timeout=30s"},
		{"--compact-timeout=soon"},
		{"--count-timeout=2m"},
		{"--table=network_client"},
	} {
		if _, err := contractLegacyCompactOptions(parse(args...)); err == nil {
			t.Fatalf("%v was accepted", args)
		}
	}

	swap, err := docopt.ParseArgs(bringyourctlUsage, []string{"model", "migrate", "table-swap", "--table=pending_task", "--dry-run"}, "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	if table, err := swap.String("--table"); err != nil || table != "pending_task" {
		t.Fatalf("table-swap's --table parses as %q, %v", table, err)
	}
	brake, err := docopt.ParseArgs(bringyourctlUsage, []string{"db", "close-evidence-retention", "--max-rows-per-second=5000"}, "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	if rate, err := brake.String("--max-rows-per-second"); err != nil || rate != "5000" {
		t.Fatalf("--max-rows-per-second parses as %q, %v", rate, err)
	}
	if rows, _ := brake.String("--max-rows"); rows != "" {
		t.Fatalf("--max-rows-per-second set --max-rows to %q", rows)
	}
}
