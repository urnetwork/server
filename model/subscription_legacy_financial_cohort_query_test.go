// Cold real PostgreSQL replies prove typed UUID encoding and the removed
// prepare exchange separately from the full financial conservation oracle.
package model

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

func TestLegacyFinancialCohortColdQueryUsesOneTypedExchange(t *testing.T) {
	env := &server.TestEnv{ApplyDbMigrations: false, RerunCount: 0}
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		protocol, cleanup := legacyFinancialRunProtocolBind(t, ctx)
		defer cleanup()
		ids := []server.Id{server.NewId(), server.NewId(), server.NewId()}
		slices.SortFunc(ids, server.Id.Cmp)
		var found []server.Id
		decodedValues := true
		server.Tx(ctx, func(tx server.PgTx) {
			// The existing BEGIN/checkout has finished. This exact SQL has
			// never been prepared on the newly bound ordinary connection.
			protocol.enabled(true)
			rows, err := queryLegacyFinancialCohort(ctx, tx, `SELECT id,NULL::uuid,id=id
 FROM unnest($1::uuid[]) AS requested(id) ORDER BY id LIMIT $2`, ids, len(ids))
			server.WithPgResult(rows, err, func() {
				for rows.Next() {
					var id server.Id
					var missing *server.Id
					var same bool
					server.Raise(rows.Scan(&id, &missing, &same))
					decodedValues = decodedValues && missing == nil && same
					found = append(found, id)
				}
			})
			protocol.enabled(false)
		}, server.TxReadCommitted, server.OptNoRetry())
		wire := protocol.snapshot()
		if !decodedValues || !slices.Equal(found, ids) || wire[server.DefaultPgVaultResourceName]["ready_replies_observed"] != 1 ||
			wire[server.MaintenancePgVaultResourceName]["ready_replies_observed"] != 0 {
			t.Fatal("cold typed cohort query added a prepare exchange or changed exact IDs", found, ids, wire)
		}
	})
}

// No prepare warming, injected clock or singleton path can complete this
// eight-contract cold cohort. Exact reports, money and output custody are read
// back independently after its acknowledged transaction has returned.
func TestLegacyFinancialCohortColdExecKeepsExactAccounting(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		fixture := legacyFinancialCohortSeed(t, ctx, legacyFinancialCohortLimit)
		protocol, cleanup := legacyFinancialRunProtocolBind(t, ctx)
		defer cleanup()
		before := contractClosedCounter.Snapshot()
		protocol.enabled(true)
		attempts, err := flushLegacySettlementCohort(ctx, fixture.ids)
		protocol.enabled(false)
		if err != nil || len(attempts) != len(fixture.ids) {
			t.Fatal("cold cohort did not return its complete exact input", attempts, err)
		}
		for index, attempt := range attempts {
			if attempt.contractId != fixture.ids[index] || !attempt.completed || attempt.busy || attempt.fallback ||
				attempt.deadlineFallback || attempt.financialWriteRollback {
				t.Fatal("cold cohort repeated or bypassed financial work", attempt)
			}
		}
		after := contractClosedCounter.Snapshot()
		if !before.Stable || !after.Stable || after.Confirmed-before.Confirmed != uint64(len(fixture.ids)) ||
			after.Uncertain != before.Uncertain || after.Untracked != before.Untracked {
			t.Fatal("cold cohort lost exact acknowledged close custody", before, after)
		}
		legacyFinancialCohortRequire(t, ctx, fixture, legacyFinancialCohortCompleted(fixture.ids))
		t.Logf("cold_financial_cohort_protocol=%v", protocol.snapshot())
	})
}
