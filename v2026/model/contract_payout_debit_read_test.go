// Payout assertions distinguish committed consumption from debit metadata that
// the bounded worker has not materialized yet. Missing authority is not zero.
package model

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// The journal owns pending consumption; settled escrow metadata retains it
// after the worker has debited the grant and acknowledged the journal.
type contractPayoutTestBalanceState struct {
	remaining, consumed, pendingDebit ByteCount
	journalRows, appliedRows          int
}

// Read one snapshot and require an exact pending or materialized shape. A
// nullable amount remains observable and can only fall back to its own journal.
func readContractPayoutTestBalance(ctx context.Context, balanceId, contractId server.Id) (state contractPayoutTestBalanceState, returnErr error) {
	var metadata, debit *ByteCount
	var applied *bool
	var settled, redisReserved bool
	var settleTime *time.Time
	var outcome *ContractOutcome
	var reservation ByteCount
	server.Db(ctx, func(conn server.PgConn) {
		returnErr = conn.QueryRow(ctx, `
			SELECT balance.balance_byte_count, escrow.payout_byte_count,
				(SELECT COALESCE(SUM(debit_byte_count),0)::bigint
				 FROM transfer_debit_journal WHERE balance_id=$1 AND NOT applied),
				(SELECT COUNT(*) FROM transfer_debit_journal WHERE balance_id=$1),
				(SELECT COUNT(*) FROM transfer_debit_journal WHERE balance_id=$1 AND applied),
				escrow.settled, escrow.settle_time, escrow.redis_reserved, escrow.balance_byte_count,
				contract.outcome, debit.debit_byte_count, debit.applied
			FROM transfer_balance balance
			JOIN transfer_escrow escrow ON escrow.balance_id=balance.balance_id
			JOIN transfer_contract contract ON contract.contract_id=escrow.contract_id
			LEFT JOIN transfer_debit_journal debit ON debit.balance_id=escrow.balance_id AND debit.contract_id=escrow.contract_id
			WHERE balance.balance_id=$1 AND escrow.contract_id=$2`, balanceId, contractId).
			Scan(&state.remaining, &metadata, &state.pendingDebit, &state.journalRows, &state.appliedRows,
				&settled, &settleTime, &redisReserved, &reservation, &outcome, &debit, &applied)
	})
	if returnErr != nil {
		return
	}
	if outcome == nil {
		return state, fmt.Errorf("payout consumption has no terminal contract outcome")
	}
	if metadata != nil {
		if !settled || settleTime == nil || *metadata < 0 || *metadata > reservation ||
			(debit != nil && (!redisReserved || applied == nil || !*applied || *debit != *metadata)) {
			return state, fmt.Errorf("settled payout metadata conflicts with its lifecycle or debit authority")
		}
		state.consumed = *metadata
	} else {
		if !redisReserved || settled || settleTime != nil || debit == nil || applied == nil || *applied ||
			*debit < 0 || *debit > reservation {
			return state, fmt.Errorf("unmaterialized payout metadata has no exact pending native debit")
		}
		state.consumed = *debit
	}
	return
}

// Public closes deliberately finish without running an independent debit
// worker. Every identity and balance is created by this synthetic fixture.
func newContractPayoutDebitReadTestFixture(t testing.TB, ctx context.Context, sameNetwork bool, consumed ByteCount) (balanceId, contractId server.Id) {
	t.Helper()
	payerNetworkId, providerNetworkId := server.NewId(), server.NewId()
	if sameNetwork {
		providerNetworkId = payerNetworkId
	}
	payerId, providerId := server.NewId(), server.NewId()
	addContractPayoutTestClients(ctx, map[server.Id]server.Id{payerId: payerNetworkId, providerId: providerNetworkId})
	balance := addContractPayoutTestBalance(ctx, payerNetworkId, 121)
	escrow, err := CreateTransferEscrow(ctx, payerNetworkId, payerId, providerNetworkId, providerId, 121)
	if err != nil || escrow == nil || escrow.TransferByteCount != 121 {
		t.Fatalf("create exact native payout reservation: escrow=%v err=%v", escrow, err)
	}
	for _, clientId := range []server.Id{payerId, providerId} {
		if err := CloseContract(ctx, escrow.ContractId, clientId, consumed, false); err != nil {
			t.Fatal("close native payout reservation", err)
		}
	}
	return balance.BalanceId, escrow.ContractId
}

