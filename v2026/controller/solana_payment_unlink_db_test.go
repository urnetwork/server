package controller

// Database-backed tests of the retention unlink of the old Solana Pay
// endpoints (model.UnlinkSolanaPaymentIntents,
// model.UnlinkSolanaUnfulfilledPayments), run by the crypto unlink task
// (RunCryptoPaymentUnlink): a payment of POST /solana/payment-intent or POST
// /pay/data/solana-intent forgets its network unlink_after past its credit, as
// a crypto invoice does, and the Solana reconcile leg verifies only payments
// inside that window. Needs the test database (server.DefaultTestEnv) and
// pro.yml.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

const solanaUnlinkTestDay = 24 * time.Hour

// solanaUnlinkTestPlan is a yearly plan bought through POST
// /solana/payment-intent and credited by the Helius webhook, with its quote
// made quotedAgo and its credit (the renewal's start) creditedAgo.
func solanaUnlinkTestPlan(
	t testing.TB,
	ctx context.Context,
	name string,
	reference string,
	signature string,
	quotedAgo time.Duration,
	creditedAgo time.Duration,
) (server.Id, *session.ClientSession, float64) {
	networkId, _, clientSession := cryptoDbTestNetwork(t, ctx, name)
	quote, err := CreateSolanaPaymentIntent(&SolanaPaymentIntentArgs{
		Reference: reference,
		Plan:      model.SolanaPlanYearly,
	}, clientSession)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, quote.Error, (*SolanaPaymentIntentError)(nil))
	webhook, err := HeliusWebhook(
		[]*SolanaTransaction{solanaTestPayment(reference, signature, quote.AmountUsd)},
		session.Testing_CreateClientSession(ctx, nil),
	)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, webhook.Message, "Processed 1 matching payments")
	connect.AssertEqual(t, cryptoDbRenewals(t, ctx, networkId, model.SubscriptionMarketSolana).transactionId, reference)

	now := server.NowUtc()
	cryptoDbExec(
		ctx,
		`UPDATE solana_payment_intent SET created_at = $2, expires_at = $3 WHERE payment_reference = $1`,
		reference,
		now.Add(-quotedAgo),
		now.Add(-quotedAgo).Add(solanaPlanIntentDuration),
	)
	cryptoDbExec(
		ctx,
		`UPDATE subscription_renewal SET start_time = $2 WHERE network_id = $1 AND market = $3`,
		networkId,
		now.Add(-creditedAgo),
		model.SubscriptionMarketSolana,
	)
	return networkId, clientSession, quote.AmountUsd
}

// solanaUnlinkTestEvent records a reconciliation event about evidence.
func solanaUnlinkTestEvent(ctx context.Context, store string, networkId server.Id, evidence string, eventTime time.Time) server.Id {
	eventId := server.NewId()
	cryptoDbExec(
		ctx,
		`
			INSERT INTO payment_reconciliation_event (event_id, run_id, store, network_id, action, evidence, details, event_time)
			VALUES ($1, $2, $3, $4, $5, $6, '{}', $7)
		`,
		eventId,
		server.NewId(),
		store,
		networkId,
		model.PaymentReconcileActionCredited,
		evidence,
		eventTime,
	)
	return eventId
}

func solanaUnlinkTestEventNetwork(ctx context.Context, eventId server.Id) (networkId *server.Id) {
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`SELECT network_id FROM payment_reconciliation_event WHERE event_id = $1`,
			eventId,
		).Scan(&networkId))
	})
	return
}

// solanaUnlinkTestMatchNote is the note the memo-less match records for a
// payment of an intent's amount after the intent was paid: it names the
// intent's network.
func solanaUnlinkTestMatchNote(reference string, networkId server.Id, signature string) string {
	paidBy := signature
	_, note := solanaMatchMemolessIntent(
		&solanaMemolessTransfer{amountMicro: 40_000_123, tokenAmountUsd: 40.000123, transactionTime: server.NowUtc()},
		[]*model.SolanaPaymentIntent{{
			PaymentReference:  reference,
			NetworkId:         networkId,
			ExpectedAmountUsd: 40.000123,
			SubscriptionPlan:  model.SolanaPlanYearly,
			TxSignature:       &paidBy,
		}},
	)
	return note
}

