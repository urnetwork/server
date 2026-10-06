package model

// The shared payment helpers, and the other model writes that dropped or
// returned a failed statement's error, now raise it at the statement (see
// forced_statement_failure_test.go). A returned statement error left the
// caller's transaction callback to return normally: postgres turned its commit
// into a rollback, which server.Tx retried for a minute before the call failed
// without the cause, and which it now fails at once as an aborted commit. A
// raise ends the callback at the failed statement instead, with that
// statement's own error.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/session"
)

// Each case fails one statement. An in-transaction helper must raise its
// statement's error rather than return it to the callback; a top-level call
// must end with that error, not with a commit that rolled back.
func TestPaymentAndModelWritesRaiseStatementFailures(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId := server.NewId()
		userId := server.NewId()
		Testing_CreateNetwork(ctx, networkId, "statement-failure", userId)
		byJwt := &jwt.ByJwt{
			NetworkId: networkId,
			UserId:    userId,
		}
		referrerNetworkId := server.NewId()
		Testing_CreateNetwork(ctx, referrerNetworkId, "statement-failure-referrer", server.NewId())
		referralCode := CreateNetworkReferralCode(ctx, referrerNetworkId)

		// a legacy account the user-auth migration picks up: a wallet on
		// network_user and no child auth rows
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`
				INSERT INTO network_user (user_id, user_name, auth_type, wallet_address, wallet_blockchain)
				VALUES ($1, 'legacy-wallet', $2, 'SyntheticLegacyWallet1111111111111111111111', 'SOL')
				`,
				server.NewId(),
				AuthTypeSolana,
			))
		})

		startTime := server.NowUtc()
		endTime := startTime.Add(time.Hour)

		unavailable := func(table string) func() func() {
			return func() func() {
				return forceTableUnavailable(ctx, table)
			}
		}
		failing := func(table string, event string) func() func() {
			return func() func() {
				return forceStatementFailures(ctx, table, event)
			}
		}
		// holds the purchase token's advisory lock on another connection
		// until the returned func releases it
		holdingPurchaseLock := func(purchaseToken string) func() func() {
			return func() func() {
				held := make(chan struct{})
				release := make(chan struct{})
				done := make(chan struct{})
				go func() {
					defer close(done)
					server.Tx(ctx, func(tx server.PgTx) {
						server.RaisePgResult(tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, purchaseToken))
						close(held)
						<-release
					})
				}()
				<-held
				return func() {
					close(release)
					<-done
				}
			}
		}

		for _, c := range []struct {
			name  string
			force func() func()
			// an in-transaction helper, called the way the credit paths call
			// it; nil for a top-level call
			helper func(tx server.PgTx, callCtx context.Context) error
			call   func(callCtx context.Context)
			code   string
		}{
			{
				name:  "LockPaymentNetworkInTx",
				force: unavailable("network"),
				helper: func(tx server.PgTx, callCtx context.Context) error {
					return LockPaymentNetworkInTx(tx, callCtx, networkId)
				},
				code: "42P01",
			},
			{
				name:  "LockPlaySubscriptionPurchaseInTx",
				force: holdingPurchaseLock("synthetic-purchase-token"),
				helper: func(tx server.PgTx, callCtx context.Context) error {
					// the lock wait fails at once on the held lock
					server.RaisePgResult(tx.Exec(callCtx, `SET LOCAL lock_timeout = '50ms'`))
					return LockPlaySubscriptionPurchaseInTx(tx, callCtx, networkId, "synthetic-purchase-token")
				},
				code: "55P03",
			},
			{
				name:  "AddSubscriptionRenewalInTx",
				force: failing("subscription_renewal", "INSERT"),
				helper: func(tx server.PgTx, callCtx context.Context) error {
					return AddSubscriptionRenewalInTx(tx, callCtx, &SubscriptionRenewal{
						NetworkId:          networkId,
						SubscriptionType:   SubscriptionTypeSupporter,
						StartTime:          startTime,
						EndTime:            endTime,
						SubscriptionMarket: SubscriptionMarketX402,
						TransactionId:      "synthetic-transaction",
					})
				},
				code: "P0001",
			},
			{
				name:  "AddBasicTransferBalanceInTx",
				force: failing("transfer_balance", "INSERT"),
				helper: func(tx server.PgTx, callCtx context.Context) error {
					return AddBasicTransferBalanceInTx(tx, callCtx, networkId, 1024, startTime, endTime)
				},
				code: "P0001",
			},
			{
				name:  "AddProTransferBalanceInTx",
				force: failing("transfer_balance", "INSERT"),
				helper: func(tx server.PgTx, callCtx context.Context) error {
					return AddProTransferBalanceInTx(tx, callCtx, networkId, 1024, startTime, endTime)
				},
				code: "P0001",
			},
			{
				name:  "AddGrantTransferBalanceInTx",
				force: failing("transfer_balance", "INSERT"),
				helper: func(tx server.PgTx, callCtx context.Context) error {
					return AddGrantTransferBalanceInTx(tx, callCtx, networkId, GrantKindFree, 1024, startTime, endTime)
				},
				code: "P0001",
			},
			{
				name:  "MarkPaymentIntentCompletedInTx",
				force: unavailable("solana_payment_intent"),
				helper: func(tx server.PgTx, callCtx context.Context) error {
					_, err := MarkPaymentIntentCompletedInTx(tx, "synthetic-reference", "synthetic-signature", session.Testing_CreateClientSession(callCtx, byJwt))
					return err
				},
				code: "42P01",
			},
			{
				name:  "SubscriptionCreatePaymentId",
				force: failing("subscription_payment", "INSERT"),
				call: func(callCtx context.Context) {
					SubscriptionCreatePaymentId(&SubscriptionCreatePaymentIdArgs{}, session.Testing_CreateClientSession(callCtx, byJwt))
				},
				code: "P0001",
			},
			{
				name:  "CreateNetworkReferral",
				force: failing("network_referral", "INSERT"),
				call: func(callCtx context.Context) {
					CreateNetworkReferral(callCtx, networkId, referralCode.ReferralCode)
				},
				code: "P0001",
			},
			{
				name:  "MigrateNetworkUserChildAuths",
				force: failing("network_user_auth_wallet", "INSERT"),
				call: func(callCtx context.Context) {
					MigrateNetworkUserChildAuths(callCtx)
				},
				code: "P0001",
			},
		} {
			helperReturned := false
			call := c.call
			if c.helper != nil {
				call = func(callCtx context.Context) {
					server.Tx(callCtx, func(tx server.PgTx) {
						helperReturned = false
						// as the credit paths do, a returned error ends the
						// callback normally
						_ = c.helper(tx, callCtx)
						helperReturned = true
					})
				}
			}
			restore := c.force()
			panicValue := callWithForcedFailure(ctx, call)
			restore()
			panicErr, _ := panicValue.(error)
			switch {
			case !isForcedFailure(panicValue, c.code, ""):
				t.Errorf("%s: the failed statement ended with %v, want the %s failure", c.name, panicValue, c.code)
			case helperReturned:
				t.Errorf("%s: the helper returned after its failed statement (%v), want it raised", c.name, panicValue)
			case errors.Is(panicErr, pgx.ErrTxCommitRollback):
				t.Errorf("%s: ended in a commit that rolled back (%v), want the statement raised", c.name, panicValue)
			}
		}
	})
}
