package controller

// Database-backed root-cause test of the Solana reconcile's receiver leg
// (solana_receiver_sweep.go): a USDC payment that reached our receiver after
// its intent was swept, and whose Helius delivery was lost, is recorded once
// for an operator by the hourly reconcile and never credited; a payment a
// record already names is left alone. Before the leg, nothing read the
// receivers' own history: seven 40 USDC payments of 2025-09 to 2026-08 were
// never credited and nobody was told (support inbox 6584, 6692, 6729).
//
// The Solana RPC is the fake of solana_receiver_fake_test.go; the database is
// real (server.DefaultTestEnv). The test calls only the reconcile's Solana
// entry point, so it runs unchanged against the code before the leg.

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// The receiver, its USDC token account and the payers of these tests.
const (
	solanaReceiverDbTestReceiver     = "SyntheticReceiver111111111111111111111111111"
	solanaReceiverDbTestTokenAccount = "ReceiverToken1111111111111111111111111111111"
	solanaReceiverDbTestPayer        = "PayerWa11et11111111111111111111111111111111"
	solanaReceiverDbTestPayerToken   = "PayerToken111111111111111111111111111111111"
	solanaReceiverDbTestCreditedSig  = "CreditedSig1111111111111111111111111111111111111111111111111111111111111111111111111111"
	solanaReceiverDbTestLostSig      = "LostSig111111111111111111111111111111111111111111111111111111111111111111111111111111111"
	solanaReceiverDbTestSweptRef     = "SweptReference11111111111111111111111111111"
	solanaReceiverDbTestCreditedRef  = "CreditedReference111111111111111111111111111"
)

// The lost payment is recorded once, with its sender and amount, and never
// credited; the credited one is never even read.
func TestSolanaReconcileRecordsAReceiverPaymentNoRecordNames(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		// a fixed clock: the hand sweep of the receivers ran on 2026-11-12
		now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

		rpc := newFakeSolanaReceiverRpc(t)
		rpc.ownerTokenAccounts[solanaReceiverDbTestReceiver] = []string{solanaReceiverDbTestTokenAccount}
		rpc.tokenAccountHistories[solanaReceiverDbTestTokenAccount] = []*solanaReceiverFakeTransaction{
			// newest first: a 40 USDC payment whose intent was swept and
			// whose delivery was lost, then a 40 USDC payment its intent
			// credited
			solanaReceiverFakeTransfer(
				solanaReceiverDbTestLostSig, 1002, now.Add(-2*time.Hour),
				solanaReceiverDbTestPayer, solanaReceiverDbTestPayerToken,
				solanaReceiverDbTestReceiver, solanaReceiverDbTestTokenAccount,
				40_000_000, solanaReceiverDbTestSweptRef,
			),
			solanaReceiverFakeTransfer(
				solanaReceiverDbTestCreditedSig, 1001, now.Add(-3*time.Hour),
				solanaReceiverDbTestPayer, solanaReceiverDbTestPayerToken,
				solanaReceiverDbTestReceiver, solanaReceiverDbTestTokenAccount,
				40_000_000, solanaReceiverDbTestCreditedRef,
			),
		}

		previousReceivers := solanaReceiverAddresses
		previousUrl := solanaChainRpcUrl
		previousRpcFor := solanaChainRpcFor
		solanaReceiverAddresses = []string{solanaReceiverDbTestReceiver}
		solanaChainRpcUrl = func() (string, bool) { return "https://synthetic.invalid/?api-key=synthetic", true }
		solanaChainRpcFor = func(rpcUrl string) solanaChainRpc { return rpc }
		solanaParsedTransactions.reset()
		defer func() {
			solanaReceiverAddresses = previousReceivers
			solanaChainRpcUrl = previousUrl
			solanaChainRpcFor = previousRpcFor
			solanaParsedTransactions.reset()
		}()

		// the credited payment: its intent was consumed by its signature
		networkId, _, clientSession := cryptoDbTestNetwork(t, ctx, "receiverleg")
		err := model.CreateSolanaPaymentIntent(solanaReceiverDbTestCreditedRef, 40, model.SolanaPlanYearly, clientSession)
		connect.AssertEqual(t, err, nil)
		completed, err := model.MarkPaymentIntentCompleted(solanaReceiverDbTestCreditedRef, solanaReceiverDbTestCreditedSig, clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, completed, true)

		reconcile := func() *paymentReconcileRun {
			run := &paymentReconcileRun{
				clientSession: session.Testing_CreateClientSession(ctx, nil),
				runId:         server.NewId(),
				now:           now,
				budget:        paymentReconcileApiBudget,
				storeDetails:  map[string]map[string]any{},
				storeResults:  map[string]*PaymentReconcileStoreResult{},
			}
			_, err := reconcileSolana(run, now.Add(-time.Hour))
			connect.AssertEqual(t, err, nil)
			return run
		}

		type unfulfilledRow struct {
			signature string
			reason    string
			amount    float64
			sender    *string
			network   *server.Id
		}
		unfulfilled := func() []unfulfilledRow {
			rows := []unfulfilledRow{}
			server.Db(ctx, func(conn server.PgConn) {
				result, err := conn.Query(ctx, `
					SELECT tx_signature, reason, token_amount_usd, sender_account, network_id
					FROM solana_unfulfilled_payment
					ORDER BY tx_signature
				`)
				server.WithPgResult(result, err, func() {
					for result.Next() {
						row := unfulfilledRow{}
						server.Raise(result.Scan(&row.signature, &row.reason, &row.amount, &row.sender, &row.network))
						rows = append(rows, row)
					}
				})
			})
			return rows
		}
		renewals := func() int {
			count := 0
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM subscription_renewal WHERE market = $1`, model.SubscriptionMarketSolana).Scan(&count))
			})
			return count
		}

		reconcile()
		rows := unfulfilled()
		// the lost payment is recorded for an operator, with its sender and
		// amount; the credited one is not
		connect.AssertEqual(t, len(rows), 1)
		connect.AssertEqual(t, rows[0].signature, solanaReceiverDbTestLostSig)
		connect.AssertEqual(t, rows[0].reason, model.SolanaUnfulfilledReasonNoIntent)
		connect.AssertEqual(t, rows[0].amount, 40.0)
		connect.AssertEqual(t, rows[0].sender != nil && *rows[0].sender == solanaReceiverDbTestPayer, true)
		connect.AssertEqual(t, rows[0].network, (*server.Id)(nil))
		// never credited: no renewal, and the credited intent is untouched
		connect.AssertEqual(t, renewals(), 0)
		intent := model.GetSolanaPaymentIntent(ctx, solanaReceiverDbTestCreditedRef)
		connect.AssertEqual(t, intent != nil && intent.NetworkId == networkId, true)
		connect.AssertEqual(t, *intent.TxSignature, solanaReceiverDbTestCreditedSig)
		// the credited payment was never read: a record names it
		for _, signature := range rpc.readSignatures {
			connect.AssertEqual(t, signature != solanaReceiverDbTestCreditedSig, true)
		}

		// recorded once: a second run records nothing more and reads nothing
		readBefore := len(rpc.readSignatures)
		reconcile()
		connect.AssertEqual(t, len(unfulfilled()), 1)
		connect.AssertEqual(t, len(rpc.readSignatures), readBefore)
		connect.AssertEqual(t, renewals(), 0)
	})
}
