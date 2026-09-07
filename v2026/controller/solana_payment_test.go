package controller

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/jwt"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// TestSolanaPlanPriceComesFromTheServer pins that the quote is derived server-side from
// pro.yml, never taken from the client. A client-supplied amount would let anyone quote
// themselves a year for a cent.
func TestSolanaPlanPriceComesFromTheServer(t *testing.T) {
	skipWithoutProYml(t)

	monthly, ok := solanaPlanPriceUsd(model.SolanaPlanMonthly)
	connect.AssertEqual(t, ok, true)
	connect.AssertEqual(t, monthly, float64(5))

	yearly, ok := solanaPlanPriceUsd(model.SolanaPlanYearly)
	connect.AssertEqual(t, ok, true)
	connect.AssertEqual(t, yearly, float64(40))

	// an unknown plan has no price -- the intent is refused rather than priced at zero
	_, ok = solanaPlanPriceUsd("free_forever_please")
	connect.AssertEqual(t, ok, false)
	_, ok = solanaPlanPriceUsd("")
	connect.AssertEqual(t, ok, false)
}

// TestSolanaPlanDuration pins that the customer gets the plan they BOUGHT.
//
// The webhook used to grant a full YEAR for every accepted payment, whatever had been
// chosen and whatever had been paid.
func TestSolanaPlanDuration(t *testing.T) {
	connect.AssertEqual(t, solanaPlanDuration(model.SolanaPlanMonthly), 30*24*time.Hour)
	connect.AssertEqual(t, solanaPlanDuration(model.SolanaPlanYearly), SubscriptionYearDuration)

	// an intent created before the plan was recorded was always treated as yearly, so
	// those legacy intents keep getting exactly that
	connect.AssertEqual(t, solanaPlanDuration(""), SubscriptionYearDuration)
}

// TestSolanaUnderpaymentIsRefused pins the amount check, which is the whole reason the
// quote is recorded.
//
// underpaid := tolerance < expected - received
func TestSolanaUnderpaymentIsRefused(t *testing.T) {
	underpaid := func(expected float64, received float64) bool {
		return solanaAmountTolerance < expected-received
	}

	// the exact price is fine
	connect.AssertEqual(t, underpaid(40, 40), false)
	connect.AssertEqual(t, underpaid(5, 5), false)

	// overpaying is the customer's choice, and is honored
	connect.AssertEqual(t, underpaid(5, 40), false)
	connect.AssertEqual(t, underpaid(40, 100), false)

	// float dust is absorbed, not treated as a shortfall
	connect.AssertEqual(t, underpaid(40, 39.999), false)

	// a real shortfall is refused. THIS is the case that used to buy a year:
	// the old code accepted anything >= 40 and granted a year regardless.
	connect.AssertEqual(t, underpaid(40, 5), true)
	connect.AssertEqual(t, underpaid(40, 39.5), true)
	connect.AssertEqual(t, underpaid(5, 0.01), true)
}

// TestSolanaMonthlyPaymentIsNoLongerIgnored is the bug in one line.
//
// The site offers UR Pro monthly at $5 and takes Solana. The webhook required
// `TokenAmount >= 40` before it would even look at a transfer — so a $5 payment was
// discarded as "no matching USDC payment". The customer paid five dollars and received
// nothing at all, with no error anywhere.
func TestSolanaMonthlyPaymentIsNoLongerIgnored(t *testing.T) {
	skipWithoutProYml(t)

	monthlyPrice, ok := solanaPlanPriceUsd(model.SolanaPlanMonthly)
	connect.AssertEqual(t, ok, true)

	// what the old code required before it would accept a transfer at all
	const oldHardcodedMinimum = 40.0
	connect.AssertEqual(t, monthlyPrice < oldHardcodedMinimum, true) // ...so $5 was dropped

	// now: a $5 payment against a $5 quote is accepted, and buys a MONTH
	connect.AssertEqual(t, solanaAmountTolerance < monthlyPrice-monthlyPrice, false)
	connect.AssertEqual(t, solanaPlanDuration(model.SolanaPlanMonthly), 30*24*time.Hour)
}

