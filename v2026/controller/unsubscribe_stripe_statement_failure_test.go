package controller

// UnsubscribeStripe raises a failed statement of its renewal query or of a
// renewal's close at once, with the statement's own error. Both used to keep
// the error and return normally from their transaction callbacks, which then
// went on to a commit that postgres rolled back.

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// Each case fails one statement: the renewal query (its table renamed away)
// or the close of a renewal whose Stripe subscription is already canceled (a
// test trigger on its update).
func TestUnsubscribeStripeRaisesStatementFailures(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fake := newNetworkRemoveStripeFake(t)

		exec := func(sql string) {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, sql))
			})
		}
		// The pool is reset after the rename: a query is prepared on its
		// connection's first use, and a failed prepare is returned by the query
		// call itself. A connection that had prepared the query already returns
		// the failure from the query's rows instead, which the read raises.
		tableUnavailable := func() func() {
			exec(`ALTER TABLE subscription_renewal RENAME TO subscription_renewal_forced_unavailable`)
			server.PgReset()
			return func() {
				exec(`ALTER TABLE subscription_renewal_forced_unavailable RENAME TO subscription_renewal`)
			}
		}
		updateFailures := func() func() {
			exec(`
				CREATE OR REPLACE FUNCTION forced_statement_failure() RETURNS trigger
				LANGUAGE plpgsql AS $$
				BEGIN
					RAISE EXCEPTION 'injected failure on % %', TG_OP, TG_TABLE_NAME;
				END
				$$
			`)
			exec(`
				CREATE TRIGGER forced_statement_failure
				BEFORE UPDATE ON subscription_renewal
				FOR EACH ROW EXECUTE FUNCTION forced_statement_failure()
			`)
			return func() {
				exec(`DROP TRIGGER forced_statement_failure ON subscription_renewal`)
			}
		}

		for i, c := range []struct {
			name  string
			force func() func()
			code  string
		}{
			{
				name:  "renewal query",
				force: tableUnavailable,
				code:  "42P01",
			},
			{
				name:  "renewal close",
				force: updateFailures,
				code:  "P0001",
			},
		} {
			networkId := server.NewId()
			userId := server.NewId()
			model.Testing_CreateNetwork(ctx, networkId, fmt.Sprintf("synthetic-unsubscribe-%d", i), userId)
			invoiceId := fmt.Sprintf("in_synthetic_unsubscribe_%d", i)
			subscriptionId := fmt.Sprintf("sub_synthetic_unsubscribe_%d", i)
			now := server.NowUtc()
			addNetworkRemoveStripeRenewal(t, ctx, networkId, invoiceId, now.Add(-time.Hour), now.Add(time.Hour))
			fake.add(invoiceId, subscriptionId, "canceled")

			restore := c.force()
			var err error
			panicValue := func() (panicValue any) {
				callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
				defer cancel()
				defer func() {
					panicValue = recover()
				}()
				err = UnsubscribeStripe(networkRemoveTestSession(callCtx, networkId, userId))
				return
			}()
			restore()

			panicErr, _ := panicValue.(error)
			var pgErr *pgconn.PgError
			switch {
			case !errors.As(panicErr, &pgErr) || pgErr.Code != c.code:
				t.Errorf("%s: the failed statement ended with %v (returned %v), want the %s failure raised", c.name, panicValue, err, c.code)
			case errors.Is(panicErr, pgx.ErrTxCommitRollback):
				t.Errorf("%s: ended in a commit that rolled back (%v), want the statement raised", c.name, panicValue)
			}
		}
	})
}
