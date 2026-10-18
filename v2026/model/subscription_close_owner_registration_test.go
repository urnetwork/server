// Historical unresolved escrow ownership must not pin the bounded registration
// lane, and rolling writers must not overwrite a classified free source owner.
package model

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// More than one complete page of unresolved historical escrow owners precedes
// healthy paid and free rows. Persisted classification must pass that prefix
// without relabeling it as free, changing retry state, or touching finances.
func TestContractCloseOwnerRegistrationPassesUnresolvedEscrowPrefix(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		requireContractCloseOwnerTestSchema(t, ctx)
		payer := newNetEscrowOrderingTestFixture(t, ctx)
		otherPayer := newNetEscrowOrderingTestFixture(t, ctx)
		const shard = 3
		const unresolvedCount = legacySettlementPayerRegistrationLimit + 1
		prefix := server.NewId()
		badIds := make([]server.Id, unresolvedCount)
		missingGrantIds, ambiguousGrantIds := []server.Id{}, []server.Id{}
		for index := range badIds {
			id := legacyPayerTestContractId(prefix, uint32(index+1), shard)
			badIds[index] = id
			if index%2 == 0 {
				missingGrantIds = append(missingGrantIds, id)
			} else {
				ambiguousGrantIds = append(ambiguousGrantIds, id)
			}
		}
		paidId := legacyPayerTestContractId(prefix, uint32(unresolvedCount+1), shard)
		freeId := legacyPayerTestContractId(prefix, uint32(unresolvedCount+2), shard)
		allIds := append(append([]server.Id{}, badIds...), paidId, freeId)
		missingBalanceId := server.NewId()
		due := time.Date(2010, time.January, 1, 0, 0, 0, 0, time.UTC)
		server.Tx(ctx, func(tx server.PgTx) {
			// The initial explicit payer permits ordinary insertion. Removing
			// that cache and classification marker models pre-migration rows.
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract
 (contract_id,source_network_id,source_id,destination_network_id,destination_id,payer_network_id,transfer_byte_count,usage_origin_is_source)
 SELECT id,$2,$3,$4,$5,$2,2,true FROM unnest($1::uuid[]) AS requested(id)`,
				allIds, payer.sourceNetworkId, payer.sourceId, payer.destinationNetworkId, payer.destinationId))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_escrow(contract_id,balance_id,balance_byte_count)
 SELECT id,$2,2 FROM unnest($1::uuid[]) AS requested(id)`, missingGrantIds, missingBalanceId))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_escrow(contract_id,balance_id,balance_byte_count)
 SELECT id,balance_id,1 FROM unnest($1::uuid[]) AS requested(id)
 CROSS JOIN unnest($2::uuid[]) AS balance(balance_id)`, ambiguousGrantIds, []server.Id{payer.balanceId, otherPayer.balanceId}))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_escrow(contract_id,balance_id,balance_byte_count)
 VALUES($1,$2,2)`, paidId, payer.balanceId))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_close(contract_id,party,used_transfer_byte_count,close_time,checkpoint)
 SELECT id,party,1,$2,false FROM unnest($1::uuid[]) AS requested(id)
 CROSS JOIN (VALUES ('source'),('destination')) AS report(party)`, allIds, due))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO legacy_settlement_intent(contract_id,shard,outcome,next_attempt_time)
 SELECT id,$2,'settled',$3 FROM unnest($1::uuid[]) AS requested(id)`, allIds, shard, due))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET payer_network_id=NULL WHERE contract_id=ANY($1::uuid[])`, allIds))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE legacy_settlement_intent
 SET payer_network_id=NULL,source_client_id=NULL WHERE contract_id=ANY($1::uuid[])`, allIds))
			// One old populated hint also reaches discovery before repair. Its
			// unresolved actual owner must not prevent registration from running.
			server.RaisePgResult(tx.Exec(ctx, `UPDATE legacy_settlement_intent SET payer_network_id=$2
 WHERE contract_id=$1`, badIds[0], payer.sourceNetworkId))
		}, server.TxReadCommitted, server.OptNoRetry())
		originalReports := legacyFinancialCohortReports(ctx, allIds)
		const escrowSnapshotSql = `SELECT jsonb_agg(jsonb_build_array(contract_id,balance_id,balance_byte_count,
 settled,settle_time,payout_byte_count) ORDER BY contract_id,balance_id)
 FROM transfer_escrow WHERE contract_id=ANY($1::uuid[])`
		var originalEscrows []byte
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, escrowSnapshotSql, allIds).Scan(&originalEscrows))
			for _, id := range []server.Id{badIds[0], badIds[1]} {
				owner, _, err := readContractCloseOwnerInConn(ctx, conn, id)
				if err == nil || owner != (ContractCloseOwner{}) {
					t.Fatal("unresolved fixture unexpectedly has an actual close owner", id, owner, err)
				}
			}
		})

		firstResult, err := DispatchLegacySettlementCloseOwners(ctx, shard, nil, nil, nil)
		first, registered, unresolved := firstResult.RegistrationCursor, firstResult.Registered, firstResult.RegistrationUnresolved
		if err != nil || firstResult.RegistrationFailed || !firstResult.More ||
			len(firstResult.PayerNetworkIds) != 0 || len(firstResult.SourceClientIds) != 0 ||
			registered != 0 || unresolved != legacySettlementPayerRegistrationLimit || first == nil || first.After == nil ||
			*first.After != badIds[legacySettlementPayerRegistrationLimit-1] || first.End != freeId {
			t.Fatal("unresolved first page lost its bounded continuation", firstResult, err)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var untouched bool
			server.Raise(conn.QueryRow(ctx, `SELECT count(*)=$2 AND bool_and(payer_network_id IS NULL AND source_client_id IS NULL)
 FROM legacy_settlement_intent WHERE contract_id=ANY($1::uuid[])`, allIds, len(allIds)).Scan(&untouched))
			if !untouched {
				t.Fatal("first page labeled unresolved escrow free or exceeded its row bound")
			}
		})
		encoded, err := json.Marshal(firstResult)
		server.Raise(err)
		var continued LegacySettlementDispatchResult
		server.Raise(json.Unmarshal(encoded, &continued))
		secondResult, err := DispatchLegacySettlementCloseOwners(ctx, shard, continued.Cursor, continued.PayerCursor,
			continued.SourceCursor, continued.RegistrationCursor)
		next, registered, unresolved := secondResult.RegistrationCursor, secondResult.Registered, secondResult.RegistrationUnresolved
		if err != nil || secondResult.RegistrationFailed || next != nil || registered != 2 || unresolved != 1 {
			t.Fatal("unresolved prefix starved later healthy paid or free classification", secondResult, err)
		}
		after, err := json.Marshal(&continued)
		server.Raise(err)
		if !bytes.Equal(encoded, after) {
			t.Fatal("registration mutated the caller's persisted cursor")
		}

		assertCustody := func() {
			t.Helper()
			if !bytes.Equal(originalReports, legacyFinancialCohortReports(ctx, allIds)) {
				t.Fatal("owner registration rewrote original reports")
			}
			server.Db(ctx, func(conn server.PgConn) {
				var escrows []byte
				server.Raise(conn.QueryRow(ctx, escrowSnapshotSql, allIds).Scan(&escrows))
				if !bytes.Equal(originalEscrows, escrows) {
					t.Fatal("owner registration mutated escrow custody")
				}
				var exact bool
				server.Raise(conn.QueryRow(ctx, `SELECT
 (SELECT count(*) FROM legacy_settlement_intent WHERE contract_id=ANY($1::uuid[]) AND payer_network_id IS NULL AND source_client_id IS NULL)=$2
 AND (SELECT payer_network_id=$4 AND source_client_id=$5 FROM legacy_settlement_intent WHERE contract_id=$3)
 AND (SELECT payer_network_id IS NULL AND source_client_id=$5 FROM legacy_settlement_intent WHERE contract_id=$6)
 AND (SELECT count(*) FROM legacy_settlement_intent WHERE contract_id=ANY($7::uuid[]) AND next_attempt_time=$8 AND failure_code='none' AND outcome='settled' AND NOT clear_dispute)=$9
 AND NOT EXISTS(SELECT 1 FROM transfer_contract WHERE contract_id=ANY($7::uuid[]) AND (outcome IS NOT NULL OR provider_usage IS NOT NULL OR payer_network_id IS NOT NULL))
 AND NOT EXISTS(SELECT 1 FROM transfer_escrow_sweep WHERE contract_id=ANY($7::uuid[]))
 AND NOT EXISTS(SELECT 1 FROM transfer_debit_journal WHERE contract_id=ANY($7::uuid[]))
 AND (SELECT count(*) FROM transfer_balance WHERE balance_id=ANY($10::uuid[]) AND balance_byte_count=1000)=2
 AND NOT EXISTS(SELECT 1 FROM transfer_balance WHERE balance_id=$11)`,
					badIds, len(badIds), paidId, payer.sourceNetworkId, payer.sourceId, freeId,
					allIds, due, len(allIds), []server.Id{payer.balanceId, otherPayer.balanceId}, missingBalanceId).Scan(&exact))
				if !exact {
					t.Fatal("classification changed unresolved authority, retry state, or money")
				}
			})
		}
		assertCustody()
		discovered, err := DispatchLegacySettlementCloseOwners(ctx, shard, secondResult.Cursor, secondResult.PayerCursor,
			secondResult.SourceCursor, secondResult.RegistrationCursor)
		if err != nil || discovered.RegistrationFailed || len(discovered.PayerNetworkIds) != 1 || discovered.PayerNetworkIds[0] != payer.sourceNetworkId ||
			len(discovered.SourceClientIds) != 1 || discovered.SourceClientIds[0] != payer.sourceId {
			t.Fatal("healthy owners behind unresolved custody did not reach actual dispatch", discovered, err)
		}
		// Another fixed pass revisits unresolved evidence once per page but
		// never re-enrolls the already classified healthy rows.
		first, registered, unresolved = registerLegacyCloseOwnerPage(ctx, shard, nil)
		if first == nil || first.End != badIds[len(badIds)-1] || registered != 0 || unresolved != legacySettlementPayerRegistrationLimit {
			t.Fatal("next registration round lost its remaining unresolved boundary", first, registered, unresolved)
		}
		next, registered, unresolved = registerLegacyCloseOwnerPage(ctx, shard, first)
		if next != nil || registered != 0 || unresolved != 1 {
			t.Fatal("unresolved registration replay repeated healthy work or failed to terminate", next, registered, unresolved)
		}
		assertCustody()
	})
}

