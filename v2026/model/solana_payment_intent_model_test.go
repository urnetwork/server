package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/jwt"
	"github.com/urnetwork/server/v2026/session"
)

func TestSolanaPaymentIntents(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {

		ctx := context.Background()

		networkId := server.NewId()
		clientId := server.NewId()

		userSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: networkId,
			ClientId:  &clientId,
		})

		reference := "test-reference-1"

		err := CreateSolanaPaymentIntent(reference, 10.00, "supporter", userSession)
		connect.AssertEqual(t, err, nil)

		// adding the same reference twice should fail
		err = CreateSolanaPaymentIntent(reference, 10.00, "supporter", userSession)
		connect.AssertNotEqual(t, err, nil)

		references := []string{"AAA", "BBB", "CCC", "DDD"}

		// test not found
		paymentSearchResult, err := SearchPaymentIntents(references, userSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, paymentSearchResult, nil)

		// test found
		references = append(references, reference)
		paymentSearchResult, err = SearchPaymentIntents(references, userSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, *paymentSearchResult.NetworkId, networkId)
		connect.AssertEqual(t, *&paymentSearchResult.PaymentReference, reference)

		// mark completed
		completed, err := MarkPaymentIntentCompleted(reference, "tx-signature-1", userSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, completed, true)

		// marking again affects nothing -- the intent is consumed
		completed, err = MarkPaymentIntentCompleted(reference, "tx-signature-1", userSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, completed, false)

	})
}

// TestSolanaConsumedIntentIsNotFoundAgain pins the single-credit gate. The webhook
// searches intents by reference and grants against whatever it finds; the ONLY thing
// standing between one on-chain payment and two grants is that a consumed intent
// (tx_signature IS NOT NULL) stops matching. If the search returned it again, a Helius
// redelivery of the same transaction would buy a second subscription with the same
// money.
func TestSolanaConsumedIntentIsNotFoundAgain(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {

		ctx := context.Background()

		networkId := server.NewId()
		clientId := server.NewId()

		userSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: networkId,
			ClientId:  &clientId,
		})

		reference := "consumed-once-1"

		err := CreateSolanaPaymentIntent(reference, 5.00, SolanaPlanMonthly, userSession)
		connect.AssertEqual(t, err, nil)

		// open: found, with the quote intact
		search, err := SearchPaymentIntents([]string{reference}, userSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertNotEqual(t, search, nil)
		connect.AssertEqual(t, search.ExpectedAmountUsd, 5.00)
		connect.AssertEqual(t, search.SubscriptionPlan, SolanaPlanMonthly)

		// a payment consumes it
		completed, err := MarkPaymentIntentCompleted(reference, "tx-sig-consumed-1", userSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, completed, true)

		// consumed: never found again, so it can never be granted against again
		search, err = SearchPaymentIntents([]string{reference}, userSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, search, nil)
	})
}

