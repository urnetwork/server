// Same-key refusals happen before financial SQL while independent payers work.
package model

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// The direct owner is admitted on its transaction's backend. Positive refusal
// observations in each caller prove exclusion; cleanup always joins its end.
func holdTransferBalanceTestOwner(t testing.TB, ctx context.Context, ids []server.Id) func() {
	t.Helper()
	return holdFinancialTestOwner(t, ctx, transferBalanceOwnershipKeys(ids))
}

func holdFinancialTestOwner(t testing.TB, ctx context.Context, keys []server.PgOwnershipKey) func() {
	t.Helper()
	ready, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		var returnErr error
		server.HandleError(func() {
			server.OwnedTx(ctx, keys, func(tx server.PgTx) {
				close(ready)
				select {
				case <-release:
				case <-ctx.Done():
					server.Raise(ctx.Err())
				}
			}, server.TxReadCommitted, server.OptNoRetry())
		}, func(err error) { returnErr = err })
		done <- returnErr
	}()
	var once sync.Once
	finish := func() {
		once.Do(func() {
			close(release)
			select {
			case err := <-done:
				if err != nil {
					t.Error("balance ownership fixture failed to join", err)
				}
			case <-time.After(10 * time.Second):
				t.Error("balance ownership fixture exceeded bounded cleanup")
			}
		})
	}
	t.Cleanup(finish)
	select {
	case <-ready:
	case err := <-done:
		once.Do(func() { close(release) })
		t.Fatal("balance ownership fixture never entered its transaction", err)
	case <-ctx.Done():
		t.Fatal("balance ownership fixture admission expired", ctx.Err())
	}
	return finish
}

// Retirement and a delayed metadata post must use the same owner as debit;
// refused cleanup retains both rows and its input cursor for the next turn.
func TestTransferBalanceOwnerCoversMetadataAndRetention(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		var reruns atomic.Int32
		ctx = server.Testing_WithTxRerunHook(ctx, func() { reruns.Add(1) })
		f := newNetEscrowOrderingTestFixture(t, ctx)
		// Its second final settles the legacy reservation in its own close.
		contract := newLegacyReservationTestContract(t, ctx, f, server.NewId(), 100, 11)
		requireLegacySettlementTestState(t, ctx, f, contract, true, 989, 0)
		expired := newNetEscrowOrderingTestFixture(t, ctx)
		orphan := newNetEscrowOrderingTestFixture(t, ctx)
		orphanId := server.Id{15: 1}
		now := server.NowUtc()
		server.Tx(ctx, func(tx server.PgTx) {
			// Preserve the terminal usage/debit while modeling a lost legacy
			// metadata post and an already eligible historical retention row.
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract
 SET create_time=$2::timestamp-interval '301 days',reap_time=$2::timestamp-interval '1 day'
 WHERE contract_id=$1`, contract, now))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_escrow SET settled=false WHERE contract_id=$1`, contract))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET end_time=$2::timestamp-interval '8 days' WHERE balance_id=$1`, expired.balanceId, now))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_escrow(contract_id,balance_id,balance_byte_count) VALUES($1,$2,17)`, orphanId, orphan.balanceId))
		}, server.TxReadCommitted, server.OptNoRetry())
		ids := []server.Id{f.balanceId, expired.balanceId, orphan.balanceId}
		release := holdTransferBalanceTestOwner(t, ctx, ids)
		defer release()
		var refused atomic.Int32
		observed := server.Testing_WithPgOwnershipObservation(ctx, func(event server.PgOwnershipEvent) {
			if event.Kind == server.PgOwnershipRefused {
				refused.Add(1)
			}
		})
		var metadataErr error
		server.HandleError(func() {
			server.Tx(observed, func(tx server.PgTx) {
				settleEscrowMetadataInTx(observed, tx, contract, now, map[server.Id]sweepPayout{
					f.balanceId: {escrowBalanceByteCount: 100, payoutByteCount: 11},
				})
			}, server.TxReadCommitted, server.OptNoRetry())
		}, func(err error) { metadataErr = err })
		if !errors.Is(metadataErr, errTransferBalanceOwnershipBusy) || refused.Load() != 1 {
			t.Fatal("metadata entered a held balance owner", refused.Load(), metadataErr)
		}
		removeCompletedTransferBalanceBatch(observed, []server.Id{expired.balanceId}, now.Add(-7*24*time.Hour))
		if refused.Load() != 2 {
			t.Fatal("grant retention omitted common ownership", refused.Load())
		}
		removeDueContractBatches(observed, now, now.Add(-300*24*time.Hour), 1)
		if refused.Load() != 3 {
			t.Fatal("contract retention omitted escrow revision ownership", refused.Load())
		}
		removed, cursor, done := SweepOrphanContractData(observed, SweepOrphanCursor{Step: 1}, 1, 1)
		if removed != 0 || cursor.Step != 1 || len(cursor.Key) != 0 || done || refused.Load() != 4 {
			t.Fatal("orphan refusal lost its row or advanced the refused input cursor", removed, cursor, done, refused.Load())
		}
		server.Db(ctx, func(conn server.PgConn) {
			var exact bool
			server.Raise(conn.QueryRow(ctx, `SELECT
 EXISTS(SELECT 1 FROM transfer_escrow WHERE contract_id=$1 AND balance_id=$2 AND NOT settled)
 AND EXISTS(SELECT 1 FROM transfer_contract WHERE contract_id=$1 AND outcome='settled')
 AND EXISTS(SELECT 1 FROM transfer_balance WHERE balance_id=$3)
 AND EXISTS(SELECT 1 FROM transfer_escrow WHERE contract_id=$4 AND balance_id=$5)
 AND (SELECT balance_byte_count FROM transfer_balance WHERE balance_id=$2)=989`,
				contract, f.balanceId, expired.balanceId, orphanId, orphan.balanceId).Scan(&exact))
			if !exact {
				t.Fatal("refused retirement mutated held financial custody")
			}
		})
		release()
		removeCompletedTransferBalanceBatch(ctx, []server.Id{expired.balanceId}, now.Add(-7*24*time.Hour))
		removeDueContractBatches(ctx, now, now.Add(-300*24*time.Hour), 1)
		removed, _, _ = SweepOrphanContractData(ctx, cursor, 1, 1)
		if removed != 1 {
			t.Fatal("retained orphan cursor did not resume exactly once", removed)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var exact bool
			server.Raise(conn.QueryRow(ctx, `SELECT
 NOT EXISTS(SELECT 1 FROM transfer_contract WHERE contract_id=$1)
 AND NOT EXISTS(SELECT 1 FROM transfer_escrow WHERE contract_id=ANY($2))
 AND NOT EXISTS(SELECT 1 FROM transfer_balance WHERE balance_id=$3)
 AND EXISTS(SELECT 1 FROM transfer_balance_net_escrow_revision WHERE balance_id=$3 AND revision>0)
 AND (SELECT balance_byte_count FROM transfer_balance WHERE balance_id=$4)=989
 AND (SELECT balance_byte_count FROM transfer_balance WHERE balance_id=$5)=1000`,
				contract, []server.Id{contract, orphanId}, expired.balanceId, f.balanceId, orphan.balanceId).Scan(&exact))
			if !exact || reruns.Load() != 0 {
				t.Fatal("resumed cleanup changed debit or failed its tombstone/cursor custody", reruns.Load())
			}
		})
	})
}
