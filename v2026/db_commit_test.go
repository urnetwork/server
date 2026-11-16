package server

// A transaction whose body has succeeded must commit even if the caller's
// context is cancelled. See the commit in `txWithPool`.

import (
	"context"
	"testing"
)

// TestTxCommitSurvivesCallerCancel is the regression test for lost post-commit
// work.
//
// `tx.Commit` used to run on the caller's context. A client disconnecting at
// the wrong moment made pgx abandon the commit round trip while postgres
// committed anyway, and the ambiguous result was raised as `Done` — so the
// caller never ran the work that follows a successful commit (redis mirror
// updates, stats, notifications) even though the data was durable. The
// observed damage was a contract's escrow rows committed while its net escrow
// counter increment was dropped, leaving the counter permanently short (see
// reportNegativeNetEscrow).
//
// Without the fix this panics out of `Tx` with `Done` and the row is absent.
func TestTxCommitSurvivesCallerCancel(t *testing.T) {
	(&TestEnv{ApplyDbMigrations: true}).Run(t, func(t testing.TB) {
		ctx := context.Background()

		Db(ctx, func(conn PgConn) {
			RaisePgResult(conn.Exec(
				ctx,
				`CREATE TABLE test_commit_cancel (id uuid PRIMARY KEY)`,
			))
		}, OptReadWrite())

		id := NewId()
		cancelCtx, cancel := context.WithCancel(ctx)

		// the caller goes away while the transaction body is completing: the
		// work is done, only the commit round trip remains
		Tx(cancelCtx, func(tx PgTx) {
			RaisePgResult(tx.Exec(
				ctx,
				`INSERT INTO test_commit_cancel (id) VALUES ($1)`,
				id,
			))
			cancel()
		})

		// the transaction body succeeded, so the row must be durable — and Tx
		// must have returned normally, so a caller can run its post-commit work
		found := false
		Db(ctx, func(conn PgConn) {
			result, err := conn.Query(
				ctx,
				`SELECT id FROM test_commit_cancel WHERE id = $1`,
				id,
			)
			WithPgResult(result, err, func() {
				found = result.Next()
			})
		})
		if !found {
			t.Fatal("the committed row is missing: the commit was abandoned with the caller's context, so any post-commit work would also have been skipped")
		}
	})
}