// TestSolanaUnusableConfiguredPriceIsNotQuoted pins the zero-price refusal in
// solanaPlanPriceUsd itself. With no pro.yml (or a mis-specified price: 0) the plan
// would otherwise quote at $0.00 -- and the webhook's check is
// `amount >= price - tolerance`, which at price 0 is `amount >= -0.01`: satisfied by
// ANY payment, including none. A free year for everyone. ok = false is what closes it.
//
// The overrides mutate the process-cached config the same way the model.Testing_Set*
// helpers do, and are restored before the test returns.
func TestSolanaUnusableConfiguredPriceIsNotQuoted(t *testing.T) {
	pro := model.Pro()
	prevMonthly := pro.Pro.PriceMonthlyUsd
	prevYearly := pro.Pro.PriceYearlyUsd
	defer func() {
		pro.Pro.PriceMonthlyUsd = prevMonthly
		pro.Pro.PriceYearlyUsd = prevYearly
	}()

	// what an absent pro.yml (or price: 0) parses to
	pro.Pro.PriceMonthlyUsd = 0
	_, ok := solanaPlanPriceUsd(model.SolanaPlanMonthly)
	connect.AssertEqual(t, ok, false)

	// negative is as unsellable as zero -- at price -5 the check is `amount >= -5.01`
	pro.Pro.PriceYearlyUsd = -5
	_, ok = solanaPlanPriceUsd(model.SolanaPlanYearly)
	connect.AssertEqual(t, ok, false)
}

// TestSolanaIntentRefusedPlansCreateNothing pins the refusal path end to end: a plan we
// will not sell must leave NOTHING behind. An unknown plan, an empty plan, and a plan
// whose configured price is unusable all return the error result -- and no row is
// written, so the webhook can never later match an arriving payment against a quote we
// refused to make.
func TestSolanaIntentRefusedPlansCreateNothing(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId := server.NewId()
		clientId := server.NewId()
		userSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: networkId,
			ClientId:  &clientId,
		})

		assertRefusedAndNothingStored := func(reference string, plan string) {
			result, err := CreateSolanaPaymentIntent(&SolanaPaymentIntentArgs{
				Reference: reference,
				Plan:      plan,
			}, userSession)
			connect.AssertEqual(t, err, nil)
			connect.AssertNotEqual(t, result.Error, nil)
			// an errored result must not carry a price the client could act on
			connect.AssertEqual(t, result.AmountUsd, float64(0))

			// and no row: the reference must not be payable
			search, err := model.SearchPaymentIntents([]string{reference}, userSession)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, search, nil)
		}

		assertRefusedAndNothingStored("intent-unknown-plan", "free_forever_please")
		assertRefusedAndNothingStored("intent-empty-plan", "")

		// a configured-but-unusable price is refused the same way. This is what an absent
		// or mis-specified pro.yml looks like to the quote -- the free-year bug.
		pro := model.Pro()
		prevMonthly := pro.Pro.PriceMonthlyUsd
		pro.Pro.PriceMonthlyUsd = 0
		func() {
			defer func() { pro.Pro.PriceMonthlyUsd = prevMonthly }()
			assertRefusedAndNothingStored("intent-zero-price", model.SolanaPlanMonthly)
		}()
	})
}

