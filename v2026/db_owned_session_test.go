// Native session-reference controls distinguish borrowed scope cleanup from
// releasing an execution/group owner or replaying an uncertain transaction.
package server

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestOwnedSessionPreservesInheritedReferencesThroughCommitPost(t *testing.T) {
	ownedTransactionTestEnv(t, func(t testing.TB, ctx context.Context) {
		conn := RaisePgResult(AcquireMaintenanceDbConn(ctx))
		defer conn.Release()
		defer releaseOwnedTransactionTestKeys(ctx, conn)
		inherited := NewPgOwnershipKey("owned-session-test", NewId())
		fresh := NewPgOwnershipKey("owned-session-test", NewId())
		const execution int64 = 813795
		RaisePgResult(conn.Exec(ctx, `SELECT pg_advisory_lock($1::bigint),pg_advisory_lock($2::integer,$3::integer)`,
			execution, inherited.first, inherited.second))
		session := NewPgOwnedSession(conn)
		requestCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		calls, posts := 0, 0
		var counter TxCommitCounter
		session.Tx(requestCtx, []PgOwnershipKey{inherited, fresh, inherited}, func(tx PgTx) {
			calls++
			if tx.Conn() != conn.Conn() || !TxOwnsKeys(tx, []PgOwnershipKey{inherited, fresh}) {
				t.Fatal("borrowed transaction changed its exact backend or ownership")
			}
			RaisePgResult(tx.Exec(requestCtx, `INSERT INTO owned_tx_effect VALUES(1,11)`))
			AddTxCommitCount(tx, &counter, 1)
			AddTxPostCommit(tx, "owned-session-post", func() any {
				posts++
				Db(ctx, func(probe PgConn) {
					defer releaseOwnedTransactionTestKeys(ctx, probe)
					var executionFree, inheritedFree, freshFree bool
					Raise(probe.QueryRow(ctx, `SELECT pg_try_advisory_lock($1::bigint),
						pg_try_advisory_lock($2::integer,$3::integer),pg_try_advisory_lock($4::integer,$5::integer)`,
						execution, inherited.first, inherited.second, fresh.first, fresh.second).Scan(
						&executionFree, &inheritedFree, &freshFree))
					if executionFree || inheritedFree || !freshFree {
						t.Fatal("scope cleanup released an inherited owner or retained its fresh reference")
					}
				}, OptNoRetry())
				return nil
			})
			cancel()
		}, TxReadCommitted, OptRetryDefault())
		if calls != 1 || posts != 1 || session.Err() != nil || conn.Conn().IsClosed() {
			t.Fatal("borrowed scope changed committed callback/post/lifetime", calls, posts, session.Err())
		}
		requireTxCommitSnapshot(t, &counter, 1, 0, 0)
		// One original reference must remain, not zero or a leaked duplicate.
		var released bool
		Raise(conn.QueryRow(ctx, `SELECT pg_advisory_unlock($1::integer,$2::integer)`, inherited.first, inherited.second).Scan(&released))
		if !released {
			t.Fatal("the caller lost its original group reference")
		}
		Db(ctx, func(probe PgConn) {
			defer releaseOwnedTransactionTestKeys(ctx, probe)
			var available bool
			Raise(probe.QueryRow(ctx, `SELECT pg_try_advisory_lock($1::integer,$2::integer)`, inherited.first, inherited.second).Scan(&available))
			if !available {
				t.Fatal("borrowed scope leaked a reentrant reference")
			}
		}, OptNoRetry())
	})
}

