// Registration has a bounded lane independent of an older chronological pass.
package model

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// The first registration turn must reach one missing payer even though a full
// registered prefix precedes it and the saved pass cutoff excludes its due time.
// Neither reports, retry eligibility nor financial rows may change.
func TestLegacyPayerDispatchRegistersOutsideOldChronologicalPass(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		missing, missingId := legacySettlementTestIntent(t, ctx)
		shard := int(missingId[15]) % LegacySettlementShardCount
		registered := newNetEscrowOrderingTestFixture(t, ctx)
		ids := make([]server.Id, legacySettlementPayerRegistrationLimit+1)
		prefix := server.NewId()
		for i := range ids {
			ids[i] = legacyPayerTestContractId(prefix, uint32(i+1), shard)
		}
		now := server.NowUtc().Truncate(time.Microsecond)
		cutoff := now.Add(-84 * time.Hour)
		due := now.Add(-58 * time.Hour)
		after := &LegacySettlementCursor{NextAttemptTime: now.Add(-120 * time.Hour), ContractId: server.NewId(), PassEndTime: cutoff}
		beforeCursor, err := json.Marshal(after)
		server.Raise(err)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract
 (contract_id,source_network_id,source_id,destination_network_id,destination_id,payer_network_id,transfer_byte_count,usage_origin_is_source)
 SELECT id,$2,$3,$4,$5,$2,0,true FROM unnest($1::uuid[]) AS pending(id)`,
				ids, registered.sourceNetworkId, registered.sourceId, registered.destinationNetworkId, registered.destinationId))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO legacy_settlement_intent(contract_id,shard,outcome,next_attempt_time,payer_network_id)
 SELECT id,$2,'settled',$3,$4 FROM unnest($1::uuid[]) AS pending(id)`,
				ids, shard, now.Add(-96*time.Hour), registered.sourceNetworkId))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE legacy_settlement_intent SET payer_network_id=NULL,source_client_id=NULL,next_attempt_time=$2 WHERE contract_id=$1`, missingId, due))
			var excluded bool
			server.Raise(tx.QueryRow(ctx, `SELECT payer_network_id IS NULL AND next_attempt_time>$2
 AND next_attempt_time<statement_timestamp() AT TIME ZONE 'UTC'
 FROM legacy_settlement_intent WHERE contract_id=$1`, missingId, cutoff).Scan(&excluded))
			if !excluded {
				t.Fatal("fixture did not exclude the due NULL payer from both discovery lanes")
			}
		})
		first, err := DispatchLegacySettlementPayers(ctx, shard, after, nil)
		if err != nil || first.RegistrationFailed || first.Registered != 1 || !first.More ||
			!slices.Equal(first.PayerNetworkIds, []server.Id{registered.sourceNetworkId}) {
			t.Fatal("registered chronological prefix hid newer missing payer", first, err)
		}
		unchangedCursor, err := json.Marshal(first.Cursor)
		server.Raise(err)
		if !slices.Equal(beforeCursor, unchangedCursor) {
			t.Fatal("indexed registration advanced unrelated chronological custody")
		}
		server.Db(ctx, func(conn server.PgConn) {
			var exact bool
			server.Raise(conn.QueryRow(ctx, `SELECT payer_network_id=$2 AND next_attempt_time=$3
 AND failure_code='none' AND outcome='settled' FROM legacy_settlement_intent WHERE contract_id=$1`,
				missingId, missing.sourceNetworkId, due).Scan(&exact))
			if !exact {
				t.Fatal("registration lost payer metadata or changed financial eligibility")
			}
		})
		second, err := DispatchLegacySettlementPayers(ctx, shard, first.Cursor, first.PayerCursor)
		if err != nil || second.RegistrationFailed || !slices.Contains(second.PayerNetworkIds, missing.sourceNetworkId) {
			t.Fatal("newly registered payer was not discovered next round", second, err)
		}
		requireLegacySettlementTestState(t, ctx, missing, missingId, true, false, 1000, 100)
	})
}

// The missing-index branch skips an actual held intent and still enrolls an
// independent missing payer. Release makes that exact skipped row recoverable.
func TestLegacyPayerDispatchIndexedRegistrationSkipsOwnedIntent(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		heldFixture, heldId := legacySettlementTestIntent(t, ctx)
		shard := int(heldId[15]) % LegacySettlementShardCount
		freeFixture := newNetEscrowOrderingTestFixture(t, ctx)
		freeId := newLegacyPayerTestIntent(t, ctx, freeFixture, legacyPayerTestContractId(server.NewId(), 1, shard), 100, 11)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE legacy_settlement_intent SET payer_network_id=NULL,source_client_id=NULL WHERE contract_id=ANY($1)`, []server.Id{heldId, freeId}))
		})
		releaseHeld := holdContractCloseTestRow(t, ctx,
			`SELECT contract_id FROM legacy_settlement_intent WHERE contract_id=$1 FOR UPDATE`, heldId)
		defer releaseHeld()
		first, err := DispatchLegacySettlementPayers(ctx, shard, nil, nil)
		if err != nil || first.RegistrationFailed || first.Registered != 1 || !first.More {
			t.Fatal("owned missing intent blocked independent registration", first, err)
		}
		releaseHeld()
		second, err := DispatchLegacySettlementPayers(ctx, shard, first.Cursor, first.PayerCursor)
		if err != nil || second.RegistrationFailed || second.Registered != 1 || !second.More ||
			!slices.Equal(second.PayerNetworkIds, []server.Id{freeFixture.sourceNetworkId}) {
			t.Fatal("released missing intent was not enrolled", second, err)
		}
		requireLegacySettlementTestState(t, ctx, heldFixture, heldId, true, false, 1000, 100)
		requireLegacySettlementTestState(t, ctx, freeFixture, freeId, true, false, 1000, 100)
	})
}