// TestSolanaIntentQuoteIsTheServersAndDuplicatesAreLoud pins the two halves of intent
// creation that each shipped broken once:
//
//   - the result carries the price the SERVER derived from pro.yml. The args have no
//     amount field to honor, so what the client is told to pay and what the webhook
//     checks the payment against cannot disagree;
//   - a duplicate reference is an ERROR the caller can see. The model error used to be
//     discarded here, so a duplicate or failed intent looked exactly like a success and
//     the customer was sent off to pay against an intent that did not exist.
func TestSolanaIntentQuoteIsTheServersAndDuplicatesAreLoud(t *testing.T) {
	skipWithoutProYml(t)

	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId := server.NewId()
		clientId := server.NewId()
		userSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: networkId,
			ClientId:  &clientId,
		})

		reference := "intent-quote-1"

		result, err := CreateSolanaPaymentIntent(&SolanaPaymentIntentArgs{
			Reference: reference,
			Plan:      model.SolanaPlanMonthly,
		}, userSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Error, nil)
		connect.AssertEqual(t, result.AmountUsd, model.Pro().PriceMonthlyUsd())

		// the STORED quote is the same price and plan -- this is what the webhook will
		// hold the arriving payment to
		search, err := model.SearchPaymentIntents([]string{reference}, userSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertNotEqual(t, search, nil)
		connect.AssertEqual(t, search.ExpectedAmountUsd, model.Pro().PriceMonthlyUsd())
		connect.AssertEqual(t, search.SubscriptionPlan, model.SolanaPlanMonthly)
		connect.AssertEqual(t, *search.NetworkId, networkId)

		// the same reference again -- a client retry -- is an error result, not a silent
		// success pretending the retry took
		result, err = CreateSolanaPaymentIntent(&SolanaPaymentIntentArgs{
			Reference: reference,
			Plan:      model.SolanaPlanYearly,
		}, userSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertNotEqual(t, result.Error, nil)

		// ...and the retry did not overwrite the original quote
		search, err = model.SearchPaymentIntents([]string{reference}, userSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertNotEqual(t, search, nil)
		connect.AssertEqual(t, search.ExpectedAmountUsd, model.Pro().PriceMonthlyUsd())
		connect.AssertEqual(t, search.SubscriptionPlan, model.SolanaPlanMonthly)

		// yearly quotes the yearly price
		result, err = CreateSolanaPaymentIntent(&SolanaPaymentIntentArgs{
			Reference: "intent-quote-2",
			Plan:      model.SolanaPlanYearly,
		}, userSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Error, nil)
		connect.AssertEqual(t, result.AmountUsd, model.Pro().PriceYearlyUsd())
	})
}

// solanaTestPayment builds the Helius transfer the webhook receives for a USDC payment
// of `amountUsd` carrying `reference` among its account keys. The reference is
// deliberately not the first account: the webhook has to find it, not assume a position.
func solanaTestPayment(reference string, signature string, amountUsd float64) *SolanaTransaction {
	return &SolanaTransaction{
		Type:      "TRANSFER",
		Signature: signature,
		TokenTransfers: []TokenTransfer{
			{
				Mint:            solanaUsdcMint,
				FromUserAccount: "CustomerWa11etAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
				ToUserAccount:   solanaReceiverAddresses[0],
				TokenAmount:     amountUsd,
			},
		},
		AccountData: []AccountData{
			{Account: "CustomerWa11etAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"},
			{Account: solanaReceiverAddresses[0]},
			{Account: reference},
			{Account: "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"},
		},
	}
}

// TestSolanaWebhookGrantsExactlyThePlanQuoted walks a $5 monthly payment through the
// real webhook: intent created by the controller, payment matched by reference among the
// transaction's account keys, amount checked against the stored quote -- and the grant
// is a MONTH (plus grace) of supporter, not the year the old webhook granted for every
// accepted payment whatever had been chosen.
//
// The replay at the end pins single-credit: the consumed intent (tx_signature set) is
// not found again, so Helius redelivering the same transaction cannot buy a second
// month with the same money.
func TestSolanaWebhookGrantsExactlyThePlanQuoted(t *testing.T) {
	skipWithoutProYml(t)

	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId := server.NewId()
		clientId := server.NewId()
		userId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "solanamonthly", userId)

		userSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: networkId,
			ClientId:  &clientId,
			UserId:    userId,
		})
		// the webhook route is NoAuth -- Helius calls it, not a signed-in user
		webhookSession := session.Testing_CreateClientSession(ctx, nil)

		reference := "webhook-monthly-1"
		intentResult, err := CreateSolanaPaymentIntent(&SolanaPaymentIntentArgs{
			Reference: reference,
			Plan:      model.SolanaPlanMonthly,
		}, userSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, intentResult.Error, nil)

		// pay exactly what was quoted
		result, err := HeliusWebhook(
			[]*SolanaTransaction{solanaTestPayment(reference, "sig-monthly-1", intentResult.AmountUsd)},
			webhookSession,
		)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Message, "Processed 1 matching payments")

		balances := model.GetActiveTransferBalances(ctx, networkId)
		connect.AssertEqual(t, len(balances), 1)
		connect.AssertEqual(t, balances[0].Pro, true)
		connect.AssertEqual(t, balances[0].BalanceByteCount, RefreshSupporterTransferBalance)
		// a MONTH plus the grace period. The year-for-everything webhook fails here.
		connect.AssertEqual(t, balances[0].EndTime.Sub(balances[0].StartTime), 30*24*time.Hour+SubscriptionGracePeriod)
		// the entitlement is refreshed immediately, not after the pro cache ttl
		connect.AssertEqual(t, model.IsProNetwork(ctx, networkId), true)

		// Helius redelivers the same transaction: the intent is consumed, nothing is
		// granted twice
		result, err = HeliusWebhook(
			[]*SolanaTransaction{solanaTestPayment(reference, "sig-monthly-1", intentResult.AmountUsd)},
			webhookSession,
		)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Message, "No payment intent found for this network ID")
		connect.AssertEqual(t, len(model.GetActiveTransferBalances(ctx, networkId)), 1)
	})
}

