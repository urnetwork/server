// Historical signed values must never wrap into spendable payer credit.
package model

import (
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server/v2026"
)

// The generated active column excludes negative balances before subtraction.
// Preserve this database boundary even for an old over-debit or bad import.
func TestNetEscrowAdmissionRejectsUnderflowingBalance(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		ctx := t.Context()
		for _, balance := range []ByteCount{math.MinInt64, math.MinInt64 + 599, -1, 0, 599, 600} {
			f := newNetEscrowOrderingTestFixture(t, ctx)
			createNetEscrowOrderingTestContract(ctx, f, 600)
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET balance_byte_count=$2 WHERE balance_id=$1`, f.balanceId, balance))
				contract, posts, err := createTransferEscrowInTx(ctx, tx,
					f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId,
					f.sourceNetworkId, 1, nil)
				if err == nil || !strings.Contains(err.Error(), "Insufficient balance") || contract != nil || len(posts) != 0 {
					t.Errorf("historical balance %d admitted nonexistent credit: contract=%+v posts=%d error=%v", balance, contract, len(posts), err)
				}
			}, server.TxReadCommitted)
			if reserved := openEscrowReservedForBalances(ctx, []server.Id{f.balanceId})[f.balanceId].reserved; reserved != 600 {
				t.Errorf("historical balance %d changed reservation to %d", balance, reserved)
			}
		}
	})
}

// Numeric SUM in PostgreSQL must reject totals outside bigint on decoding;
// a malformed overbooked history cannot wrap into a smaller reservation.
func TestNetEscrowAdmissionRejectsOutOfRangeReservationSum(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		first, _ := createNetEscrowOrderingTestContract(ctx, f, 600)
		second, _ := createNetEscrowOrderingTestContract(ctx, f, 400)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET balance_byte_count=$2,start_balance_byte_count=$2 WHERE balance_id=$1`, f.balanceId, ByteCount(math.MaxInt64)))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_escrow SET balance_byte_count=$2 WHERE contract_id=$1`, first.ContractId, ByteCount(math.MaxInt64)))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_escrow SET balance_byte_count=1 WHERE contract_id=$1`, second.ContractId))
		}, server.TxReadCommitted)
		var admissionErr error
		server.HandleError(func() {
			server.Tx(ctx, func(tx server.PgTx) {
				_, _, err := createTransferEscrowInTx(ctx, tx,
					f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId,
					f.sourceNetworkId, 1, nil)
				server.Raise(err)
			}, server.TxReadCommitted, server.OptNoRetry())
		}, func(err error) { admissionErr = err })
		if admissionErr == nil || !strings.Contains(admissionErr.Error(), "out of range") {
			t.Fatalf("oversized reservation sum was not rejected by checked decoding: %v", admissionErr)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var count int
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM transfer_escrow WHERE balance_id=$1`, f.balanceId).Scan(&count))
			if count != 2 {
				t.Fatalf("failed census admitted a new reservation: %d", count)
			}
		})
	})
}

// Admission stops at the requested capacity even when eligible grants sum
// above the storage limit, and settlement partitions exactly that capacity.
func TestNetEscrowSettlementMaximumAcrossGrantsDebitsExactly(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		now := server.NowUtc()
		AddBasicTransferBalance(ctx, f.sourceNetworkId, math.MaxInt64, now, now.Add(2*time.Hour))
		contract, _ := createNetEscrowOrderingTestContract(ctx, f, math.MaxInt64)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_close
				(contract_id,party,used_transfer_byte_count,close_time,checkpoint)
				VALUES($1,'source',$2,$3,false),($1,'destination',$2,$3,false)`, contract.ContractId, ByteCount(math.MaxInt64), now))
			_, closed, err := settleEscrowInTx(ctx, tx, contract.ContractId, ContractOutcomeSettled)
			server.Raise(err)
			if !closed {
				t.Fatal("maximum split-grant settlement did not close")
			}
			rows, err := tx.Query(ctx, `SELECT balance_id,balance_byte_count FROM transfer_balance WHERE network_id=$1`, f.sourceNetworkId)
			server.WithPgResult(rows, err, func() {
				count := 0
				for rows.Next() {
					var balanceId server.Id
					var remaining ByteCount
					server.Raise(rows.Scan(&balanceId, &remaining))
					want := ByteCount(1000)
					if balanceId == f.balanceId {
						want = 0
					}
					if remaining != want {
						t.Errorf("split grant remaining=%d want=%d", remaining, want)
					}
					count++
				}
				if count != 2 {
					t.Fatalf("split grant count=%d want=2", count)
				}
			})
		}, server.TxReadCommitted)
	})
}