// solanaUnlinkTestUnfulfilled records an unfulfilled payment at recordedAt.
func solanaUnlinkTestUnfulfilled(t testing.TB, ctx context.Context, payment *model.UnfulfilledSolanaPayment, recordedAt time.Time) {
	connect.AssertEqual(t, model.RecordUnfulfilledSolanaPayment(ctx, payment), nil)
	cryptoDbExec(
		ctx,
		`UPDATE solana_unfulfilled_payment SET record_time = $2 WHERE tx_signature = $1`,
		payment.TxSignature,
		recordedAt,
	)
}

func solanaUnlinkTestDayAligned(value time.Time) bool {
	return value.Equal(value.Truncate(solanaUnlinkTestDay))
}

// A plan paid through POST /solana/payment-intent forgets its network once
// unlink_after has passed since the credit: the intent's network goes, its
// times become the day, the renewal's transaction id becomes random (also a
// renewal keyed by the signature, as the first credits were), and the events
// and match notes about the payment lose the network. A plan credited
// 29 days ago is untouched, even one quoted 31 days ago: a plan's clock is its
// credit. The entitlement, the amounts and the signature stay; a redelivery
// still credits nothing; a second run changes nothing.
func TestSolanaPaymentUnlinkPlans(t *testing.T) {
	skipWithoutProYml(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		cryptoTestResetLimiters()
		now := server.NowUtc()

		oldReference := "unlink-plan-old-ref"
		oldSignature := "sig-unlink-plan-old"
		oldNetworkId, clientSession, oldAmountUsd := solanaUnlinkTestPlan(
			t, ctx, "solanaunlinkold", oldReference, oldSignature, 31*solanaUnlinkTestDay, 31*solanaUnlinkTestDay,
		)
		// quoted 31 days ago, but paid late: credited 29 days ago
		newReference := "unlink-plan-new-ref"
		newSignature := "sig-unlink-plan-new"
		newNetworkId, _, _ := solanaUnlinkTestPlan(
			t, ctx, "solanaunlinknew", newReference, newSignature, 31*solanaUnlinkTestDay, 29*solanaUnlinkTestDay,
		)
		// the first credits (2025-09-06 to 09-09) keyed the renewal by the
		// signature, not the reference
		legacyReference := "unlink-plan-legacy-ref"
		legacySignature := "sig-unlink-plan-legacy"
		legacyNetworkId, _, _ := solanaUnlinkTestPlan(
			t, ctx, "solanaunlinklegacy", legacyReference, legacySignature, 31*solanaUnlinkTestDay, 31*solanaUnlinkTestDay,
		)
		cryptoDbExec(
			ctx,
			`UPDATE subscription_renewal SET transaction_id = $2 WHERE network_id = $1 AND market = $3`,
			legacyNetworkId,
			legacySignature,
			model.SubscriptionMarketSolana,
		)
		oldRenewal := cryptoDbRenewals(t, ctx, oldNetworkId, model.SubscriptionMarketSolana)

		// a credit the reconciler made of each payment, recorded today; a
		// Solana event about some other payment, and another store's event,
		// both recorded 31 days ago
		oldEvent := solanaUnlinkTestEvent(ctx, model.SubscriptionMarketSolana, oldNetworkId, oldSignature, now)
		newEvent := solanaUnlinkTestEvent(ctx, model.SubscriptionMarketSolana, newNetworkId, newSignature, now)
		staleEvent := solanaUnlinkTestEvent(ctx, model.SubscriptionMarketSolana, newNetworkId, "sig-unlink-elsewhere", now.Add(-31*solanaUnlinkTestDay))
		stripeEvent := solanaUnlinkTestEvent(ctx, model.SubscriptionMarketStripe, oldNetworkId, "in_unlink_elsewhere", now.Add(-31*solanaUnlinkTestDay))
		// memo-less payments of each intent's amount after it was paid,
		// recorded today: each note names the intent's network
		oldNote := solanaUnlinkTestMatchNote(oldReference, oldNetworkId, oldSignature)
		newNote := solanaUnlinkTestMatchNote(newReference, newNetworkId, newSignature)
		connect.AssertEqual(t, strings.Contains(oldNote, oldNetworkId.String()), true)
		for signature, note := range map[string]string{"sig-unlink-note-old": oldNote, "sig-unlink-note-new": newNote} {
			solanaUnlinkTestUnfulfilled(t, ctx, &model.UnfulfilledSolanaPayment{
				TxSignature:    signature,
				Reason:         model.SolanaUnfulfilledReasonNoIntent,
				TokenAmountUsd: 40.000123,
				MatchNote:      &note,
			}, now)
		}

		result, err := RunCryptoPaymentUnlink(clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.SolanaUnlinked, 2)
		connect.AssertEqual(t, result.SolanaUnfulfilledUnlinked, 0)
		connect.AssertEqual(t, result.Unlinked, 0)

		// the legacy renewal is found by its signature
		legacyRenewal := cryptoDbRenewals(t, ctx, legacyNetworkId, model.SubscriptionMarketSolana)
		connect.AssertEqual(t, legacyRenewal.count, 1)
		connect.AssertEqual(t, legacyRenewal.transactionId != legacySignature, true)
		connect.AssertEqual(t, legacyRenewal.transactionId != legacyReference, true)
		connect.AssertEqual(t, model.GetSolanaPaymentIntent(ctx, legacyReference).NetworkId, server.Id{})

		// the old payment forgot its network; its signature, quote and plan
		// stay, and every time of the intent is the day
		old := model.GetSolanaPaymentIntent(ctx, oldReference)
		connect.AssertEqual(t, old.NetworkId, server.Id{})
		connect.AssertEqual(t, *old.TxSignature, oldSignature)
		connect.AssertEqual(t, old.ExpectedAmountUsd, oldAmountUsd)
		connect.AssertEqual(t, old.SubscriptionPlan, model.SolanaPlanYearly)
		connect.AssertEqual(t, solanaUnlinkTestDayAligned(old.CreatedAt), true)
		connect.AssertEqual(t, solanaUnlinkTestDayAligned(*old.ExpiresAt), true)
		_, _, linked := model.GetSolanaPaymentIntentCompletion(ctx, oldReference)
		connect.AssertEqual(t, linked, false)
		// the reference no longer finds the renewal, whose window and revenue
		// stay; the entitlement itself stays
		renewal := cryptoDbRenewals(t, ctx, oldNetworkId, model.SubscriptionMarketSolana)
		connect.AssertEqual(t, renewal.count, 1)
		connect.AssertEqual(t, renewal.transactionId != oldReference, true)
		connect.AssertEqual(t, renewal.transactionId != "", true)
		connect.AssertEqual(t, renewal.startTime, oldRenewal.startTime)
		connect.AssertEqual(t, renewal.endTime, oldRenewal.endTime)
		connect.AssertEqual(t, renewal.netRevenue, model.UsdToNanoCents(oldAmountUsd))
		connect.AssertEqual(t, model.IsProNetwork(ctx, oldNetworkId), true)

		// the payment credited 29 days ago keeps its link
		recent := model.GetSolanaPaymentIntent(ctx, newReference)
		connect.AssertEqual(t, recent.NetworkId, newNetworkId)
		connect.AssertEqual(t, cryptoDbRenewals(t, ctx, newNetworkId, model.SubscriptionMarketSolana).transactionId, newReference)
		owner, _, linked := model.GetSolanaPaymentIntentCompletion(ctx, newReference)
		connect.AssertEqual(t, linked, true)
		connect.AssertEqual(t, owner, newNetworkId)

		// the events about the old payment, and every Solana event older than
		// the window, lost their network; the recent payment's event and
		// another store's event did not
		connect.AssertEqual(t, solanaUnlinkTestEventNetwork(ctx, oldEvent), (*server.Id)(nil))
		connect.AssertEqual(t, solanaUnlinkTestEventNetwork(ctx, staleEvent), (*server.Id)(nil))
		connect.AssertEqual(t, *solanaUnlinkTestEventNetwork(ctx, newEvent), newNetworkId)
		connect.AssertEqual(t, *solanaUnlinkTestEventNetwork(ctx, stripeEvent), oldNetworkId)

		// the note naming the old intent no longer names its network, and
		// keeps the rest; the note naming the recent one is untouched
		unlinkedNote, err := model.GetUnfulfilledSolanaPayment(ctx, "sig-unlink-note-old")
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, strings.Contains(*unlinkedNote.MatchNote, oldNetworkId.String()), false)
		connect.AssertEqual(t, *unlinkedNote.MatchNote, strings.Replace(oldNote, "network "+oldNetworkId.String(), "network (unlinked)", 1))
		keptNote, err := model.GetUnfulfilledSolanaPayment(ctx, "sig-unlink-note-new")
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, *keptNote.MatchNote, newNote)

		// Helius redelivers the old payment: the kept signature says it was
		// credited, and nothing is credited again
		webhook, err := HeliusWebhook(
			[]*SolanaTransaction{solanaTestPayment(oldReference, oldSignature, oldAmountUsd)},
			session.Testing_CreateClientSession(ctx, nil),
		)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, webhook.Message, "No payment intent found for this network ID")
		connect.AssertEqual(t, cryptoDbRenewals(t, ctx, oldNetworkId, model.SubscriptionMarketSolana).count, 1)

		// a second run changes nothing
		result, err = RunCryptoPaymentUnlink(clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.SolanaUnlinked, 0)
		connect.AssertEqual(t, result.SolanaUnfulfilledUnlinked, 0)
		connect.AssertEqual(t, cryptoDbRenewals(t, ctx, oldNetworkId, model.SubscriptionMarketSolana).transactionId, renewal.transactionId)
		connect.AssertEqual(t, model.GetSolanaPaymentIntent(ctx, oldReference).CreatedAt, old.CreatedAt)
		connect.AssertEqual(t, model.GetSolanaPaymentIntent(ctx, newReference).NetworkId, newNetworkId)
	})
}

