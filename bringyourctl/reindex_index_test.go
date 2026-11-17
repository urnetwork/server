package main

import (
	"testing"
	"time"

	"github.com/docopt/docopt-go"

	"github.com/urnetwork/server"
)

// The index rebuild command parses to the index and the rebuild's bounds:
// the defaults without options, Go durations and a worker count with them;
// a duration that does not parse and bounds the rebuild refuses are refused
// before anything is read.
func TestReindexIndexCommandParses(t *testing.T) {
	defaults := server.DefaultIndexRebuildOptions()
	for _, test := range []struct {
		args        []string
		dryRun      bool
		lockTimeout time.Duration
		timeout     time.Duration
		parallel    int
	}{
		{args: []string{"transfer_contract_pkey"}, lockTimeout: defaults.LockTimeout, timeout: defaults.Timeout},
		{args: []string{"transfer_contract_pkey", "--dry-run"}, dryRun: true, lockTimeout: defaults.LockTimeout, timeout: defaults.Timeout},
		{args: []string{"transfer_escrow_unsettled_balance_contract", "--lock-timeout=30m", "--timeout=4h", "--parallel=4"}, lockTimeout: 30 * time.Minute, timeout: 4 * time.Hour, parallel: 4},
	} {
		opts, err := docopt.ParseArgs(bringyourctlUsage, append([]string{"db", "reindex-index"}, test.args...), "synthetic")
		if err != nil {
			t.Fatalf("%v: %v", test.args, err)
		}
		if command, _ := opts.Bool("reindex-index"); !command {
			t.Fatalf("%v: not the index rebuild command", test.args)
		}
		indexName, options, err := reindexIndexOptions(opts)
		if err != nil {
			t.Fatalf("%v: %v", test.args, err)
		}
		if indexName != test.args[0] || options.DryRun != test.dryRun || options.LockTimeout != test.lockTimeout || options.Timeout != test.timeout || options.ParallelWorkers != test.parallel {
			t.Fatalf("%v: %s %+v", test.args, indexName, options)
		}
	}
	for _, args := range [][]string{
		{"transfer_contract_pkey", "--lock-timeout=soon"},
		{"transfer_contract_pkey", "--lock-timeout=0s"},
		{"transfer_contract_pkey", "--timeout=8"},
		{"transfer_contract_pkey", "--timeout=500ms"},
		{"transfer_contract_pkey", "--parallel=many"},
		{"transfer_contract_pkey", "--parallel=9"},
		{"transfer_contract_pkey", "--parallel=-1"},
	} {
		opts, err := docopt.ParseArgs(bringyourctlUsage, append([]string{"db", "reindex-index"}, args...), "synthetic")
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if _, _, err := reindexIndexOptions(opts); err == nil {
			t.Fatalf("%v was accepted", args)
		}
	}
}
