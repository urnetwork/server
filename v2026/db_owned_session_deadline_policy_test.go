// An acknowledged rollback preserves a healthy borrowed session and caller
// custody while releasing only the failed scope's advisory-lock references.
package server

import (
	"context"
	"errors"
	"net"
	"testing"
)

// A deadline alone must not quarantine an idle session or replay business work.
func TestOwnedSessionCallbackDeadlineRetainsHealthyCallerSession(t *testing.T) {
	var networkError net.Error
	if !errors.As(context.DeadlineExceeded, &networkError) || !networkError.Timeout() ||
		!isConnectionError(context.DeadlineExceeded) || isConnectionError(errors.New("synthetic application refusal")) {
		t.Fatal("deadline policy discriminator does not match the existing error classifier")
	}
	ownedTransactionTestEnv(t, func(t testing.TB, ctx context.Context) {
		conn := RaisePgResult(AcquireMaintenanceDbConn(ctx))
		defer conn.Release()
		defer releaseOwnedTransactionTestKeys(ctx, conn)
		inherited := NewPgOwnershipKey("owned-deadline-policy", NewId())
		fresh := NewPgOwnershipKey("owned-deadline-policy", NewId())
		const execution int64 = 813797
		RaisePgResult(conn.Exec(ctx, `SELECT pg_advisory_lock($1::bigint),pg_advisory_lock($2::integer,$3::integer)`,
			execution, inherited.first, inherited.second))
		session := NewPgOwnedSession(conn)
		calls, posts := 0, 0
		var counter TxCommitCounter
		failure := captureDbErrorPanic(func() {
			session.Tx(ctx, []PgOwnershipKey{inherited, fresh}, func(tx PgTx) {
				calls++
				RaisePgResult(tx.Exec(ctx, `INSERT INTO owned_tx_effect VALUES(1,11)`))
				AddTxCommitCount(tx, &counter, 1)
				AddTxPostCommit(tx, "deadline-must-not-publish", func() any { posts++; return nil })
				panic(context.DeadlineExceeded)
			}, TxReadCommitted, OptNoRetry())
		})
		if failure != context.DeadlineExceeded || calls != 1 || posts != 0 || ctx.Err() != nil ||
			conn.Conn().IsClosed() || conn.Conn().PgConn().TxStatus() != 'I' || session.Err() != nil {
			t.Fatal("deadline did not retain its original cause and healthy idle session", failure, session.Err())
		}
		requireTxCommitSnapshot(t, &counter, 0, 0, 0)
		requirePgOwnershipHeld(t, ctx, conn.Conn().PgConn().PID(), inherited)
		Db(ctx, func(probe PgConn) {
			defer releaseOwnedTransactionTestKeys(ctx, probe)
			var rows int
			var executionFree, freshFree bool
			Raise(probe.QueryRow(ctx, `SELECT count(*) FROM owned_tx_effect`).Scan(&rows))
			Raise(probe.QueryRow(ctx, `SELECT pg_try_advisory_lock($1::bigint)`, execution).Scan(&executionFree))
			Raise(probe.QueryRow(ctx, `SELECT pg_try_advisory_lock($1::integer,$2::integer)`, fresh.first, fresh.second).Scan(&freshFree))
			if rows != 0 || executionFree || !freshFree {
				t.Fatal("deadline rollback left effects, lost caller custody or retained a scope-only reference")
			}
		}, OptNoRetry())
		reused := captureDbErrorPanic(func() {
			session.Tx(ctx, []PgOwnershipKey{inherited, fresh}, func(PgTx) { calls++ }, TxReadCommitted, OptNoRetry())
		})
		if reused != nil || session.Err() != nil || calls != 2 || posts != 0 {
			t.Fatal("healthy session refused deliberate reuse or replayed business work", reused, session.Err())
		}
		requireTxCommitSnapshot(t, &counter, 0, 0, 0)
		requirePgOwnershipHeld(t, ctx, conn.Conn().PgConn().PID(), inherited)
		// Every callback and publication has already joined. Only the caller
		// ends the retained ownership; the failing scope did not do so.
		releaseOwnedTransactionTestKeys(ctx, conn)
		Db(ctx, func(probe PgConn) {
			defer releaseOwnedTransactionTestKeys(ctx, probe)
			var executionFree, inheritedFree, freshFree bool
			Raise(probe.QueryRow(ctx, `SELECT pg_try_advisory_lock($1::bigint),
				pg_try_advisory_lock($2::integer,$3::integer),pg_try_advisory_lock($4::integer,$5::integer)`,
				execution, inherited.first, inherited.second, fresh.first, fresh.second).Scan(
				&executionFree, &inheritedFree, &freshFree))
			if !executionFree || !inheritedFree || !freshFree {
				t.Fatal("the caller's completed join did not release its retained custody")
			}
		}, OptNoRetry())
		t.Log("deadline_is_connection_class=true rollback_effects_absent=true session_open_idle=true scope_references_released=true custody_retained_until_caller_join=true callback_replayed=false deliberate_reuse=true")
	})
}