// A data pack bought for a named network through POST
// /pay/data/solana-intent forgets the network once unlink_after has passed
// since its quote (a data pack's clock): the intent loses it, and so does the
// underpayment recorded against the quote. The data stays on the network, and
// the public status no longer names it.
func TestSolanaPaymentUnlinkDataPack(t *testing.T) {
	skipWithoutProYml(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		cryptoTestResetLimiters()
		networkId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "solanaunlinkdata", server.NewId())
		pageSession := session.Testing_CreateClientSession(ctx, nil)
		webhookSession := session.Testing_CreateClientSession(ctx, nil)

		reference := "8Hk3sQx9pLmN2vB4cD6eF8hJ1kM5nP7rS9tU2wY4zA6C"
		solanaTestAmountSuffixes(t, 203)
		quote, err := PayDataSolanaIntent(&PayDataSolanaIntentArgs{
			ItemId:      StripeItemData1Tib,
			NetworkName: "solanaunlinkdata",
			Reference:   reference,
		}, pageSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, quote.Error, (*PayDataCheckoutError)(nil))

		// an underpayment, recorded with the quote's network, then the payment
		webhook, err := HeliusWebhook(
			[]*SolanaTransaction{solanaTestMemoPayment(reference, "sig-unlink-data-under", quote.AmountUsd-1)},
			webhookSession,
		)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, webhook.Message, "Payment is less than the quoted price")
		webhook, err = HeliusWebhook(
			[]*SolanaTransaction{solanaTestMemoPayment(reference, "sig-unlink-data", quote.AmountUsd)},
			webhookSession,
		)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, webhook.Message, "Processed 1 matching payments")
		underpaid, err := model.GetUnfulfilledSolanaPayment(ctx, "sig-unlink-data-under")
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, *underpaid.NetworkId, networkId)

		// recent: kept
		result, err := RunCryptoPaymentUnlink(pageSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.SolanaUnlinked, 0)
		status, err := PayDataSolanaStatus(&PayDataSolanaStatusArgs{Reference: reference}, pageSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, status.NetworkName, "solanaunlinkdata")

		now := server.NowUtc()
		cryptoDbExec(
			ctx,
			`UPDATE solana_payment_intent SET created_at = $2, expires_at = $3 WHERE payment_reference = $1`,
			reference,
			now.Add(-31*solanaUnlinkTestDay),
			now.Add(-31*solanaUnlinkTestDay).Add(payDataSolanaIntentDuration),
		)
		result, err = RunCryptoPaymentUnlink(pageSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.SolanaUnlinked, 1)

		intent := model.GetSolanaPaymentIntent(ctx, reference)
		connect.AssertEqual(t, intent.NetworkId, server.Id{})
		connect.AssertEqual(t, *intent.TxSignature, "sig-unlink-data")
		connect.AssertEqual(t, intent.ExpectedAmountUsd, quote.AmountUsd)
		connect.AssertEqual(t, intent.SubscriptionPlan, StripeItemData1Tib)
		connect.AssertEqual(t, solanaUnlinkTestDayAligned(intent.CreatedAt), true)
		connect.AssertEqual(t, solanaUnlinkTestDayAligned(*intent.ExpiresAt), true)

		// the underpayment, recorded less than the window ago, lost the
		// network with its quote; its amounts and reference stay
		underpaid, err = model.GetUnfulfilledSolanaPayment(ctx, "sig-unlink-data-under")
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, underpaid.NetworkId, (*server.Id)(nil))
		connect.AssertEqual(t, *underpaid.PaymentReference, reference)
		connect.AssertEqual(t, underpaid.TokenAmountUsd, quote.AmountUsd-1)
		connect.AssertEqual(t, *underpaid.ExpectedAmountUsd, quote.AmountUsd)

		// the data stays on the network; the reference, which is on the chain,
		// no longer finds its name
		balances := model.GetActiveTransferBalances(ctx, networkId)
		connect.AssertEqual(t, len(balances), 1)
		connect.AssertEqual(t, balances[0].NetRevenue, model.UsdToNanoCents(quote.AmountUsd))
		status, err = PayDataSolanaStatus(&PayDataSolanaStatusArgs{Reference: reference}, pageSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, status.Status, PayDataSolanaStatusPaid)
		connect.AssertEqual(t, status.NetworkName, "")

		result, err = RunCryptoPaymentUnlink(pageSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.SolanaUnlinked, 0)
	})
}