// TestSolanaPaymentIntentExpiry pins the quote's lifetime and WHO enforces it.
//
// An intent is payable for a full hour. The search does not check expires_at -- expiry
// is enforced by the periodic sweep (CleanupExpiredPaymentIntents), so a payment that
// arrives after the hour but before the sweep still lands rather than being orphaned.
// Once swept, the reference is gone: a payment against it finds no intent and buys
// nothing, which is why the sweep must never run ahead of the expiry it enforces.
//
// A CONSUMED intent is never swept, however old: the completed row is the audit record
// tying the on-chain signature to the grant.
func TestSolanaPaymentIntentExpiry(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {

		ctx := context.Background()

		networkId := server.NewId()
		clientId := server.NewId()

		userSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: networkId,
			ClientId:  &clientId,
		})

		reference := "expiry-1"

		err := CreateSolanaPaymentIntent(reference, 40.00, SolanaPlanYearly, userSession)
		connect.AssertEqual(t, err, nil)

		now := server.NowUtc()

		// a fresh quote survives the sweep...
		err = CleanupExpiredPaymentIntents(ctx, now)
		connect.AssertEqual(t, err, nil)
		search, err := SearchPaymentIntents([]string{reference}, userSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertNotEqual(t, search, nil)

		// ...for the full hour the customer was given to pay...
		err = CleanupExpiredPaymentIntents(ctx, now.Add(59*time.Minute))
		connect.AssertEqual(t, err, nil)
		search, err = SearchPaymentIntents([]string{reference}, userSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertNotEqual(t, search, nil)

		// ...and no longer
		err = CleanupExpiredPaymentIntents(ctx, now.Add(61*time.Minute))
		connect.AssertEqual(t, err, nil)
		search, err = SearchPaymentIntents([]string{reference}, userSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, search, nil)

		// an EXPIRED intent the sweep has not reached yet is still payable: the sweep
		// cadence owns the cutoff, not the search. A payment in that window lands
		// instead of being silently orphaned.
		lateReference := "expiry-late-1"
		err = CreateSolanaPaymentIntent(lateReference, 40.00, SolanaPlanYearly, userSession)
		connect.AssertEqual(t, err, nil)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`UPDATE solana_payment_intent SET expires_at = $2 WHERE payment_reference = $1`,
				lateReference,
				now.Add(-1*time.Minute),
			))
		})
		search, err = SearchPaymentIntents([]string{lateReference}, userSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertNotEqual(t, search, nil)

		// once the sweep catches up, the reference is gone
		err = CleanupExpiredPaymentIntents(ctx, now)
		connect.AssertEqual(t, err, nil)
		search, err = SearchPaymentIntents([]string{lateReference}, userSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, search, nil)

		// a consumed intent is not swept, however far past expiry: keep the audit trail
		consumedReference := "expiry-consumed-1"
		err = CreateSolanaPaymentIntent(consumedReference, 40.00, SolanaPlanYearly, userSession)
		connect.AssertEqual(t, err, nil)
		completed, err := MarkPaymentIntentCompleted(consumedReference, "tx-sig-expiry-1", userSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, completed, true)

		err = CleanupExpiredPaymentIntents(ctx, now.Add(365*24*time.Hour))
		connect.AssertEqual(t, err, nil)

		// the search cannot see consumed rows (that is the point), so count directly
		var consumedRows int
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(
				ctx,
				`SELECT COUNT(*) FROM solana_payment_intent WHERE payment_reference = $1 AND tx_signature IS NOT NULL`,
				consumedReference,
			)
			server.WithPgResult(result, err, func() {
				if result.Next() {
					server.Raise(result.Scan(&consumedRows))
				}
			})
		})
		connect.AssertEqual(t, consumedRows, 1)
	})
}

// TestSolanaMarkCompletedConcurrent pins the S4 race: two concurrent deliveries of
// the SAME transaction both try to consume the intent, and both set the SAME
// signature -- so the unique index on tx_signature can never fire, and the guarded
// UPDATE (tx_signature IS NULL, rows-affected checked) is the only thing standing
// between one payment and two credits. Exactly one caller may win.
func TestSolanaMarkCompletedConcurrent(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {

		ctx := context.Background()

		networkId := server.NewId()
		clientId := server.NewId()

		userSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: networkId,
			ClientId:  &clientId,
		})

		reference := "concurrent-mark-1"

		err := CreateSolanaPaymentIntent(reference, 5.00, SolanaPlanMonthly, userSession)
		connect.AssertEqual(t, err, nil)

		start := make(chan struct{})
		results := make(chan bool, 2)
		errs := make(chan error, 2)
		for i := 0; i < 2; i += 1 {
			go func() {
				<-start
				completed, err := MarkPaymentIntentCompleted(reference, "tx-sig-concurrent-1", userSession)
				results <- completed
				errs <- err
			}()
		}
		close(start)

		wins := 0
		for i := 0; i < 2; i += 1 {
			connect.AssertEqual(t, <-errs, nil)
			if <-results {
				wins += 1
			}
		}
		// exactly one delivery wins the credit
		connect.AssertEqual(t, wins, 1)
	})
}
