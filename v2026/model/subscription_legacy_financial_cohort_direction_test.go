// Missing usage authority stops only its chronological cohort prefix, before
// any shared outcome write. PostgreSQL framing witnesses actual rollback work.
package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// Head, middle and final refusals retain every untouched intent and report.
// The baseline sends NULL usage in the shared outcome statement and rolls back
// the acknowledged intent deletion, even though its rollback flag stays false.
func TestLegacyFinancialCohortMissingDirectionPreservesPrefix(t *testing.T) {
	for _, missingIndex := range []int{0, 3, 7} {
		env := server.DefaultTestEnv()
		env.RerunCount = 0
		env.Run(t, func(t testing.TB) {
			ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
			defer cancel()
			f := legacyFinancialCohortSeed(t, ctx, 8)
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET usage_origin_is_source=NULL WHERE contract_id=$1`, f.ids[missingIndex]))
			}, server.TxReadCommitted, server.OptNoRetry())
			wire, closeWire := legacyFinancialRunProtocolBind(t, ctx)
			defer closeWire()
			before := contractClosedCounter.Snapshot()
			wire.enabled(true)
			attempts, err := flushLegacySettlementCohort(ctx, f.ids)
			wire.enabled(false)
			if err != nil || len(attempts) != missingIndex+1 {
				t.Fatal("missing usage direction lost the chronological prefix", missingIndex, attempts, err)
			}
			for index, attempt := range attempts {
				if attempt.contractId != f.ids[index] || attempt.busy || attempt.deadlineFallback || attempt.financialWriteRollback ||
					attempt.completed != (index < missingIndex) || attempt.fallback != (index == missingIndex) {
					t.Fatal("missing usage direction changed an exact cohort owner", missingIndex, index, attempt)
				}
			}
			for route, counts := range wire.snapshot() {
				if counts["transactions_with_writes_rolled_back"] != 0 || counts["rollback_commands_observed"] != 0 ||
					counts["connections_closed_with_active_writes"] != 0 || counts["connections_closed_after_begin_without_end_command"] != 0 {
					t.Fatal("missing usage direction rolled back healthy shared work", missingIndex, route, counts)
				}
				if missingIndex == 0 && counts["transactions_with_writes_committed"] != 0 {
					t.Fatal("a missing-direction head wrote before fallback", route, counts)
				}
			}
			legacyFinancialCohortRequire(t, ctx, f, legacyFinancialCohortCompleted(f.ids[:missingIndex]))
			after := contractClosedCounter.Snapshot()
			if !before.Stable || !after.Stable || after.Confirmed-before.Confirmed != uint64(missingIndex) ||
				after.Uncertain != before.Uncertain || after.Untracked != before.Untracked {
				t.Fatal("missing usage direction changed confirmed close custody", before, after)
			}
		})
	}
}

// Ordinary failure owns its own backoff and cursor. The same payer's healthy
// tail and a new arrival after EOF remain discoverable without repairing it.
func TestLegacyFinancialCohortMissingDirectionPayerContinuation(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		f := legacyFinancialCohortSeed(t, ctx, 8)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET usage_origin_is_source=NULL WHERE contract_id=$1`, f.ids[3]))
		}, server.TxReadCommitted, server.OptNoRetry())
		first, err := FlushLegacyPayerSettlements(ctx, f.payer.sourceNetworkId, nil, 8)
		if err != nil || first.Visited != 4 || first.Completed != 3 || first.Failed != 1 || first.BusyOrGone != 0 ||
			first.Cursor == nil || first.Cursor.ContractId != f.ids[3] || first.FinancialCohortCompleted != 3 ||
			first.FinancialCohortFallbacks != 1 || first.FinancialCohortWriteRollbacks != 0 {
			t.Fatal("missing usage direction changed the public committed prefix", first, err)
		}
		legacyFinancialCohortRequire(t, ctx, f, legacyFinancialCohortCompleted(f.ids[:3]))
		server.Db(ctx, func(conn server.PgConn) {
			var retained bool
			server.Raise(conn.QueryRow(ctx, `SELECT failure_code='operational' AND next_attempt_time>statement_timestamp() AT TIME ZONE 'UTC'
            FROM legacy_settlement_intent WHERE contract_id=$1`, f.ids[3]).Scan(&retained))
			if !retained {
				t.Fatal("missing usage direction lost its own ordinary backoff")
			}
		})
		tail, err := FlushLegacyPayerSettlements(ctx, f.payer.sourceNetworkId, first.Cursor, 8)
		if err != nil || tail.Completed != 4 || tail.Failed != 0 || tail.BusyOrGone != 0 || tail.Cursor != nil || tail.More {
			t.Fatal("same payer's healthy tail did not reach EOF", tail, err)
		}
		completed := legacyFinancialCohortCompleted(f.ids)
		delete(completed, f.ids[3])
		legacyFinancialCohortRequire(t, ctx, f, completed)

		// Public CloseContract queues fresh work for this same actual payer.
		provider := f.providers[0]
		later := f.payer
		later.destinationId, later.destinationNetworkId = provider.destinationId, provider.destinationNetworkId
		id := newLegacyPayerTestIntent(t, ctx, later, server.NewId(), 16, 3)
		f.ids = append(f.ids, id)
		f.balanceIds = append(f.balanceIds, f.balances[0])
		f.providerIndexes = append(f.providerIndexes, 0)
		f.reservationAmounts = append(f.reservationAmounts, 16)
		f.shards = append(f.shards, int(id[15])%LegacySettlementShardCount)
		f.reports = legacyFinancialCohortReports(ctx, f.ids)
		var cursor *LegacySettlementCursor
		newCompleted := 0
		for range 2 {
			page, err := FlushLegacyPayerSettlements(ctx, f.payer.sourceNetworkId, cursor, 8)
			if err != nil || page.BusyOrGone != 0 || page.Failed > 1 || page.FinancialCohortWriteRollbacks != 0 {
				t.Fatal("fresh payer pass lost its unchanged missing-direction owner", page, err)
			}
			newCompleted += page.Completed
			cursor = page.Cursor
			if cursor == nil {
				break
			}
		}
		if newCompleted != 1 || cursor != nil {
			t.Fatal("same payer's new healthy work was hidden behind an EOF refusal", newCompleted, cursor)
		}
		completed[id] = true
		legacyFinancialCohortRequire(t, ctx, f, completed)
	})
}
