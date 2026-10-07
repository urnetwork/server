// Composed mainnet paths retain their original financial and signed evidence
// when later callbacks or identity retries cross the custody boundary.
package model

import (
	"bytes"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// Settlement commits its debit and usage before any post. Archival and late
// callbacks must preserve both without restoring already consumed credit.
func TestMainnetCompositionSettlementArchiveRetainsDebitAndUsage(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		ctx := t.Context()
		fixture := newNetEscrowOrderingTestFixture(t, ctx)
		contract, createPosts := createNetEscrowOrderingTestContract(ctx, fixture, 700)
		var settlementPosts []func() any
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_close
				(contract_id,party,used_transfer_byte_count,close_time,checkpoint)
				VALUES($1,'source',600,$2,false),($1,'destination',600,$2,false)`, contract.ContractId, server.NowUtc()))
			var closed bool
			var err error
			settlementPosts, closed, err = settleEscrowInTx(ctx, tx, contract.ContractId, ContractOutcomeSettled)
			server.Raise(err)
			if !closed {
				t.Fatal("settlement did not claim its terminal outcome")
			}
		}, server.TxReadCommitted)
		var originalUsage []byte
		var closedAt time.Time
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT provider_usage,close_time FROM transfer_contract WHERE contract_id=$1`, contract.ContractId).Scan(&originalUsage, &closedAt))
		})
		assertCustody := func() {
			t.Helper()
			var balance ByteCount
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT balance_byte_count FROM transfer_balance WHERE balance_id=$1`, fixture.balanceId).Scan(&balance))
			})
			if balance != 400 {
				t.Fatalf("settlement/callback changed durable remaining credit: got=%d want=400", balance)
			}
			usage, err := GetStEpochProviderUsageAtEpoch(ctx, 906, closedAt, closedAt.Add(time.Hour))
			if err != nil || len(usage) != 1 || usage[0].ClientId != fixture.destinationId || usage[0].NetworkId != fixture.destinationNetworkId || usage[0].PayoutByteCount != 600 {
				t.Fatalf("settlement/archive changed original provider usage: usage=%+v error=%v", usage, err)
			}
		}
		assertCustody()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM transfer_contract WHERE contract_id=$1`, contract.ContractId))
		})
		server.Db(ctx, func(conn server.PgConn) {
			var archivedUsage []byte
			var archivedTime time.Time
			var outcome ContractOutcome
			server.Raise(conn.QueryRow(ctx, `SELECT provider_usage,close_time,outcome FROM st_provider_usage_archive WHERE contract_id=$1`, contract.ContractId).Scan(&archivedUsage, &archivedTime, &outcome))
			if !bytes.Equal(archivedUsage, originalUsage) || !archivedTime.Equal(closedAt) || outcome != ContractOutcomeSettled {
				t.Fatal("archive changed the original terminal usage or its time owner")
			}
		})
		assertCustody()
		server.RunPosts(ctx, createPosts...)
		server.RunPosts(ctx, settlementPosts...)
		ReconcileNetEscrowForNetwork(ctx, fixture.sourceNetworkId, true)
		assertCustody()
		if reserved := Testing_NetEscrowByteCount(ctx, fixture.balanceId); reserved != 0 {
			t.Fatalf("late callback restored archived reservation: %d", reserved)
		}
		var admitted *TransferEscrow
		var admissionErr error
		server.Tx(ctx, func(tx server.PgTx) {
			admitted, _, admissionErr = createTransferEscrowInTx(ctx, tx,
				fixture.sourceNetworkId, fixture.sourceId, fixture.destinationNetworkId, fixture.destinationId,
				fixture.sourceNetworkId, 401, nil)
		}, server.TxReadCommitted)
		if admissionErr == nil || admitted != nil {
			t.Fatalf("archival and delayed callbacks re-admitted consumed credit: contract=%+v error=%v", admitted, admissionErr)
		}
		assertCustody()
	})
}

