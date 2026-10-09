// Source closes retain the ordinary financial revalidation without entering a
// batch that requires escrow. Its EOF and replay remain the same owner boundary.
package model

import (
	"testing"

	"github.com/urnetwork/server"
)

func TestContractCloseOwnerFreePageDoesNotEnterFinancialCohort(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		requireContractCloseOwnerTestSchema(t, ctx)
		owner := ContractCloseOwner{Kind: ContractCloseOwnerSourceClient, Id: server.NewId()}
		ids := make([]server.Id, legacyFinancialCohortLimit)
		for index := range ids {
			ids[index] = server.NewId()
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract
 (contract_id,source_network_id,source_id,destination_network_id,destination_id,transfer_byte_count)
 SELECT id,$2,$3,$4,$5,0 FROM unnest($1::uuid[]) AS requested(id)`,
				ids, server.NewId(), owner.Id, server.NewId(), server.NewId()))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO legacy_settlement_intent
 (contract_id,shard,outcome,next_attempt_time)
 SELECT id,get_byte(uuid_send(id),15)%16,'settled',timestamp '2010-01-01'
 FROM unnest($1::uuid[]) AS requested(id)`, ids))
		}, server.TxReadCommitted, server.OptNoRetry())
		before := contractClosedCounter.Snapshot()
		result, err := runLegacyCloseSettlementPages(ctx, owner, nil, LegacySettlementPageLimit, true)
		if err != nil || result.Visited != len(ids) || result.Completed != len(ids) ||
			result.BusyOrGone != 0 || result.Failed != 0 || result.More || result.Cursor != nil {
			t.Fatal("free source page did not complete through its ordinary EOF", result, err)
		}
		if result.FinancialCohortAttempts != 0 || result.FinancialCohortSelected != 0 ||
			result.FinancialCohortCompleted != 0 || result.FinancialCohortFallbacks != 0 || result.FinancialCohortWriteRollbacks != 0 {
			t.Fatal("free source entered an unsupported financial cohort", result)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var exact bool
			server.Raise(conn.QueryRow(ctx, `SELECT
 (SELECT count(*) FROM transfer_contract WHERE contract_id=ANY($1::uuid[]) AND outcome='settled')=$2
 AND NOT EXISTS(SELECT 1 FROM legacy_settlement_intent WHERE contract_id=ANY($1::uuid[]))
 AND NOT EXISTS(SELECT 1 FROM transfer_escrow WHERE contract_id=ANY($1::uuid[]))
 AND NOT EXISTS(SELECT 1 FROM transfer_escrow_sweep WHERE contract_id=ANY($1::uuid[]))
 AND NOT EXISTS(SELECT 1 FROM transfer_debit_journal WHERE contract_id=ANY($1::uuid[]))`, ids, len(ids)).Scan(&exact))
			if !exact {
				t.Fatal("free source lost an outcome or created financial records")
			}
		}, server.OptNoRetry())
		replay, err := runLegacyCloseSettlementPages(ctx, owner, nil, LegacySettlementPageLimit, true)
		if err != nil || replay.Visited != 0 || replay.Completed != 0 || replay.More || replay.Cursor != nil {
			t.Fatal("free source EOF replay repeated work", replay, err)
		}
		after := contractClosedCounter.Snapshot()
		if !before.Stable || !after.Stable || after.Confirmed-before.Confirmed != uint64(len(ids)) ||
			after.Uncertain != before.Uncertain || after.Untracked != before.Untracked {
			t.Fatal("free source replay changed acknowledged close custody", before, after)
		}
	})
}
