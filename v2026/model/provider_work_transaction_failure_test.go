package model

// Required endpoint work raises an aborted or canceled owner's original
// failure before another statement or a commit can replace its cause.

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/urnetwork/server/v2026"
)

// Each case makes the actual fence fail in the caller's transaction, which must
// raise rather than return to the caller.
func TestProviderWorkSessionMutationRaisesOwnerFailure(t *testing.T) {
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
			// fails the fence the call makes in the transaction, and
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
				want: "the refused fence (25P02) carrying the dropped division by zero (22012)",
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
					fenceCtx := c.prepare(callCtx, tx)
					providerWorkLockCurrentSessionMutationInTx(fenceCtx, tx, server.NewId())
					returned = true
				}, server.OptNoRetry())
			})
			panicErr, _ := panicValue.(error)
			switch {
			case returned:
				t.Errorf("%s: returned after its fence failed (then %v), want the failure raised", c.name, panicValue)
			case !c.raised(panicErr):
				t.Errorf("%s: ended with %v, want %s", c.name, panicValue, c.want)
			case errors.Is(panicErr, pgx.ErrTxCommitRollback):
				t.Errorf("%s: ended in a commit that rolled back (%v), want the fence's failure raised", c.name, panicValue)
			}
		}
	})
}
