package model

// A provider work savepoint that cannot be created raises, like any other
// failed statement: the transaction is aborted, its context canceled or its
// connection lost, so neither the optional work nor the caller's can go on.
// providerWorkOptionalSchemaInTx used to return false for it, the answer for
// an absent optional schema, and the caller went on: in an aborted
// transaction its commit then rolled back, and with a canceled context it
// committed without the optional work. Each call runs under
// forcedFailureCallTimeout (see forced_statement_failure_test.go).

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/urnetwork/server"
)

// Each case makes the savepoint fail in the caller's transaction, which must
// raise rather than return to the caller.
func TestProviderWorkOptionalSchemaRaisesFailedSavepoint(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		// whether the error, or an error it wraps, is the database error with
		// the code
		var hasPgCode func(err error, code string) bool
		hasPgCode = func(err error, code string) bool {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == code {
				return true
			}
			switch v := err.(type) {
			case interface{ Unwrap() []error }:
				for _, wrapped := range v.Unwrap() {
					if hasPgCode(wrapped, code) {
						return true
					}
				}
			case interface{ Unwrap() error }:
				return hasPgCode(v.Unwrap(), code)
			}
			return false
		}

		for _, c := range []struct {
			name string
			// fails the savepoint the call makes in the transaction, and
			// returns the context of the call
			prepare func(callCtx context.Context, tx server.PgTx) context.Context
			raised  func(panicErr error) bool
			want    string
		}{
			{
				name: "aborted transaction",
				prepare: func(callCtx context.Context, tx server.PgTx) context.Context {
					// a statement fails and is dropped, as a defective caller
					// would, which leaves the transaction aborted
					_, _ = tx.Exec(callCtx, `SELECT 1/0`)
					return callCtx
				},
				raised: func(panicErr error) bool {
					return hasPgCode(panicErr, "25P02") && hasPgCode(panicErr, "22012")
				},
				want: "the refused savepoint (25P02) carrying the dropped division by zero (22012)",
			},
			{
				name: "canceled context",
				prepare: func(callCtx context.Context, tx server.PgTx) context.Context {
					canceledCtx, cancel := context.WithCancel(callCtx)
					cancel()
					return canceledCtx
				},
				raised: func(panicErr error) bool {
					return errors.Is(panicErr, context.Canceled)
				},
				want: "the cancellation",
			},
		} {
			returned := false
			panicValue := callWithForcedFailure(ctx, func(callCtx context.Context) {
				server.Tx(callCtx, func(tx server.PgTx) {
					returned = false
					savepointCtx := c.prepare(callCtx, tx)
					providerWorkOptionalSchemaInTx(savepointCtx, tx, func(optional server.PgTx) error {
						return nil
					})
					returned = true
				})
			})
			panicErr, _ := panicValue.(error)
			switch {
			case returned:
				t.Errorf("%s: returned after its savepoint failed (then %v), want the failure raised", c.name, panicValue)
			case !c.raised(panicErr):
				t.Errorf("%s: ended with %v, want %s", c.name, panicValue, c.want)
			case errors.Is(panicErr, pgx.ErrTxCommitRollback):
				t.Errorf("%s: ended in a commit that rolled back (%v), want the savepoint's failure raised", c.name, panicValue)
			}
		}
	})
}
