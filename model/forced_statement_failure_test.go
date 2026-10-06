package model

// Forces a statement in a model transaction to fail, in each test's own
// database, to show that the failure surfaces at once with its own error.
// A transaction callback that returned normally after a failed statement
// ended in a commit that postgres turned into a rollback, and server.Tx
// retried that for its one-minute window before the call failed without the
// cause. Each forced call runs under forcedFailureCallTimeout, far inside that
// window, so a call that still holds its transaction fails at the deadline.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/urnetwork/server"
)

const forcedFailureCallTimeout = 10 * time.Second

// Fails every statement of the event (INSERT, UPDATE or DELETE) on the table
// with "injected failure on <event> <table>", until the returned func removes
// the failure.
func forceStatementFailures(ctx context.Context, table string, event string) (removeFailure func()) {
	sanitizedTable := pgx.Identifier{table}.Sanitize()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			CREATE OR REPLACE FUNCTION forced_statement_failure() RETURNS trigger
			LANGUAGE plpgsql AS $$
			BEGIN
				RAISE EXCEPTION 'injected failure on % %', TG_OP, TG_TABLE_NAME;
			END
			$$
			`,
		))
		server.RaisePgResult(tx.Exec(
			ctx,
			fmt.Sprintf(
				`
				CREATE TRIGGER forced_statement_failure
				BEFORE %s ON %s
				FOR EACH ROW EXECUTE FUNCTION forced_statement_failure()
				`,
				event,
				sanitizedTable,
			),
		))
	})
	return func() {
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				fmt.Sprintf(`DROP TRIGGER forced_statement_failure ON %s`, sanitizedTable),
			))
		})
	}
}

// Renames the table away, so that every statement on it, reads included,
// fails with undefined_table, until the returned func renames it back.
func forceTableUnavailable(ctx context.Context, table string) (restore func()) {
	sanitizedTable := pgx.Identifier{table}.Sanitize()
	sanitizedAway := pgx.Identifier{table + "_forced_unavailable"}.Sanitize()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, fmt.Sprintf(`ALTER TABLE %s RENAME TO %s`, sanitizedTable, sanitizedAway)))
	})
	return func() {
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, fmt.Sprintf(`ALTER TABLE %s RENAME TO %s`, sanitizedAway, sanitizedTable)))
		})
	}
}

// Runs the call under forcedFailureCallTimeout and returns what it panicked
// with.
func callWithForcedFailure(ctx context.Context, call func(callCtx context.Context)) (panicValue any) {
	callCtx, cancel := context.WithTimeout(ctx, forcedFailureCallTimeout)
	defer cancel()
	defer func() {
		panicValue = recover()
	}()
	call(callCtx)
	return
}

// Whether the panic is the database error with the code, and with the message
// when one is given.
func isForcedFailure(panicValue any, code string, message string) bool {
	panicErr, ok := panicValue.(error)
	if !ok {
		return false
	}
	var pgErr *pgconn.PgError
	return errors.As(panicErr, &pgErr) && pgErr.Code == code && (message == "" || pgErr.Message == message)
}
