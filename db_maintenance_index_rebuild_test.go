package server

import (
	"strings"
	"testing"
	"time"
)

// Pure: an operator's rebuild takes a plain public index name and refuses
// anything that could name another relation, a quoted identifier, or the
// transient copy a concurrent rebuild leaves behind, which its cleanup drops.
func TestIndexRebuildNameProblem(t *testing.T) {
	for _, name := range []string{
		"transfer_contract_pkey",
		"transfer_contract_unresolved_source_pair_create_time",
		"network_client_network_id_active",
		"_leading_underscore",
		"transfer_escrow_pkey_ccnew0",
		strings.Repeat("a", 63),
	} {
		if problem := indexRebuildNameProblem(name); problem != "" {
			t.Errorf("%q refused: %s", name, problem)
		}
	}
	for _, name := range []string{
		"",
		"Transfer_contract_pkey",
		"transfer-contract",
		"public.transfer_contract_pkey",
		`transfer_contract_pkey"; DROP TABLE transfer_contract; --`,
		"transfer contract",
		"1index",
		strings.Repeat("a", 64),
		"transfer_contract_pkey_ccnew",
		"transfer_contract_pkey_ccnew1",
		"transfer_contract_pkey_ccold",
		"transfer_contract_pkey_ccold12",
	} {
		if indexRebuildNameProblem(name) == "" {
			t.Errorf("%q accepted", name)
		}
	}
}

// Pure: the statement names exactly the public index, quoted.
func TestIndexRebuildStatement(t *testing.T) {
	if got, want := indexRebuildStatement("transfer_contract_pkey"), `REINDEX INDEX CONCURRENTLY "public"."transfer_contract_pkey"`; got != want {
		t.Fatalf("statement %q, want %q", got, want)
	}
}

// Pure: the defaults wait out the hour-long transactions a priority rebuild
// waits out and give the largest indexes their two heap reads on one cpu;
// bounds a rebuild cannot honour are refused.
func TestIndexRebuildOptionsValidate(t *testing.T) {
	defaults := DefaultIndexRebuildOptions()
	if defaults.LockTimeout != DefaultDbMaintenancePriorityReindexLockTimeout || defaults.Timeout != 8*time.Hour || defaults.ParallelWorkers != 0 || defaults.DryRun {
		t.Fatalf("defaults %+v", defaults)
	}
	if err := defaults.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, options := range []IndexRebuildOptions{
		{LockTimeout: time.Millisecond, Timeout: time.Second},
		{LockTimeout: time.Hour, Timeout: 8 * time.Hour, ParallelWorkers: maxIndexRebuildParallelWorkers},
	} {
		if err := options.Validate(); err != nil {
			t.Errorf("%+v refused: %v", options, err)
		}
	}
	for _, options := range []IndexRebuildOptions{
		{LockTimeout: 0, Timeout: time.Hour},
		{LockTimeout: time.Microsecond, Timeout: time.Hour},
		{LockTimeout: time.Hour, Timeout: 0},
		{LockTimeout: time.Hour, Timeout: 999 * time.Millisecond},
		{LockTimeout: time.Hour, Timeout: time.Hour, ParallelWorkers: -1},
		{LockTimeout: time.Hour, Timeout: time.Hour, ParallelWorkers: maxIndexRebuildParallelWorkers + 1},
	} {
		if options.Validate() == nil {
			t.Errorf("%+v accepted", options)
		}
	}
}
