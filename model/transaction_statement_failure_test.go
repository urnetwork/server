package model

// Model writes that used to go on after a failed statement now raise it, so
// the failure surfaces at once with its own error (see
// forced_statement_failure_test.go).

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server"

	"github.com/urnetwork/server/session"
)

// Each call fails one of its statements and must panic with that statement's
// error well inside forcedFailureCallTimeout. These calls used to drop or
// return the error, after which their transaction went on to a commit that
// server.Tx retried for its one-minute window before the call failed with
// "commit unexpectedly resulted in rollback".
func TestTransactionStatementFailuresSurfaceAtOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId := server.NewId()
		userId := server.NewId()
		Testing_CreateNetwork(ctx, networkId, "test", userId)
		byJwt := &session.ByJwt{
			NetworkId: networkId,
			UserId:    userId,
		}
		locationId := server.NewId()
		expiresAt := server.NowUtc().Add(time.Hour)

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
		createIntentWithUniqueAmount := func(callCtx context.Context) {
			CreateSolanaPaymentIntentWithUniqueAmount(
				callCtx,
				"reference-unique-amount",
				networkId,
				5,
				"monthly",
				expiresAt,
				func() int64 {
					return 1234
				},
			)
		}

		for _, c := range []struct {
			name  string
			force func() func()
			call  func(callCtx context.Context)
			code  string
		}{
			{
				name:  "NetworkBlockLocation",
				force: unavailable("exclude_network_client_location"),
				call: func(callCtx context.Context) {
					NetworkBlockLocation(callCtx, networkId, locationId)
				},
				code: "42P01",
			},
			{
				name:  "NetworkUnblockLocation",
				force: unavailable("exclude_network_client_location"),
				call: func(callCtx context.Context) {
					NetworkUnblockLocation(callCtx, networkId, locationId)
				},
				code: "42P01",
			},
			{
				name:  "SetPaymentReconcileWatermark",
				force: unavailable("payment_reconciliation_watermark"),
				call: func(callCtx context.Context) {
					SetPaymentReconcileWatermark(callCtx, "test-store", server.NowUtc())
				},
				code: "42P01",
			},
			{
				name:  "LoadAppleOfferCodePool",
				force: unavailable("network_onboarding_apple_offer_code"),
				call: func(callCtx context.Context) {
					LoadAppleOfferCodePool(callCtx, []*AppleOfferCode{
						{
							Code:      "TESTOFFERCODE",
							ExpiresAt: expiresAt,
						},
					})
				},
				code: "42P01",
			},
			{
				name:  "CreateSolanaPaymentIntent",
				force: unavailable("solana_payment_intent"),
				call: func(callCtx context.Context) {
					CreateSolanaPaymentIntent("reference-intent", 5, "monthly", session.Testing_CreateClientSession(callCtx, byJwt))
				},
				code: "42P01",
			},
			{
				name:  "CreateSolanaPaymentIntentForNetwork",
				force: unavailable("solana_payment_intent"),
				call: func(callCtx context.Context) {
					CreateSolanaPaymentIntentForNetwork(callCtx, "reference-for-network", networkId, 5, "data_1tib", expiresAt)
				},
				code: "42P01",
			},
			{
				name:  "CreateSolanaPaymentIntentWithUniqueAmount, its intent",
				force: unavailable("solana_payment_intent"),
				call:  createIntentWithUniqueAmount,
				code:  "42P01",
			},
			{
				name:  "CreateSolanaPaymentIntentWithUniqueAmount, its reservation",
				force: unavailable("solana_payment_amount_reservation"),
				call:  createIntentWithUniqueAmount,
				code:  "42P01",
			},
			{
				name:  "CreateSolanaPaymentIntentWithUniqueAmount, its quoted amount",
				force: failing("solana_payment_intent", "UPDATE"),
				call:  createIntentWithUniqueAmount,
				code:  "P0001",
			},
			{
				name:  "RecordUnfulfilledSolanaPayment",
				force: unavailable("solana_unfulfilled_payment"),
				call: func(callCtx context.Context) {
					RecordUnfulfilledSolanaPayment(callCtx, &UnfulfilledSolanaPayment{
						TxSignature: "signature-unfulfilled",
						Reason:      "test",
					})
				},
				code: "42P01",
			},
			{
				name:  "RemoveUnfulfilledSolanaPayment",
				force: unavailable("solana_unfulfilled_payment"),
				call: func(callCtx context.Context) {
					RemoveUnfulfilledSolanaPayment(callCtx, "signature-unfulfilled")
				},
				code: "42P01",
			},
			{
				name:  "CreateStripeCustomer",
				force: unavailable("stripe_customer"),
				call: func(callCtx context.Context) {
					CreateStripeCustomer("cus_test", session.Testing_CreateClientSession(callCtx, byJwt))
				},
				code: "42P01",
			},
			{
				name:  "GetStripeCustomer",
				force: unavailable("stripe_customer"),
				call: func(callCtx context.Context) {
					GetStripeCustomer(session.Testing_CreateClientSession(callCtx, byJwt))
				},
				code: "42P01",
			},
		} {
			restore := c.force()
			panicValue := callWithForcedFailure(ctx, c.call)
			restore()
			if !isForcedFailure(panicValue, c.code, "") {
				t.Errorf("%s: the failed statement ended with %v, want the %s failure", c.name, panicValue, c.code)
			}
		}
	})
}