// TestSolanaWebhookOverpaymentBuysThePlanQuotedNotAYear pins the other half of the old
// `>= 40` bug: any payment of 40+ USDC used to buy a YEAR, whatever the customer had
// actually chosen. A generous $100 against a $5 monthly quote is honored -- as a month.
func TestSolanaWebhookOverpaymentBuysThePlanQuotedNotAYear(t *testing.T) {
	skipWithoutProYml(t)

	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId := server.NewId()
		clientId := server.NewId()
		userId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "solanaoverpay", userId)

		userSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: networkId,
			ClientId:  &clientId,
			UserId:    userId,
		})
		webhookSession := session.Testing_CreateClientSession(ctx, nil)

		reference := "webhook-overpay-1"
		intentResult, err := CreateSolanaPaymentIntent(&SolanaPaymentIntentArgs{
			Reference: reference,
			Plan:      model.SolanaPlanMonthly,
		}, userSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, intentResult.Error, nil)

		// 100 USDC clears the old hardcoded 40 threshold by a wide margin
		result, err := HeliusWebhook(
			[]*SolanaTransaction{solanaTestPayment(reference, "sig-overpay-1", 100)},
			webhookSession,
		)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Message, "Processed 1 matching payments")

		balances := model.GetActiveTransferBalances(ctx, networkId)
		connect.AssertEqual(t, len(balances), 1)
		// the plan QUOTED: a month, not the year the old webhook assumed
		connect.AssertEqual(t, balances[0].EndTime.Sub(balances[0].StartTime), 30*24*time.Hour+SubscriptionGracePeriod)
	})
}

// TestSolanaWebhookUnderpaymentTakesNothingAndKeepsTheQuote pins the refusal: $5 against
// a $40 yearly quote grants nothing -- no balance, no entitlement -- and does NOT
// consume the intent. The quote survives, so when the customer then pays the price they
// were quoted, the plan still lands, as the YEAR that was quoted.
func TestSolanaWebhookUnderpaymentTakesNothingAndKeepsTheQuote(t *testing.T) {
	skipWithoutProYml(t)

	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId := server.NewId()
		clientId := server.NewId()
		userId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "solanaunderpay", userId)

		userSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: networkId,
			ClientId:  &clientId,
			UserId:    userId,
		})
		webhookSession := session.Testing_CreateClientSession(ctx, nil)

		reference := "webhook-underpay-1"
		intentResult, err := CreateSolanaPaymentIntent(&SolanaPaymentIntentArgs{
			Reference: reference,
			Plan:      model.SolanaPlanYearly,
		}, userSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, intentResult.Error, nil)

		// $5 against the $40 quote: refused
		result, err := HeliusWebhook(
			[]*SolanaTransaction{solanaTestPayment(reference, "sig-underpay-1", 5)},
			webhookSession,
		)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Message, "Payment is less than the quoted price")

		// nothing granted
		connect.AssertEqual(t, len(model.GetActiveTransferBalances(ctx, networkId)), 0)
		connect.AssertEqual(t, model.IsProNetwork(ctx, networkId), false)

		// and the quote is still open -- the underpayment did not burn it
		search, err := model.SearchPaymentIntents([]string{reference}, userSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertNotEqual(t, search, nil)

		// paying the quoted price lands the plan that was quoted: a year
		result, err = HeliusWebhook(
			[]*SolanaTransaction{solanaTestPayment(reference, "sig-underpay-2", intentResult.AmountUsd)},
			webhookSession,
		)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Message, "Processed 1 matching payments")

		balances := model.GetActiveTransferBalances(ctx, networkId)
		connect.AssertEqual(t, len(balances), 1)
		connect.AssertEqual(t, balances[0].Pro, true)
		connect.AssertEqual(t, balances[0].EndTime.Sub(balances[0].StartTime), SubscriptionYearDuration+SubscriptionGracePeriod)
	})
}