// A USDC invoice of the crypto flow links its Solana intent; the old
// endpoints' unlink leaves it to the invoice's unlink, which runs by the
// invoice's own clock, and once that ran, nothing is done to it twice.
func TestSolanaPaymentUnlinkLeavesInvoicesToTheCryptoUnlink(t *testing.T) {
	skipWithoutProYml(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		cryptoTestResetLimiters()
		cryptoTestEnable(t, cryptoTestConfig(), map[string]*fakeCryptoWallet{})
		networkId, _, clientSession := cryptoDbTestNetwork(t, ctx, "solanaunlinkinvoice")
		solanaTestAmountSuffixes(t, 401)

		invoice, err := CreateCryptoPaymentIntent(&CreateCryptoPaymentIntentArgs{Asset: model.CryptoAssetUsdcSolana, Plan: model.SolanaPlanYearly}, clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, invoice.Error, (*CryptoPaymentError)(nil))
		reference := invoice.Memo
		intentId := server.RequireParseId(invoice.IntentId)
		webhook, err := HeliusWebhook(
			[]*SolanaTransaction{solanaTestPayment(reference, "sig-unlink-invoice", model.GetSolanaPaymentIntent(ctx, reference).ExpectedAmountUsd)},
			session.Testing_CreateClientSession(ctx, nil),
		)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, webhook.Message, "Processed 1 matching payments")

		// the Solana side says 31 days, the invoice 29
		now := server.NowUtc()
		cryptoDbExec(
			ctx,
			`UPDATE subscription_renewal SET start_time = $2 WHERE network_id = $1 AND market = $3`,
			networkId,
			now.Add(-31*solanaUnlinkTestDay),
			model.SubscriptionMarketSolana,
		)
		cryptoDbExec(ctx, `UPDATE solana_payment_intent SET created_at = $2 WHERE payment_reference = $1`, reference, now.Add(-31*solanaUnlinkTestDay))
		cryptoDbExec(ctx, `UPDATE crypto_payment_intent SET credited_at = $2 WHERE intent_id = $1`, intentId, now.Add(-29*solanaUnlinkTestDay))

		result, err := RunCryptoPaymentUnlink(clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Unlinked, 0)
		connect.AssertEqual(t, result.SolanaUnlinked, 0)
		connect.AssertEqual(t, model.GetSolanaPaymentIntent(ctx, reference).NetworkId, networkId)
		connect.AssertEqual(t, cryptoDbRenewals(t, ctx, networkId, model.SubscriptionMarketSolana).transactionId, reference)

		// past the invoice's own unlink_after: the crypto unlink takes the
		// invoice and its Solana intent
		cryptoDbExec(ctx, `UPDATE crypto_payment_intent SET credited_at = $2 WHERE intent_id = $1`, intentId, now.Add(-31*solanaUnlinkTestDay))
		result, err = RunCryptoPaymentUnlink(clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Unlinked, 1)
		connect.AssertEqual(t, result.SolanaUnlinked, 0)
		connect.AssertEqual(t, model.GetCryptoPaymentIntent(ctx, intentId).UnlinkedAt != nil, true)
		connect.AssertEqual(t, model.GetSolanaPaymentIntent(ctx, reference).NetworkId, server.Id{})
		renewal := cryptoDbRenewals(t, ctx, networkId, model.SubscriptionMarketSolana)
		connect.AssertEqual(t, renewal.transactionId != reference, true)

		result, err = RunCryptoPaymentUnlink(clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Unlinked, 0)
		connect.AssertEqual(t, result.SolanaUnlinked, 0)
		connect.AssertEqual(t, cryptoDbRenewals(t, ctx, networkId, model.SubscriptionMarketSolana).transactionId, renewal.transactionId)
	})
}