// A negative early grant followed by a maximum grant formerly wrapped both
// used-minus-settled and the cumulative settled count into a false full debit.
func TestNetEscrowSettlementRejectsNegativeEscrowBeforeArithmetic(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		contract, _ := createNetEscrowOrderingTestContract(ctx, f, 700)
		now := server.NowUtc()
		AddBasicTransferBalance(ctx, f.sourceNetworkId, math.MaxInt64, now, now.Add(2*time.Hour))
		var laterBalanceId server.Id
		for _, balance := range GetActiveTransferBalances(ctx, f.sourceNetworkId) {
			if balance.BalanceId != f.balanceId {
				laterBalanceId = balance.BalanceId
			}
		}
		if laterBalanceId == (server.Id{}) {
			t.Fatal("missing synthetic later grant")
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_escrow SET balance_byte_count=-1 WHERE contract_id=$1`, contract.ContractId))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_escrow(contract_id,balance_id,balance_byte_count) VALUES($1,$2,$3)`, contract.ContractId, laterBalanceId, ByteCount(math.MaxInt64)))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_close
				(contract_id,party,used_transfer_byte_count,close_time,checkpoint)
				VALUES($1,'source',$2,$3,false),($1,'destination',$2,$3,false)`, contract.ContractId, ByteCount(math.MaxInt64), now))
			posts, closed, err := settleEscrowInTx(ctx, tx, contract.ContractId, ContractOutcomeSettled)
			if err == nil || !strings.Contains(err.Error(), "negative escrow byte count") || closed || len(posts) != 0 {
				t.Errorf("negative grant bypassed debit arithmetic: closed=%t posts=%d error=%v", closed, len(posts), err)
			}
			var outcome *ContractOutcome
			server.Raise(tx.QueryRow(ctx, `SELECT outcome FROM transfer_contract WHERE contract_id=$1`, contract.ContractId).Scan(&outcome))
			if outcome != nil {
				t.Errorf("malformed grant claimed terminal outcome: %s", *outcome)
			}
		}, server.TxReadCommitted)
	})
}

// PostgreSQL bigint arithmetic is checked; a historical underflow rolls back
// the outcome claim as well as the failed debit instead of leaving it terminal.
func TestNetEscrowSettlementDebitUnderflowRollsBack(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		contract, _ := createNetEscrowOrderingTestContract(ctx, f, 700)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET balance_byte_count=$2 WHERE balance_id=$1`, f.balanceId, ByteCount(math.MinInt64)))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_close
				(contract_id,party,used_transfer_byte_count,close_time,checkpoint)
				VALUES($1,'source',600,$2,false),($1,'destination',600,$2,false)`, contract.ContractId, server.NowUtc()))
		}, server.TxReadCommitted)
		var debitErr error
		server.HandleError(func() {
			server.Tx(ctx, func(tx server.PgTx) {
				_, _, err := settleEscrowInTx(ctx, tx, contract.ContractId, ContractOutcomeSettled)
				server.Raise(err)
			}, server.TxReadCommitted, server.OptNoRetry())
		}, func(err error) { debitErr = err })
		var pgErr *pgconn.PgError
		if !errors.As(debitErr, &pgErr) || pgErr.Code != "22003" {
			t.Fatalf("underflow error=%v, want checked PostgreSQL bigint failure", debitErr)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var outcome *ContractOutcome
			var balance ByteCount
			server.Raise(conn.QueryRow(ctx, `SELECT outcome FROM transfer_contract WHERE contract_id=$1`, contract.ContractId).Scan(&outcome))
			server.Raise(conn.QueryRow(ctx, `SELECT balance_byte_count FROM transfer_balance WHERE balance_id=$1`, f.balanceId).Scan(&balance))
			if outcome != nil || balance != math.MinInt64 {
				t.Fatalf("checked debit did not roll back ownership: outcome=%v balance=%d", outcome, balance)
			}
		})
		if reserved := openEscrowReservedForBalances(ctx, []server.Id{f.balanceId})[f.balanceId].reserved; reserved != 700 {
			t.Fatalf("failed debit released reservation: %d", reserved)
		}
	})
}