// TestSolanaWebhookBatchSurvivesLeadingNoise pins the batch semantics: HeliusWebhook
// used to RETURN on the first non-matching transaction instead of continuing to the
// next one, so a real payment that arrived in the same Helius batch behind any
// unrelated transfer or swap was dropped -- money taken, nothing delivered, only an
// "Ignoring..." message logged. Every skip is now a `continue`, so a payment is
// found wherever it sits in the delivery.
func TestSolanaWebhookBatchSurvivesLeadingNoise(t *testing.T) {
	skipWithoutProYml(t)

	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId := server.NewId()
		clientId := server.NewId()
		userId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "solanabatch", userId)

		userSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: networkId,
			ClientId:  &clientId,
			UserId:    userId,
		})
		webhookSession := session.Testing_CreateClientSession(ctx, nil)

		reference := "webhook-batch-1"
		intentResult, err := CreateSolanaPaymentIntent(&SolanaPaymentIntentArgs{
			Reference: reference,
			Plan:      model.SolanaPlanMonthly,
		}, userSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, intentResult.Error, nil)

		// an unrelated TRANSFER ahead of the real payment: right type, but a token
		// transfer between strangers that has nothing to do with us. This is the
		// most ordinary leading noise a batch carries.
		unrelatedTransfer := &SolanaTransaction{
			Type:      "TRANSFER",
			Signature: "sig-batch-noise-transfer-1",
			TokenTransfers: []TokenTransfer{
				{
					Mint:            solanaUsdcMint,
					FromUserAccount: "SomeoneE1seAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
					ToUserAccount:   "NotOurAddressAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
					TokenAmount:     123,
				},
			},
		}
		// an unrelated swap too, for good measure
		noise := &SolanaTransaction{
			Type:      "SWAP",
			Signature: "sig-batch-noise-1",
		}
		payment := solanaTestPayment(reference, "sig-batch-1", intentResult.AmountUsd)

		result, err := HeliusWebhook([]*SolanaTransaction{unrelatedTransfer, noise, payment}, webhookSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Message, "Processed 1 matching payments")

		// the payment behind the noise still lands
		balances := model.GetActiveTransferBalances(ctx, networkId)
		connect.AssertEqual(t, len(balances), 1)
		connect.AssertEqual(t, balances[0].Pro, true)
		connect.AssertEqual(t, model.IsProNetwork(ctx, networkId), true)
	})
}