// Unfulfilled Solana payments recorded before the window lose their
// network, in the column and in the match note, and keep the rest: the one
// record of money to refund or credit by hand. Recent ones are untouched; a
// second run changes nothing.
func TestSolanaUnfulfilledPaymentUnlink(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession := session.Testing_CreateClientSession(ctx, nil)
		now := server.NowUtc()
		quoteNetworkId := server.NewId()
		candidateNetworkId := server.NewId()
		reference := "unlink-unfulfilled-ref"
		expectedAmountUsd := 40.000123
		note := solanaUnlinkTestMatchNote("unlink-unfulfilled-candidate", candidateNetworkId, "sig-unlink-candidate")
		recordedAgo := map[string]time.Duration{
			"old":    31 * solanaUnlinkTestDay,
			"recent": 29 * solanaUnlinkTestDay,
		}
		for suffix, ago := range recordedAgo {
			recordedAt := now.Add(-ago)
			solanaUnlinkTestUnfulfilled(t, ctx, &model.UnfulfilledSolanaPayment{
				TxSignature:       "sig-unlink-underpaid-" + suffix,
				Reason:            model.SolanaUnfulfilledReasonUnderpaid,
				TokenAmountUsd:    39,
				ExpectedAmountUsd: &expectedAmountUsd,
				PaymentReference:  &reference,
				NetworkId:         &quoteNetworkId,
			}, recordedAt)
			solanaUnlinkTestUnfulfilled(t, ctx, &model.UnfulfilledSolanaPayment{
				TxSignature:         "sig-unlink-memoless-" + suffix,
				Reason:              model.SolanaUnfulfilledReasonNoIntent,
				TokenAmountUsd:      40.000123,
				ReferenceCandidates: []string{"SomeAccountAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"},
				MatchNote:           &note,
			}, recordedAt)
		}

		result, err := RunCryptoPaymentUnlink(clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.SolanaUnfulfilledUnlinked, 2)

		underpaid, err := model.GetUnfulfilledSolanaPayment(ctx, "sig-unlink-underpaid-old")
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, underpaid.NetworkId, (*server.Id)(nil))
		connect.AssertEqual(t, underpaid.Reason, model.SolanaUnfulfilledReasonUnderpaid)
		connect.AssertEqual(t, underpaid.TokenAmountUsd, 39.0)
		connect.AssertEqual(t, *underpaid.ExpectedAmountUsd, expectedAmountUsd)
		connect.AssertEqual(t, *underpaid.PaymentReference, reference)
		memoless, err := model.GetUnfulfilledSolanaPayment(ctx, "sig-unlink-memoless-old")
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, strings.Contains(*memoless.MatchNote, candidateNetworkId.String()), false)
		connect.AssertEqual(t, *memoless.MatchNote, strings.Replace(note, "network "+candidateNetworkId.String(), "network (unlinked)", 1))
		connect.AssertEqual(t, memoless.TokenAmountUsd, 40.000123)

		underpaid, err = model.GetUnfulfilledSolanaPayment(ctx, "sig-unlink-underpaid-recent")
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, *underpaid.NetworkId, quoteNetworkId)
		memoless, err = model.GetUnfulfilledSolanaPayment(ctx, "sig-unlink-memoless-recent")
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, *memoless.MatchNote, note)

		result, err = RunCryptoPaymentUnlink(clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.SolanaUnfulfilledUnlinked, 0)
	})
}