// Old registration writers can issue a direct UPDATE after newer code has
// classified an intent. That write must not strand free work under a network
// hint while leaving its non-NULL source marker outside the repair lane.
func TestContractCloseOwnerRollingPayerUpdatePreservesClassifiedFreeSource(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		requireContractCloseOwnerTestSchema(t, ctx)
		contractId, sourceId, destinationId := server.NewId(), server.NewId(), server.NewId()
		sourceNetworkId, destinationNetworkId := server.NewId(), server.NewId()
		due := time.Date(2020, time.January, 1, 0, 0, 0, 0, time.UTC)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract
 (contract_id,source_network_id,source_id,destination_network_id,destination_id,transfer_byte_count,usage_origin_is_source)
 VALUES($1,$2,$3,$4,$5,0,true)`, contractId, sourceNetworkId, sourceId, destinationNetworkId, destinationId))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO legacy_settlement_intent(contract_id,shard,outcome,next_attempt_time,payer_network_id)
 VALUES($1,$2,'settled',$3,$4)`, contractId, int(contractId[15])%LegacySettlementShardCount, due, sourceNetworkId))
		}, server.TxReadCommitted, server.OptNoRetry())
		for _, hint := range []server.Id{sourceNetworkId, destinationNetworkId} {
			server.Tx(ctx, func(tx server.PgTx) {
				var classified bool
				server.Raise(tx.QueryRow(ctx, `SELECT payer_network_id IS NULL AND source_client_id=$2
 FROM legacy_settlement_intent WHERE contract_id=$1`, contractId, sourceId).Scan(&classified))
				if !classified {
					t.Fatal("fixture lost its classified free source before the old write")
				}
				server.RaisePgResult(tx.Exec(ctx, `UPDATE legacy_settlement_intent SET payer_network_id=$2 WHERE contract_id=$1`, contractId, hint))
			}, server.TxReadCommitted, server.OptNoRetry())
			server.Db(ctx, func(conn server.PgConn) {
				var exact bool
				server.Raise(conn.QueryRow(ctx, `SELECT payer_network_id IS NULL AND source_client_id=$2
 AND next_attempt_time=$3 AND failure_code='none' AND outcome='settled' AND NOT clear_dispute
 AND NOT EXISTS(SELECT 1 FROM transfer_escrow WHERE contract_id=$1)
 AND NOT EXISTS(SELECT 1 FROM transfer_escrow_sweep WHERE contract_id=$1)
 AND NOT EXISTS(SELECT 1 FROM transfer_debit_journal WHERE contract_id=$1)
 AND NOT EXISTS(SELECT 1 FROM transfer_contract WHERE contract_id=$1 AND (outcome IS NOT NULL OR provider_usage IS NOT NULL))
 AND NOT EXISTS(SELECT 1 FROM network_client WHERE client_id=ANY($4::uuid[]))
 FROM legacy_settlement_intent WHERE contract_id=$1`, contractId, sourceId, due, []server.Id{sourceId, destinationId}).Scan(&exact))
				if !exact {
					t.Fatal("rolling payer update stranded classified free work or changed custody", hint)
				}
			})
		}
	})
}

