package model

// More model writes that returned a failed statement's error to their
// transaction callback. The callback then returned normally, postgres turned
// its commit into a rollback, and the call failed as an aborted commit after
// the callback had gone on past the failure. Now the statement raises, which
// ends the transaction at once with its own error, or, where the callers log
// the error and go on, the call returns it after its transaction rolled back.
// Each forced call runs under forcedFailureCallTimeout (see
// forced_statement_failure_test.go).

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/urnetwork/server"
)

// Each case fails one statement of a write, which must raise that statement's
// error, not end in a commit that rolled back.
func TestModelWritesRaiseStatementFailuresAtOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		Testing_CreateNetwork(ctx, networkId, "raise-statement-failure", server.NewId())

		failing := func(table string, event string) func() func() {
			return func() func() {
				return forceStatementFailures(ctx, table, event)
			}
		}
		// Renames the column away, so that every statement naming it fails with
		// undefined_column while the others on the table still run. The pool is
		// then reset: a query is prepared on its connection's first use, and a
		// failed prepare is returned by the query call itself. A connection that
		// had prepared the query already returns the failure from the query's
		// rows instead, which every read raises.
		columnUnavailable := func(table string, column string) func() func() {
			sanitizedTable := pgx.Identifier{table}.Sanitize()
			sanitizedColumn := pgx.Identifier{column}.Sanitize()
			sanitizedAway := pgx.Identifier{column + "_forced_unavailable"}.Sanitize()
			return func() func() {
				server.Tx(ctx, func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(ctx, fmt.Sprintf(`ALTER TABLE %s RENAME COLUMN %s TO %s`, sanitizedTable, sanitizedColumn, sanitizedAway)))
				})
				server.PgReset()
				return func() {
					server.Tx(ctx, func(tx server.PgTx) {
						server.RaisePgResult(tx.Exec(ctx, fmt.Sprintf(`ALTER TABLE %s RENAME COLUMN %s TO %s`, sanitizedTable, sanitizedAway, sanitizedColumn)))
					})
				}
			}
		}
		userAuth := "raise-statement-failure@example.com"

		for _, c := range []struct {
			name  string
			force func() func()
			call  func(callCtx context.Context)
			code  string
		}{
			{
				name:  "AddOnboardingEvents",
				force: failing("network_onboarding_event", "INSERT"),
				call: func(callCtx context.Context) {
					AddOnboardingEvents(callCtx, []*OnboardingEvent{
						{
							NetworkId: networkId,
							Name:      "synthetic.event",
						},
					})
				},
				code: "P0001",
			},
			{
				name:  "IssueOnboardingOffer",
				force: failing("network_onboarding_offer", "INSERT"),
				call: func(callCtx context.Context) {
					IssueOnboardingOffer(callCtx, &IssueOnboardingOfferArgs{
						NetworkId: networkId,
						IssuedBy:  "synthetic",
						Surface:   "synthetic",
						Tier:      "synthetic",
						Validity:  time.Hour,
					})
				},
				code: "P0001",
			},
			{
				// the availability check reads the password table without
				// auth_type; the existing-auth-type query names it
				name:  "addUserAuth",
				force: columnUnavailable("network_user_auth_password", "auth_type"),
				call: func(callCtx context.Context) {
					addUserAuth(&AddUserAuthArgs{
						UserId:   server.NewId(),
						UserAuth: &userAuth,
						Verified: true,
					}, callCtx)
				},
				code: "42703",
			},
		} {
			restore := c.force()
			panicValue := callWithForcedFailure(ctx, c.call)
			restore()
			panicErr, _ := panicValue.(error)
			switch {
			case !isForcedFailure(panicValue, c.code, ""):
				t.Errorf("%s: the failed statement ended with %v, want the %s failure", c.name, panicValue, c.code)
			case errors.Is(panicErr, pgx.ErrTxCommitRollback):
				t.Errorf("%s: ended in a commit that rolled back (%v), want the statement raised", c.name, panicValue)
			}
		}
	})
}

// A failed insert of a reconciliation event rolls back its own transaction and
// is returned as the error: the callers log it and go on, since the audit
// trail must not turn a completed repair into a failed run. The insert's error
// used to be returned to the transaction callback, which went on to a commit
// that postgres rolled back, and the call panicked instead.
func TestPaymentReconciliationEventReturnsFailedInsert(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		evidence := "synthetic-reconciliation-evidence"
		newEvent := func() *PaymentReconciliationEvent {
			return &PaymentReconciliationEvent{
				RunId:    server.NewId(),
				Store:    SubscriptionMarketStripe,
				Action:   PaymentReconcileActionError,
				Evidence: evidence,
			}
		}

		restore := forceStatementFailures(ctx, "payment_reconciliation_event", "INSERT")
		var err error
		panicValue := callWithForcedFailure(ctx, func(callCtx context.Context) {
			err = AddPaymentReconciliationEvent(callCtx, newEvent())
		})
		var onceErr error
		added := false
		oncePanicValue := callWithForcedFailure(ctx, func(callCtx context.Context) {
			added, onceErr = AddPaymentReconciliationEventOnce(callCtx, newEvent())
		})
		restore()

		if panicValue != nil || !isForcedFailure(err, "P0001", "") {
			t.Errorf("a failed event insert ended with %v (panic %v), want the insert's failure returned", err, panicValue)
		}
		if oncePanicValue != nil || added || !isForcedFailure(onceErr, "P0001", "") {
			t.Errorf("a failed once-only event insert ended with added %t, %v (panic %v), want the insert's failure returned", added, onceErr, oncePanicValue)
		}
		if count := countRows(ctx, `SELECT COUNT(*) FROM payment_reconciliation_event WHERE evidence = $1`, evidence); count != 0 {
			t.Errorf("the failed inserts wrote %d events", count)
		}
	})
}