func TestOwnedSessionBusyLastChunkReleasesOnlyItsReferences(t *testing.T) {
	ownedTransactionTestEnv(t, func(t testing.TB, ctx context.Context) {
		keys := make([]PgOwnershipKey, pgOwnershipQueryLimit+7)
		for index := range keys {
			keys[index] = NewPgOwnershipKey("owned-session-test", NewId())
		}
		keys = normalizePgOwnershipKeys(keys)
		conn := RaisePgResult(AcquireMaintenanceDbConn(ctx))
		defer conn.Release()
		defer releaseOwnedTransactionTestKeys(ctx, conn)
		inherited, busy := keys[0], keys[len(keys)-1]
		RaisePgResult(conn.Exec(ctx, `SELECT pg_advisory_lock($1::integer,$2::integer)`, inherited.first, inherited.second))
		session := NewPgOwnedSession(conn)
		Db(ctx, func(blocker PgConn) {
			defer releaseOwnedTransactionTestKeys(ctx, blocker)
			RaisePgResult(blocker.Exec(ctx, `SELECT pg_advisory_lock($1::integer,$2::integer)`, busy.first, busy.second))
			requestCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			waiting, calls := 0, 0
			requestCtx = Testing_WithPgOwnershipObservation(requestCtx, func(event PgOwnershipEvent) {
				if event.Kind != PgOwnershipWaiting {
					return
				}
				waiting++
				var idle bool
				Raise(blocker.QueryRow(ctx, `SELECT xact_start IS NULL FROM pg_stat_activity WHERE pid=$1`, conn.Conn().PgConn().PID()).Scan(&idle))
				if !idle || calls != 0 {
					t.Fatal("busy borrowed admission entered a business transaction")
				}
				for index, key := range keys[:len(keys)-1] {
					var available bool
					Raise(blocker.QueryRow(ctx, `SELECT pg_try_advisory_lock($1::integer,$2::integer)`, key.first, key.second).Scan(&available))
					if available == (index == 0) {
						t.Fatal("busy scope lost an inherited reference or retained a partial fresh key")
					}
					if available {
						RaisePgResult(blocker.Exec(ctx, `SELECT pg_advisory_unlock($1::integer,$2::integer)`, key.first, key.second))
					}
				}
				cancel()
			})
			failure, ok := captureDbErrorPanic(func() {
				session.Tx(requestCtx, append(slices.Clone(keys), inherited), func(PgTx) { calls++ }, TxReadCommitted)
			}).(error)
			if !ok || !errors.Is(failure, context.Canceled) || waiting != 1 || calls != 0 || session.Err() != nil {
				t.Fatal("known busy scope became uncertain or executed business", failure, waiting, calls, session.Err())
			}
		}, OptNoRetry())
		session.Tx(ctx, keys, func(tx PgTx) { RaisePgResult(tx.Exec(ctx, `INSERT INTO owned_tx_effect VALUES(1,11)`)) }, TxReadCommitted)
		if session.Err() != nil {
			t.Fatal("released admission could not reuse the caller's live session", session.Err())
		}
	})
}

func TestOwnedSessionStatementRollbackDoesNotReplayOrReleaseCaller(t *testing.T) {
	ownedTransactionTestEnv(t, func(t testing.TB, ctx context.Context) {
		conn := RaisePgResult(AcquireMaintenanceDbConn(ctx))
		defer conn.Release()
		defer releaseOwnedTransactionTestKeys(ctx, conn)
		key := NewPgOwnershipKey("owned-session-test", NewId())
		RaisePgResult(conn.Exec(ctx, `SELECT pg_advisory_lock($1::integer,$2::integer)`, key.first, key.second))
		session := NewPgOwnedSession(conn)
		calls, posts := 0, 0
		var counter TxCommitCounter
		failure, ok := captureDbErrorPanic(func() {
			session.Tx(ctx, []PgOwnershipKey{key}, func(tx PgTx) {
				calls++
				RaisePgResult(tx.Exec(ctx, `INSERT INTO owned_tx_effect VALUES(1,11)`))
				AddTxCommitCount(tx, &counter, 1)
				AddTxPostCommit(tx, "rolled-back-session", func() any { posts++; return nil })
				RaisePgResult(tx.Exec(ctx, `DO $$ BEGIN RAISE EXCEPTION 'synthetic scope refusal' USING ERRCODE='40001'; END $$`))
			}, TxReadCommitted, OptRetryDefault())
		}).(error)
		var pgErr *pgconn.PgError
		if !ok || !errors.As(failure, &pgErr) || pgErr.Code != "40001" || calls != 1 || posts != 0 || session.Err() != nil {
			t.Fatal("borrowed rollback replayed or lost its primary outcome", failure, calls, posts, session.Err())
		}
		requireTxCommitSnapshot(t, &counter, 0, 0, 0)
		requirePgOwnershipHeld(t, ctx, conn.Conn().PgConn().PID(), key)
		session.Tx(ctx, []PgOwnershipKey{key}, func(tx PgTx) {
			RaisePgResult(tx.Exec(ctx, `INSERT INTO owned_tx_effect VALUES(1,37)`))
		}, TxReadCommitted)
	})
}