// The Solana reconcile leg re-verifies on the chain only payments credited
// within the retention window: one credited 31 days ago, not yet unlinked,
// is left alone even though the fake chain does not know its signature,
// while one credited 29 days ago is checked and ended.
func TestPaymentReconcileSolanaVerifiesOnlyTheLinkWindow(t *testing.T) {
	skipWithoutProYml(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer disableAllReconcileStores(t)()
		newSolanaReconcileTestEnv(t)
		cryptoTestResetLimiters()

		oldNetworkId, _, _ := solanaUnlinkTestPlan(
			t, ctx, "reconcilewindowold", "reconcile-window-old-ref", "sig-reconcile-window-old", 31*solanaUnlinkTestDay, 31*solanaUnlinkTestDay,
		)
		newNetworkId, _, _ := solanaUnlinkTestPlan(
			t, ctx, "reconcilewindownew", "reconcile-window-new-ref", "sig-reconcile-window-new", 29*solanaUnlinkTestDay, 29*solanaUnlinkTestDay,
		)

		result, err := RunPaymentReconciliationWithOptions(
			reconcileTestSession(t, ctx),
			&PaymentReconcileRunOptions{Stores: []string{model.SubscriptionMarketSolana}},
		)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Errors, 0)
		connect.AssertEqual(t, result.Ended, 1)
		connect.AssertEqual(t, result.StoreResults[model.SubscriptionMarketSolana].Examined, 1)
		connect.AssertEqual(t, model.IsProNetwork(ctx, newNetworkId), false)
		connect.AssertEqual(t, model.IsProNetwork(ctx, oldNetworkId), true)
		events := model.GetPaymentReconciliationEvents(ctx, result.RunId)
		connect.AssertEqual(t, countReconcileEvents(events, model.SubscriptionMarketSolana, model.PaymentReconcileActionEnded), 1)
		for _, event := range events {
			if event.Action == model.PaymentReconcileActionEnded {
				connect.AssertEqual(t, event.Evidence, "sig-reconcile-window-new")
			}
		}
	})
}