// The original shared assertion scans the deliberately absent escrow amount
// into an integer and fails here before it can run the real debit worker.
func TestContractPayoutBalanceAssertionReadsPendingDebit(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		for _, sameNetwork := range []bool{false, true} {
			balanceId, contractId := newContractPayoutDebitReadTestFixture(t, ctx, sameNetwork, 121)
			var pending bool
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT
					escrow.redis_reserved AND NOT escrow.settled AND escrow.settle_time IS NULL
					AND escrow.payout_byte_count IS NULL AND debit.debit_byte_count=121 AND NOT debit.applied
					FROM transfer_escrow escrow JOIN transfer_debit_journal debit USING (contract_id,balance_id)
					WHERE balance_id=$1 AND contract_id=$2`, balanceId, contractId).Scan(&pending))
			})
			if !pending {
				t.Fatal("close lost the explicit pending metadata boundary")
			}
			assertContractPayoutTestBalanceConsumed(t, ctx, balanceId, contractId, 121)
			// Replay also covers reading completed metadata without a journal.
			assertContractPayoutTestBalanceConsumed(t, ctx, balanceId, contractId, 121)
		}
	})
}

// Zero and partial usage remain exact journal amounts while their original
// positive reservation and absent escrow metadata await the independent owner.
func TestContractPayoutBalanceReadPreservesZeroAndPartialDebit(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		for _, consumed := range []ByteCount{0, 37} {
			balanceId, contractId := newContractPayoutDebitReadTestFixture(t, ctx, false, consumed)
			before, err := readContractPayoutTestBalance(ctx, balanceId, contractId)
			if err != nil || before != (contractPayoutTestBalanceState{remaining: 121, consumed: consumed, pendingDebit: consumed, journalRows: 1}) {
				t.Fatalf("pending usage %d lost its exact journal authority: state=%+v err=%v", consumed, before, err)
			}
			applied, released, busy, err := flushTransferDebitBalance(ctx, balanceId)
			if err != nil || busy || applied != 1 || released != 1 {
				t.Fatalf("native debit application = %d/%d busy=%t err=%v", applied, released, busy, err)
			}
			after, err := readContractPayoutTestBalance(ctx, balanceId, contractId)
			if err != nil || after != (contractPayoutTestBalanceState{remaining: 121 - consumed, consumed: consumed}) {
				t.Fatalf("materialized usage %d differs from its journal: state=%+v err=%v", consumed, after, err)
			}
		}
	})
}

// Missing, partially written or contradictory authority must remain an error;
// permitting pending metadata must not convert every nullable payout to zero.
func TestContractPayoutBalanceReadRejectsIncompleteAuthority(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		for _, test := range []struct {
			name, statement string
			applied         bool
		}{
			{name: "missing journal", statement: `DELETE FROM transfer_debit_journal WHERE contract_id=$1`},
			{name: "applied journal without metadata", statement: `UPDATE transfer_debit_journal SET applied=true WHERE contract_id=$1`},
			{name: "non-native reservation", statement: `UPDATE transfer_escrow SET redis_reserved=false WHERE contract_id=$1`},
			{name: "settled without amount", statement: `UPDATE transfer_escrow SET settled=true WHERE contract_id=$1`},
			{name: "timestamp without amount", statement: `UPDATE transfer_escrow SET settle_time=clock_timestamp() WHERE contract_id=$1`},
			{name: "amount without settlement", statement: `UPDATE transfer_escrow SET payout_byte_count=0 WHERE contract_id=$1`},
			{name: "completed amount without timestamp", statement: `UPDATE transfer_escrow SET settled=true,payout_byte_count=121 WHERE contract_id=$1`, applied: true},
			{name: "materialized amount with unapplied journal", statement: `UPDATE transfer_escrow SET settled=true,settle_time=clock_timestamp(),payout_byte_count=121 WHERE contract_id=$1`},
			{name: "conflicting materialized amount", statement: `UPDATE transfer_escrow SET settled=true,settle_time=clock_timestamp(),payout_byte_count=120 WHERE contract_id=$1`, applied: true},
			{name: "debit exceeds reservation", statement: `UPDATE transfer_debit_journal SET debit_byte_count=122 WHERE contract_id=$1`},
		} {
			balanceId, contractId := newContractPayoutDebitReadTestFixture(t, ctx, false, 121)
			server.Tx(ctx, func(tx server.PgTx) {
				if test.applied {
					server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_debit_journal SET applied=true WHERE contract_id=$1`, contractId))
				}
				server.RaisePgResult(tx.Exec(ctx, test.statement, contractId))
			})
			if state, err := readContractPayoutTestBalance(ctx, balanceId, contractId); err == nil {
				t.Fatalf("%s was accepted as payout consumption: %+v", test.name, state)
			}
		}
	})
}

// An open reservation with stray journal data cannot masquerade as consumption.
// Terminal receipts are immutable, so this fixture never reopens a closed row.
func TestContractPayoutBalanceReadRequiresTerminalOutcome(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		escrow, err := CreateTransferEscrow(ctx, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId, 121)
		if err != nil || escrow == nil || escrow.TransferByteCount != 121 {
			t.Fatalf("create open native payout reservation: escrow=%v err=%v", escrow, err)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_debit_journal
				(contract_id,balance_id,debit_byte_count,shard) VALUES($1,$2,121,$3)`,
				escrow.ContractId, f.balanceId, transferDebitShard(f.balanceId)))
		})
		if state, err := readContractPayoutTestBalance(ctx, f.balanceId, escrow.ContractId); err == nil || err.Error() != "payout consumption has no terminal contract outcome" {
			t.Fatalf("open reservation did not fail at its missing outcome: state=%+v err=%v", state, err)
		}
	})
}
