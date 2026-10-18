// Caller-owned admission must not authorize business SQL from an older snapshot.
package server

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// The prior writer commits after a real repeatable-read snapshot exists but
// before admission. Its key is free; the stale caller must still be refused.
func TestTryTxOwnershipRejectsRepeatableReadBeforeSharedWrite(t *testing.T) {
	runTryTxOwnershipSnapshotControl(t, pgx.RepeatableRead, true)
}

// The same ordering with read committed observes the acknowledged prior value
// in the financial statement, without business overlap or transaction replay.
func TestTryTxOwnershipReadCommittedRefreshesAfterPriorOwner(t *testing.T) {
	runTryTxOwnershipSnapshotControl(t, pgx.ReadCommitted, false)
}

func runTryTxOwnershipSnapshotControl(t *testing.T, isolation pgx.TxIsoLevel, wantRefused bool) {
	ownedTransactionTestEnv(t, func(t testing.TB, ctx context.Context) {
		Tx(ctx, func(tx PgTx) {
			RaisePgResult(tx.Exec(ctx, `INSERT INTO owned_tx_effect VALUES(1,11)`))
		}, TxReadCommitted, OptNoRetry())
		var reruns, businessCalls atomic.Int32
		ctx = Testing_WithTxRerunHook(ctx, func() { reruns.Add(1) })
		key := NewPgOwnershipKey("owned-tx-snapshot-test", NewId())
		priorWritten, callerRead := make(chan struct{}), make(chan struct{})
		priorRelease, callerRelease := make(chan struct{}), make(chan struct{})
		var priorOnce, callerOnce sync.Once
		releasePrior := func() { priorOnce.Do(func() { close(priorRelease) }) }
		releaseCaller := func() { callerOnce.Do(func() { close(callerRelease) }) }
		prior := startOwnedTransactionTest(func() {
			OwnedTx(ctx, []PgOwnershipKey{key}, func(tx PgTx) {
				RaisePgResult(tx.Exec(ctx, `UPDATE owned_tx_effect SET amount=37 WHERE id=1`))
				close(priorWritten)
				select {
				case <-priorRelease:
				case <-ctx.Done():
					Raise(ctx.Err())
				}
			}, TxReadCommitted)
		})
		defer func() {
			releasePrior()
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			prior.join(t, cleanup)
		}()
		ownedTransactionTestAwait(t, ctx, priorWritten)
		var admissionErr error
		caller := startOwnedTransactionTest(func() {
			Tx(ctx, func(tx PgTx) {
				var amount int
				Raise(tx.QueryRow(ctx, `SELECT amount FROM owned_tx_effect WHERE id=1`).Scan(&amount))
				if amount != 11 {
					panic(errors.New("caller did not establish the required prior-version snapshot"))
				}
				close(callerRead)
				select {
				case <-callerRelease:
				case <-ctx.Done():
					Raise(ctx.Err())
				}
				admitted, err := TryTxOwnership(ctx, tx, []PgOwnershipKey{key})
				if err != nil {
					admissionErr = err
					return
				}
				if !admitted {
					panic(errors.New("prior owner did not release its acknowledged key"))
				}
				businessCalls.Add(1)
				RaisePgResult(tx.Exec(ctx, `UPDATE owned_tx_effect SET amount=amount+5 WHERE id=1`))
			}, isolation, OptNoRetry())
		})
		defer func() {
			releasePrior()
			releaseCaller()
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			caller.join(t, cleanup)
		}()
		ownedTransactionTestAwait(t, ctx, callerRead)
		releasePrior()
		if err := prior.join(t, ctx); err != nil {
			t.Fatal("prior owner did not acknowledge its commit and key release", err)
		}
		releaseCaller()
		outcome := caller.join(t, ctx)
		expectedAmount := 42
		if wantRefused {
			var pgErr *pgconn.PgError
			if cause, ok := outcome.(error); ok && errors.As(cause, &pgErr) && pgErr.Code == "40001" {
				t.Fatal("repeatable-read ownership admitted shared SQL from a stale snapshot (SQLSTATE 40001)")
			}
			if outcome != nil || admissionErr == nil || businessCalls.Load() != 0 {
				t.Fatal("repeatable-read caller was not refused before shared business SQL", outcome)
			}
			expectedAmount = 37
		} else if outcome != nil || admissionErr != nil || businessCalls.Load() != 1 {
			t.Fatal("read-committed owner failed its fresh exact update", outcome, admissionErr)
		}
		if reruns.Load() != 0 {
			t.Fatal("snapshot admission automatically replayed a transaction")
		}
		Db(ctx, func(conn PgConn) {
			var amount int
			Raise(conn.QueryRow(ctx, `SELECT amount FROM owned_tx_effect WHERE id=1`).Scan(&amount))
			if amount != expectedAmount {
				t.Fatal("snapshot ownership lost or repeated an exact business amount")
			}
		}, OptNoRetry())
	})
}