// TestSolanaWebhookLatePaymentStillCredits pins the late-payment half of S3: an
// intent EXPIRES an hour after creation but is only DELETED by the periodic sweep
// ~1-2h later. A payment landing in that gap still resolves the reference -- the
// search ignores expires_at on purpose -- and is credited. Late is not fraudulent.
func TestSolanaWebhookLatePaymentStillCredits(t *testing.T) {
	skipWithoutProYml(t)

	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId := server.NewId()
		clientId := server.NewId()
		userId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "solanalate", userId)

		userSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: networkId,
			ClientId:  &clientId,
			UserId:    userId,
		})
		webhookSession := session.Testing_CreateClientSession(ctx, nil)

		reference := "webhook-late-1"
		intentResult, err := CreateSolanaPaymentIntent(&SolanaPaymentIntentArgs{
			Reference: reference,
			Plan:      model.SolanaPlanMonthly,
		}, userSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, intentResult.Error, nil)

		// push the intent past its expiry, but do not sweep it
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`UPDATE solana_payment_intent SET expires_at = $2 WHERE payment_reference = $1`,
				reference,
				server.NowUtc().Add(-30*time.Minute),
			))
		})

		result, err := HeliusWebhook(
			[]*SolanaTransaction{solanaTestPayment(reference, "sig-late-1", intentResult.AmountUsd)},
			webhookSession,
		)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Message, "Processed 1 matching payments")

		// the late payment bought the plan that was quoted
		connect.AssertEqual(t, len(model.GetActiveTransferBalances(ctx, networkId)), 1)
		connect.AssertEqual(t, model.IsProNetwork(ctx, networkId), true)

		// ...and nothing was recorded as unfulfilled
		_, err = model.GetUnfulfilledSolanaPayment(ctx, "sig-late-1")
		connect.AssertNotEqual(t, err, nil)
	})
}

// TestSolanaWebhookRecordsUnmatchedAndUnderpaid pins the operator-visible record of
// S3: money that arrived and bought nothing must leave a row an operator can see
// and repair from -- not just a log line behind a 200.
func TestSolanaWebhookRecordsUnmatchedAndUnderpaid(t *testing.T) {
	skipWithoutProYml(t)

	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId := server.NewId()
		clientId := server.NewId()
		userId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "solanaunfulfilled", userId)

		userSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: networkId,
			ClientId:  &clientId,
			UserId:    userId,
		})
		webhookSession := session.Testing_CreateClientSession(ctx, nil)

		// a payment whose reference matches nothing (the intent was already swept)
		result, err := HeliusWebhook(
			[]*SolanaTransaction{solanaTestPayment("swept-reference-1", "sig-unmatched-1", 40)},
			webhookSession,
		)
		connect.AssertEqual(t, err, nil)
		// still acked 200 -- Helius never re-examines a delivered tx
		connect.AssertEqual(t, result.Message, "No payment intent found for this network ID")

		unmatched, err := model.GetUnfulfilledSolanaPayment(ctx, "sig-unmatched-1")
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, unmatched.Reason, model.SolanaUnfulfilledReasonNoIntent)
		connect.AssertEqual(t, unmatched.TokenAmountUsd, float64(40))

		// an underpayment against an open quote
		reference := "webhook-unfulfilled-underpay-1"
		intentResult, err := CreateSolanaPaymentIntent(&SolanaPaymentIntentArgs{
			Reference: reference,
			Plan:      model.SolanaPlanYearly,
		}, userSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, intentResult.Error, nil)

		result, err = HeliusWebhook(
			[]*SolanaTransaction{solanaTestPayment(reference, "sig-under-recorded-1", 5)},
			webhookSession,
		)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Message, "Payment is less than the quoted price")

		underpaid, err := model.GetUnfulfilledSolanaPayment(ctx, "sig-under-recorded-1")
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, underpaid.Reason, model.SolanaUnfulfilledReasonUnderpaid)
		connect.AssertEqual(t, underpaid.TokenAmountUsd, float64(5))
		connect.AssertEqual(t, *underpaid.ExpectedAmountUsd, intentResult.AmountUsd)
		connect.AssertEqual(t, *underpaid.PaymentReference, reference)
		connect.AssertEqual(t, *underpaid.NetworkId, networkId)

		// a REDELIVERY of a payment that already credited is NOT recorded as
		// unfulfilled: pay it properly, then deliver the same tx again
		result, err = HeliusWebhook(
			[]*SolanaTransaction{solanaTestPayment(reference, "sig-under-recorded-2", intentResult.AmountUsd)},
			webhookSession,
		)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Message, "Processed 1 matching payments")

		result, err = HeliusWebhook(
			[]*SolanaTransaction{solanaTestPayment(reference, "sig-under-recorded-2", intentResult.AmountUsd)},
			webhookSession,
		)
		connect.AssertEqual(t, err, nil)
		_, err = model.GetUnfulfilledSolanaPayment(ctx, "sig-under-recorded-2")
		connect.AssertNotEqual(t, err, nil)
	})
}
