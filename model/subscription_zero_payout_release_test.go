// The deadline close of an unused Redis contract charges nothing, but it must
// still release its reservation when the release after its commit is lost. Its
// commit journals a zero debit, so the debit worker releases the reservation as
// it does for a paid close.
package model

import (
	"context"
	"errors"
	"testing"

	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/server"
)

// Reads a balance's reserved total and one contract's token amount.
func zeroPayoutReservation(t testing.TB, ctx context.Context, balanceId, contractId server.Id) (total, token int64) {
	t.Helper()
	keys := redisContractReservationKeys(balanceId)
	server.Redis(ctx, func(r server.RedisClient) {
		value, err := r.Get(ctx, keys[0]).Int64()
		if !errors.Is(err, redis.Nil) {
			server.Raise(err)
			total = value
		}
		value, err = r.HGet(ctx, keys[1], contractId.String()).Int64()
		if !errors.Is(err, redis.Nil) {
			server.Raise(err)
			token = value
		}
	})
	return
}

// The deadline close of an unused Redis contract charges nothing. With every
// post lost after its commit, the debit worker still releases the reservation
// and leaves the payer's credit and the journal exactly as before the contract.
func TestDeadlineCloseZeroPayoutReleaseSurvivesLostPost(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		escrow := createRedisAdmissionTest(ctx, f, 100)
		if total, token := zeroPayoutReservation(t, ctx, f.balanceId, escrow.ContractId); total != 100 || token != 100 {
			t.Fatal("fixture did not reserve its Redis credit", total, token)
		}
		readCredit := func() (credit ByteCount, journals int) {
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT
					(SELECT balance_byte_count FROM transfer_balance WHERE balance_id=$1),
					(SELECT count(*) FROM transfer_debit_journal WHERE contract_id=$2)`, f.balanceId, escrow.ContractId).Scan(&credit, &journals))
			})
			return
		}
		before, _ := readCredit()
		keys := deadlineFinancialOwnershipKeys(escrow.ContractId, readDeadlineBalanceIds(ctx, escrow.ContractId))
		var result *ContractDeadlineReconciliation
		server.OwnedTx(ctx, keys, func(tx server.PgTx) {
			// The process stops right after this commit, so every post is lost.
			result, _ = reconcileContractAtDeadlineWithOwnershipInTx(ctx, tx, escrow.ContractId, server.NowUtc(), true)
		}, server.TxReadCommitted, server.OptNoRetry())
		if result == nil || result.AlreadyClosed || result.Missing || result.Charged != 0 {
			t.Fatalf("unused contract did not close at its deadline without a charge: %+v", result)
		}
		if total, token := zeroPayoutReservation(t, ctx, f.balanceId, escrow.ContractId); total != 100 || token != 100 {
			t.Fatal("the lost post's release happened anyway", total, token)
		}
		applied, released, busy, err := flushTransferDebitBalance(ctx, f.balanceId)
		if err != nil || busy || applied != 1 || released != 1 {
			t.Fatal("debit worker did not drain the zero-payout close", applied, released, busy, err)
		}
		if total, token := zeroPayoutReservation(t, ctx, f.balanceId, escrow.ContractId); total != 0 || token != 0 {
			t.Fatal("reservation stayed held after recovery", total, token)
		}
		if after, journals := readCredit(); after != before || journals != 0 {
			t.Fatal("zero-payout recovery charged the payer or kept its journal", before, after, journals)
		}
	})
}
