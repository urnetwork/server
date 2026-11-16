// The reaper's batch statements run without JIT, under its plan and time bounds.
package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// Each batch that reads contracts runs with JIT off, as well as its statement
// bound and bitmap-only plans, and still removes the eligible contract.
func TestContractHorizonBatchRunsWithoutJit(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		contract := newHorizonTestContract(server.NowUtc().Add(-100*24*time.Hour), "canceled")
		insertHorizonTestContracts(t, ctx, f, contract)

		var batches []map[string]string
		contractHorizonBatchObserved = func(ctx context.Context, tx server.PgTx) {
			settings := map[string]string{}
			for _, name := range []string{"jit", "statement_timeout", "enable_seqscan", "enable_indexscan"} {
				var value string
				server.Raise(tx.QueryRow(ctx, `SELECT current_setting($1)`, name).Scan(&value))
				settings[name] = value
			}
			batches = append(batches, settings)
		}
		t.Cleanup(func() { contractHorizonBatchObserved = nil })

		report, _, err := RemoveContractsPastHorizon(ctx, server.NowUtc(), ContractHorizonCursor{}, horizonTestSettings())
		if err != nil {
			t.Fatal(err)
		}
		if len(batches) == 0 {
			t.Fatalf("no batch read a contract: %+v", report)
		}
		want := map[string]string{
			"jit":               "off",
			"statement_timeout": "30s",
			"enable_seqscan":    "off",
			"enable_indexscan":  "off",
		}
		for index, settings := range batches {
			for name, value := range want {
				if settings[name] != value {
					t.Errorf("batch %d %s = %q, want %q", index, name, settings[name], value)
				}
			}
		}
		if rows, _ := countHorizonTestRows(t, ctx, contract.contractId); rows[0] != 0 {
			t.Errorf("the eligible contract was kept: rows %v, report %+v", rows, report)
		}
	})
}