// A deferred database rejection occurs after the classification statements
// return. The dispatcher must publish neither their counts nor advanced cursor
// before that commit succeeds, and the original paid intent remains intact.
func TestContractCloseOwnerRegistrationCommitFailureRetainsCursorAndCustody(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		requireContractCloseOwnerTestSchema(t, ctx)
		fixture, contractId := legacySettlementTestIntent(t, ctx)
		shard := int(contractId[15]) % LegacySettlementShardCount
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE legacy_settlement_intent
 SET payer_network_id=NULL,source_client_id=NULL WHERE contract_id=$1`, contractId))
			server.RaisePgResult(tx.Exec(ctx, `CREATE SEQUENCE close_owner_registration_test_commit;
 CREATE FUNCTION fail_close_owner_registration_test_commit()
 RETURNS trigger LANGUAGE plpgsql AS $body$
 BEGIN
  IF NEW.source_client_id IS NOT NULL THEN
   PERFORM nextval('close_owner_registration_test_commit');
   RAISE EXCEPTION 'synthetic owner classification commit refusal' USING ERRCODE='23514';
  END IF;
  RETURN NEW;
 END;
 $body$;
 CREATE CONSTRAINT TRIGGER fail_close_owner_registration_test_commit
 AFTER UPDATE ON legacy_settlement_intent DEFERRABLE INITIALLY DEFERRED
 FOR EACH ROW EXECUTE FUNCTION fail_close_owner_registration_test_commit()`))
		}, server.TxReadCommitted, server.OptNoRetry())
		defer server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DROP TRIGGER fail_close_owner_registration_test_commit ON legacy_settlement_intent;
 DROP FUNCTION fail_close_owner_registration_test_commit();
 DROP SEQUENCE close_owner_registration_test_commit`))
		}, server.TxReadCommitted, server.OptNoRetry())
		beforeReports := legacyFinancialCohortReports(ctx, []server.Id{contractId})
		firstKey := server.Id{}
		cursor := &LegacySettlementOwnerCursor{After: &firstKey, End: contractId}
		before, err := json.Marshal(cursor)
		server.Raise(err)
		result, err := DispatchLegacySettlementCloseOwners(ctx, shard, nil, nil, nil, cursor)
		if err != nil || !result.RegistrationFailed || result.Registered != 0 || result.RegistrationUnresolved != 0 ||
			result.RegistrationCursor == nil || !result.More || len(result.PayerNetworkIds) != 0 || len(result.SourceClientIds) != 0 {
			t.Fatal("failed classification commit published progress or lost continuation", result, err)
		}
		for _, retained := range []*LegacySettlementOwnerCursor{cursor, result.RegistrationCursor} {
			after, err := json.Marshal(retained)
			server.Raise(err)
			if !bytes.Equal(before, after) {
				t.Fatal("failed classification commit advanced an input or output cursor", retained)
			}
		}
		if !bytes.Equal(beforeReports, legacyFinancialCohortReports(ctx, []server.Id{contractId})) {
			t.Fatal("failed classification commit changed original reports")
		}
		server.Db(ctx, func(conn server.PgConn) {
			var untouched bool
			var refusedAtCommit bool
			server.Raise(conn.QueryRow(ctx, `SELECT is_called AND last_value=1 FROM close_owner_registration_test_commit`).Scan(&refusedAtCommit))
			if !refusedAtCommit {
				t.Fatal("classification failed before the deferred commit refusal")
			}
			server.Raise(conn.QueryRow(ctx, `SELECT payer_network_id IS NULL AND source_client_id IS NULL
 AND failure_code='none' AND outcome='settled' AND NOT clear_dispute
 FROM legacy_settlement_intent WHERE contract_id=$1`, contractId).Scan(&untouched))
			if !untouched {
				t.Fatal("failed classification commit leaked owner or retry metadata")
			}
		})
		requireLegacySettlementTestState(t, ctx, fixture, contractId, true, false, 1000, 100)
	})
}