// A durable registration resumes the same client after a policy rollover.
// Deletion retires both namespaces and leaves the binding as a tombstone.
func TestMainnetCompositionRegistrationRetainsPolicyHistoryAfterDeletion(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		ctx := t.Context()
		clientSession, request := newNetworkClientRegistrationTest(t, ctx)
		first, err := RegisterNetworkClient(&request, clientSession)
		requireNetworkClientRegistrationTestResult(t, request, first, err)
		old := newStClientKeyHistoryTestInput(t)
		old.ClientID = *first.ClientId
		current := old
		current.Domain.PolicyHash = [32]byte{7}
		current.PublicKey = bytes.Repeat([]byte{10}, 32)
		current.Boundary.Block++
		current.Boundary.Hash = [32]byte{8}
		inputs := []StClientKeyRegistrationInput{old, current}
		records := make([]*StClientKeyHistoryRecord, 0, len(inputs))
		for _, input := range inputs {
			record, err := StoreStClientKeyRegistration(ctx, input)
			if err != nil || record == nil || record.Registration.Generation != 1 {
				t.Fatalf("independent policy did not retain generation one: %v", err)
			}
			records = append(records, record)
		}
		replayed, err := RegisterNetworkClient(&request, clientSession)
		requireNetworkClientRegistrationTestResult(t, request, replayed, err)
		if *replayed.ClientId != *first.ClientId || *replayed.DeviceId != *first.DeviceId {
			t.Fatal("policy rollover changed the original registration identity")
		}
		for index, input := range inputs {
			history, err := LoadStClientKeyHistory(ctx, input.Domain, input.ClientID, 1, MaxStClientKeyHistoryBytes)
			if err != nil || len(history) != 1 || !bytes.Equal(history[0].EvidenceBytes, records[index].EvidenceBytes) {
				t.Fatalf("registration replay changed policy %d signed evidence: %v", index, err)
			}
		}
		key, err := GetClientPublicKey(ctx, current.ClientID)
		if err != nil || !bytes.Equal(key, current.PublicKey) {
			t.Fatalf("registration replay lost the current policy projection: %v", err)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM network_client_role WHERE client_id=$1`, *first.ClientId))
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM network_client WHERE client_id=$1`, *first.ClientId))
		})
		refused, err := RegisterNetworkClient(&request, clientSession)
		if err != nil || refused == nil || refused.Error == nil || refused.Error.Code != "identity_unavailable" || refused.ClientId != nil || refused.ByClientJwt != nil {
			t.Fatalf("deleted registration recovered signing authority: error=%v result=%+v", err, refused)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var heads, retired, bindings, clients int
			server.Raise(conn.QueryRow(ctx, `SELECT
				(SELECT count(*) FROM st_client_key_head WHERE client_id=$1),
				(SELECT count(*) FROM st_client_key_head WHERE client_id=$1 AND retired),
				(SELECT count(*) FROM network_client_registration WHERE network_id=$2 AND client_id=$1 AND device_id=$3),
				(SELECT count(*) FROM network_client WHERE network_id=$2)`, *first.ClientId, clientSession.ByJwt.NetworkId, *first.DeviceId).Scan(&heads, &retired, &bindings, &clients))
			if heads != 2 || retired != 2 || bindings != 1 || clients != 0 {
				t.Fatalf("deletion/replay changed durable ownership: heads=%d retired=%d bindings=%d clients=%d", heads, retired, bindings, clients)
			}
			for index, input := range inputs {
				domainHash, err := input.Domain.Digest()
				server.Raise(err)
				var evidence, registration []byte
				server.Raise(conn.QueryRow(ctx, `SELECT evidence,registration FROM st_client_key_history WHERE client_id=$1 AND domain_hash=$2 AND generation=1`, input.ClientID, domainHash[:]).Scan(&evidence, &registration))
				if !bytes.Equal(evidence, records[index].EvidenceBytes) || !bytes.Equal(registration, records[index].RegistrationBytes) {
					t.Fatalf("deletion rewrote policy %d historical approval", index)
				}
			}
		})
		key, err = GetClientPublicKey(ctx, current.ClientID)
		if err != nil || len(key) != 0 {
			t.Fatalf("deleted client retained current policy authority: %v", err)
		}
	})
}
