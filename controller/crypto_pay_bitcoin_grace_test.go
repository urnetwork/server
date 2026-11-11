package controller

// Bitcoin's confirmation grace (cryptoBitcoinConfirmationGrace, mmm/BTCPAY.md
// §16.8), judged: the block scanner sees a payment only in its block, so a
// payment sent before its quote expired is first seen at its block's time,
// often after the expiry. A Bitcoin payment of the quoted amount whose block is
// within 3 hours of the expiry counts as sent within the quote and is credited
// at the quoted price; one mined later takes the late path every coin has
// (valued at its first-seen rate); the invoice stays open through the grace.
// Every other coin keeps its quote's end at its expiry, as before.
//
// Hermetic and deterministic: the clock is passed in, and the scanner's tests
// read the recorded mainnet blocks of crypto_pay_bitcoin_test.go's fake node.
// These tests name nothing the grace added, so they run against the rail
// before it too, where the Bitcoin ones fail.

import (
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server/model"
)

// The grace as these tests know it: 3 hours past a 30-minute quote.
const cryptoTestBtcGrace = 3 * time.Hour

// cryptoTestBtcIntent is a $40 Bitcoin year quoted at $73,596.90 with the
// 0.5% buffer: 54,622 sats, quoted at quotedAt, expiring 30 minutes later.
func cryptoTestBtcIntent(t testing.TB, quotedAt time.Time) *model.CryptoPaymentIntent {
	t.Helper()
	intent := cryptoTestCoinIntent(model.CryptoAssetBtc, "bc1q0wd209cv5k9pd9mhk7nspacywcj038xxdhnt5u", 54_622)
	amountAtomic, ok := cryptoAssetQuoteAmountAtomic(model.CryptoAssetBtc, 40, 73_596.90, 0.5)
	connect.AssertEqual(t, ok, true)
	connect.AssertEqual(t, amountAtomic, int64(54_622))
	intent.RateUsd = 73_596.90
	intent.QuotedAt = quotedAt
	intent.CreateTime = quotedAt
	intent.ExpiresAt = quotedAt.Add(30 * time.Minute)
	return intent
}

// cryptoTestMined is a receipt of a payment mined at minedAt, at
// confirmations, first seen at its block's time with the rate then: what the
// Bitcoin scanner records for it (no mempool sighting before).
func cryptoTestMined(intent *model.CryptoPaymentIntent, txId string, amountAtomic int64, confirmations int, minedAt time.Time, rateUsd float64) *model.CryptoPaymentReceipt {
	receipt := cryptoTestReceipt(intent, txId, amountAtomic, confirmations, minedAt, model.CryptoReceiptStateConfirmed)
	if 0 < rateUsd {
		rate := rateUsd
		receipt.FirstSeenRateUsd = &rate
	}
	return receipt
}

// The root cause, reproduced: a payment of exactly the quoted amount, sent as
// the quote expired and mined 2 hours 50 minutes after the expiry, while the
// price fell 5%. Bitcoin's block scan first sees it at its block, so the rail
// judged it late and valued it at the fallen rate ($38.19 of a $40 year, under
// the 99.5% that pays): review, late_underpaid, an honest payment not
// credited. It counts as sent within the quote now: credited at the price.
func TestCryptoBitcoinPaymentMinedInTheGraceIsCreditedAtTheQuote(t *testing.T) {
	quotedAt := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	intent := cryptoTestBtcIntent(t, quotedAt)
	policy := cryptoTestPolicy(model.CryptoAssetBtc)
	minedAt := intent.ExpiresAt.Add(2*time.Hour + 50*time.Minute)
	fallen := 73_596.90 * 0.95

	paid := cryptoTestMined(intent, "tx-grace", intent.AmountAtomic, 1, minedAt, fallen)
	view := cryptoEvaluateIntent(intent, []*model.CryptoPaymentReceipt{paid}, policy, minedAt.Add(time.Minute))
	connect.AssertEqual(t, view.ReviewReason, "")
	connect.AssertEqual(t, view.Action, cryptoIntentActionCredit)
	connect.AssertEqual(t, view.Late, false)
	connect.AssertEqual(t, view.CreditUsd, intent.PriceUsdNanoCents)
	connect.AssertEqual(t, view.CreditReceipts, cryptoTestKeys(paid))
	// each receipt's share of the quoted price, not of the fallen rate
	connect.AssertEqual(t, view.ReceiptUsdValue[paid.Key()], intent.PriceUsdNanoCents)
	connect.AssertEqual(t, view.NeedsRate, false)

	// whatever the rate did: one whose first-seen rate is not recorded yet
	// needs none
	unrated := cryptoTestMined(intent, "tx-grace", intent.AmountAtomic, 1, minedAt, 0)
	view = cryptoEvaluateIntent(intent, []*model.CryptoPaymentReceipt{unrated}, policy, minedAt.Add(time.Minute))
	connect.AssertEqual(t, view.NeedsRate, false)
	connect.AssertEqual(t, view.Action, cryptoIntentActionCredit)
	connect.AssertEqual(t, view.CreditUsd, intent.PriceUsdNanoCents)

	// below the credit depth it is confirming, and on time, not seen
	policyAtTwo := cryptoTestPolicy(model.CryptoAssetBtc)
	policyAtTwo.creditConfirmations = 2
	view = cryptoEvaluateIntent(intent, []*model.CryptoPaymentReceipt{paid}, policyAtTwo, minedAt.Add(time.Minute))
	connect.AssertEqual(t, view.Status, CryptoPaymentStatusConfirming)
	connect.AssertEqual(t, view.Late, false)
	connect.AssertEqual(t, view.Action, cryptoIntentActionNone)
	deeper := cryptoTestMined(intent, "tx-grace", intent.AmountAtomic, 2, minedAt, fallen)
	view = cryptoEvaluateIntent(intent, []*model.CryptoPaymentReceipt{deeper}, policyAtTwo, minedAt.Add(11*time.Minute))
	connect.AssertEqual(t, view.Action, cryptoIntentActionCredit)
	connect.AssertEqual(t, view.CreditUsd, intent.PriceUsdNanoCents)
}

// The same payment as the block scanner reads it from the recorded mainnet
// block 975401 (140,682 sats to a P2WPKH address, its block time
// 2026-11-11 07:08:17Z), on an invoice that expired 2 hours 50 minutes
// before that block: the receipt is first seen at the block's time, never at
// the scan's, and is credited at the quoted price, at 1 confirmation.
func TestCryptoBitcoinScannedPaymentMinedInTheGraceIsCreditedAtTheQuote(t *testing.T) {
	node := newCryptoTestBitcoinNode(t, 975401)
	endpoint := node.serve()
	state := cryptoTestBitcoinSeams(t, map[string]int64{cryptoTestBtcP2wpkhSingle: 11})
	blockTime := time.Unix(1794380897, 0).UTC()

	intent := cryptoTestCoinIntent(model.CryptoAssetBtc, cryptoTestBtcP2wpkhSingle, 140_682)
	index := int64(11)
	intent.AddressIndex = &index
	// $40 at $28,575.27 with the 0.5% buffer is 140,682 sats
	intent.RateUsd = 28_575.27
	intent.ExpiresAt = blockTime.Add(-(2*time.Hour + 50*time.Minute))
	intent.QuotedAt = intent.ExpiresAt.Add(-30 * time.Minute)
	intent.CreateTime = intent.QuotedAt
	intents := map[int64]*model.CryptoPaymentIntent{11: intent}

	result := state.scan(t, endpoint)
	connect.AssertEqual(t, result.err, nil)
	if len(result.receipts) != 1 {
		t.Fatalf("receipts = %d, want the one payment", len(result.receipts))
	}
	// the scan that reads it runs a minute after the block, with the price
	// down to $27,000 since the quote: $37.98 at that rate
	scannedAt := blockTime.Add(time.Minute)
	receipt := cryptoReceiptOf(model.CryptoAssetBtc, result.receipts[0], intents, scannedAt)
	connect.AssertEqual(t, receipt.FirstSeenAt, blockTime)
	connect.AssertEqual(t, receipt.State, model.CryptoReceiptStateConfirmed)
	rate := 27_000.0
	receipt.FirstSeenRateUsd = &rate

	view := cryptoEvaluateIntent(intent, []*model.CryptoPaymentReceipt{receipt}, cryptoTestPolicy(model.CryptoAssetBtc), scannedAt)
	connect.AssertEqual(t, view.ReviewReason, "")
	connect.AssertEqual(t, view.Action, cryptoIntentActionCredit)
	connect.AssertEqual(t, view.Late, false)
	connect.AssertEqual(t, view.Confirmations, 1)
	connect.AssertEqual(t, view.CreditUsd, intent.PriceUsdNanoCents)
}

// Past the grace a Bitcoin payment takes the late path every coin has: valued
// at its first-seen rate, credited at that value capped at the price when it
// covers 99.5% of the price, else held for review (late_underpaid); without a
// recorded rate the scanner records one first. The grace's end is inclusive,
// as the quote's expiry is.
func TestCryptoBitcoinPaymentMinedAfterTheGraceIsLate(t *testing.T) {
	quotedAt := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	intent := cryptoTestBtcIntent(t, quotedAt)
	policy := cryptoTestPolicy(model.CryptoAssetBtc)
	graceEnd := intent.ExpiresAt.Add(cryptoTestBtcGrace)
	fallen := 73_596.90 * 0.95
	risen := 73_596.90 * 1.02

	// mined at the grace's last second: on time
	atEnd := cryptoTestMined(intent, "tx-end", intent.AmountAtomic, 1, graceEnd, fallen)
	view := cryptoEvaluateIntent(intent, []*model.CryptoPaymentReceipt{atEnd}, policy, graceEnd.Add(time.Minute))
	connect.AssertEqual(t, view.Action, cryptoIntentActionCredit)
	connect.AssertEqual(t, view.Late, false)
	connect.AssertEqual(t, view.CreditUsd, intent.PriceUsdNanoCents)

	// a second later: late, and at the fallen rate short of the price
	late := cryptoTestMined(intent, "tx-late", intent.AmountAtomic, 1, graceEnd.Add(time.Second), fallen)
	view = cryptoEvaluateIntent(intent, []*model.CryptoPaymentReceipt{late}, policy, graceEnd.Add(time.Minute))
	connect.AssertEqual(t, view.Status, CryptoPaymentStatusReview)
	connect.AssertEqual(t, view.ReviewReason, model.CryptoReviewReasonLateUnderpaid)
	connect.AssertEqual(t, view.Action, cryptoIntentActionReview)
	connect.AssertEqual(t, view.ActionReason, model.CryptoReviewReasonLateUnderpaid)
	connect.AssertEqual(t, view.ReviewReceipts, cryptoTestKeys(late))

	// at a risen rate the late value covers the price: credited late, at the
	// value capped at the price
	lateRisen := cryptoTestMined(intent, "tx-late", intent.AmountAtomic, 1, graceEnd.Add(10*time.Minute), risen)
	view = cryptoEvaluateIntent(intent, []*model.CryptoPaymentReceipt{lateRisen}, policy, graceEnd.Add(11*time.Minute))
	connect.AssertEqual(t, view.Action, cryptoIntentActionCredit)
	connect.AssertEqual(t, view.Late, true)
	connect.AssertEqual(t, view.CreditUsd, intent.PriceUsdNanoCents)
	connect.AssertEqual(t, view.ReceiptUsdValue[lateRisen.Key()], cryptoAtomicUsdValueNanoCents(intent.AmountAtomic, 8, risen))

	// with no rate recorded yet the late part cannot be valued: seen, and the
	// scanner records the rate (crypto_pay_scan.go)
	unrated := cryptoTestMined(intent, "tx-late", intent.AmountAtomic, 1, graceEnd.Add(10*time.Minute), 0)
	view = cryptoEvaluateIntent(intent, []*model.CryptoPaymentReceipt{unrated}, policy, graceEnd.Add(11*time.Minute))
	connect.AssertEqual(t, view.NeedsRate, true)
	connect.AssertEqual(t, view.Status, CryptoPaymentStatusSeen)
	connect.AssertEqual(t, view.Action, cryptoIntentActionNone)
}

// A Bitcoin invoice stays open through its grace, since a payment sent within
// the quote may not be mined yet: pending with nothing received, seen while a
// part mined so far is short (a top-up mined in the grace completes it at the
// price), and expired, or held as underpaid, only once the grace ends. A
// credited Bitcoin invoice stays paid whatever comes after: no credit is taken
// back, and nothing is judged again.
func TestCryptoBitcoinInvoiceStaysOpenThroughTheGrace(t *testing.T) {
	quotedAt := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	intent := cryptoTestBtcIntent(t, quotedAt)
	policy := cryptoTestPolicy(model.CryptoAssetBtc)
	graceEnd := intent.ExpiresAt.Add(cryptoTestBtcGrace)

	for _, at := range []time.Time{quotedAt.Add(10 * time.Minute), intent.ExpiresAt, intent.ExpiresAt.Add(time.Minute), graceEnd} {
		view := cryptoEvaluateIntent(intent, nil, policy, at)
		connect.AssertEqual(t, view.Status, CryptoPaymentStatusPending)
		connect.AssertEqual(t, view.RemainingAtomic, intent.AmountAtomic)
	}
	view := cryptoEvaluateIntent(intent, nil, policy, graceEnd.Add(time.Second))
	connect.AssertEqual(t, view.Status, CryptoPaymentStatusExpired)
	connect.AssertEqual(t, view.Action, cryptoIntentActionNone)

	// a part mined in the window: seen through the grace, with what remains
	half := cryptoTestMined(intent, "tx-half", intent.AmountAtomic/2, 3, quotedAt.Add(20*time.Minute), 73_596.90)
	view = cryptoEvaluateIntent(intent, []*model.CryptoPaymentReceipt{half}, policy, intent.ExpiresAt.Add(time.Hour))
	connect.AssertEqual(t, view.Status, CryptoPaymentStatusSeen)
	connect.AssertEqual(t, view.RemainingAtomic, intent.AmountAtomic-intent.AmountAtomic/2)
	connect.AssertEqual(t, view.Action, cryptoIntentActionNone)
	// the top-up, mined two hours into the grace, makes it whole at the price
	topUp := cryptoTestMined(intent, "tx-topup", intent.AmountAtomic-intent.AmountAtomic/2, 1, intent.ExpiresAt.Add(2*time.Hour), 73_596.90*0.9)
	view = cryptoEvaluateIntent(intent, []*model.CryptoPaymentReceipt{half, topUp}, policy, intent.ExpiresAt.Add(2*time.Hour+time.Minute))
	connect.AssertEqual(t, view.Action, cryptoIntentActionCredit)
	connect.AssertEqual(t, view.Late, false)
	connect.AssertEqual(t, view.CreditUsd, intent.PriceUsdNanoCents)
	connect.AssertEqual(t, len(view.CreditReceipts), 2)
	// without it, the part is held as underpaid once the grace ends
	view = cryptoEvaluateIntent(intent, []*model.CryptoPaymentReceipt{half}, policy, graceEnd.Add(time.Second))
	connect.AssertEqual(t, view.Status, CryptoPaymentStatusReview)
	connect.AssertEqual(t, view.ReviewReason, model.CryptoReviewReasonUnderpaid)
	connect.AssertEqual(t, view.Action, cryptoIntentActionReview)

	// credited: paid, at any time, and a credited payment is never judged
	// again, so it is never taken back
	creditedAt := intent.ExpiresAt.Add(2 * time.Hour)
	credited := cryptoTestBtcIntent(t, quotedAt)
	credited.CreditedAt = &creditedAt
	receipt := cryptoTestMined(credited, "tx-credited", credited.AmountAtomic, 6, intent.ExpiresAt.Add(2*time.Hour), 73_596.90*0.5)
	receipt.State = model.CryptoReceiptStateCredited
	for _, at := range []time.Time{creditedAt, graceEnd.Add(time.Hour), graceEnd.Add(30 * 24 * time.Hour)} {
		view = cryptoEvaluateIntent(credited, []*model.CryptoPaymentReceipt{receipt}, policy, at)
		connect.AssertEqual(t, view.Status, CryptoPaymentStatusPaid)
		connect.AssertEqual(t, view.Action, cryptoIntentActionNone)
		connect.AssertEqual(t, view.ReceivedAtomic, credited.AmountAtomic)
	}
	// and the Bitcoin reader keeps its credits over a reorg
	connect.AssertEqual(t, cryptoTestBitcoinWallet("https://synthetic.invalid/v2/key").KeepsCredits(), true)
}

// Every other coin keeps its quote's end at its expiry, exactly as before:
// pending until expires_at and expired just past it with nothing received;
// a payment first seen at the expiry is on time and one a second past it is
// late, as is the one Bitcoin's grace credits at the quote (2 hours 50
// minutes past the expiry); a part short at the expiry is held as underpaid
// a minute later. USDC keeps its pending, paid and expired.
func TestCryptoOtherCoinsKeepTheirQuoteEnd(t *testing.T) {
	quotedAt := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	for i, asset := range []string{model.CryptoAssetXmr, model.CryptoAssetZec, model.CryptoAssetTao, model.CryptoAssetEth, model.CryptoAssetSol} {
		t.Run(asset, func(t *testing.T) {
			policy := cryptoTestPolicy(asset)
			intent := cryptoTestIntent(asset, int64(50+i), quotedAt)
			connect.AssertEqual(t, intent.ExpiresAt, quotedAt.Add(30*time.Minute))
			depth := int(policy.creditConfirmations)
			fallen := intent.RateUsd * 0.95
			risen := intent.RateUsd * 1.02

			connect.AssertEqual(t, cryptoEvaluateIntent(intent, nil, policy, intent.ExpiresAt).Status, CryptoPaymentStatusPending)
			view := cryptoEvaluateIntent(intent, nil, policy, intent.ExpiresAt.Add(time.Nanosecond))
			connect.AssertEqual(t, view.Status, CryptoPaymentStatusExpired)
			connect.AssertEqual(t, cryptoEvaluateIntent(intent, nil, policy, intent.ExpiresAt.Add(time.Minute)).Status, CryptoPaymentStatusExpired)

			// first seen at the expiry: on time, at the price, whatever the rate
			atExpiry := cryptoTestMined(intent, "tx-expiry", intent.AmountAtomic, depth, intent.ExpiresAt, fallen)
			view = cryptoEvaluateIntent(intent, []*model.CryptoPaymentReceipt{atExpiry}, policy, intent.ExpiresAt.Add(time.Hour))
			connect.AssertEqual(t, view.Action, cryptoIntentActionCredit)
			connect.AssertEqual(t, view.Late, false)
			connect.AssertEqual(t, view.CreditUsd, intent.PriceUsdNanoCents)

			// a second past it, or where Bitcoin's grace would still hold it:
			// late, at the first-seen rate
			for _, firstSeen := range []time.Time{intent.ExpiresAt.Add(time.Second), intent.ExpiresAt.Add(2*time.Hour + 50*time.Minute)} {
				late := cryptoTestMined(intent, "tx-late", intent.AmountAtomic, depth, firstSeen, fallen)
				view = cryptoEvaluateIntent(intent, []*model.CryptoPaymentReceipt{late}, policy, firstSeen.Add(time.Minute))
				connect.AssertEqual(t, view.Status, CryptoPaymentStatusReview)
				connect.AssertEqual(t, view.ReviewReason, model.CryptoReviewReasonLateUnderpaid)
				connect.AssertEqual(t, view.Action, cryptoIntentActionReview)
				lateRisen := cryptoTestMined(intent, "tx-late", intent.AmountAtomic, depth, firstSeen, risen)
				view = cryptoEvaluateIntent(intent, []*model.CryptoPaymentReceipt{lateRisen}, policy, firstSeen.Add(time.Minute))
				connect.AssertEqual(t, view.Action, cryptoIntentActionCredit)
				connect.AssertEqual(t, view.Late, true)
				connect.AssertEqual(t, view.CreditUsd, intent.PriceUsdNanoCents)
			}

			// short at the expiry: held as underpaid just past it
			half := cryptoTestMined(intent, "tx-half", intent.AmountAtomic/2, depth, quotedAt.Add(10*time.Minute), intent.RateUsd)
			connect.AssertEqual(t, cryptoEvaluateIntent(intent, []*model.CryptoPaymentReceipt{half}, policy, intent.ExpiresAt).Status, CryptoPaymentStatusSeen)
			view = cryptoEvaluateIntent(intent, []*model.CryptoPaymentReceipt{half}, policy, intent.ExpiresAt.Add(time.Minute))
			connect.AssertEqual(t, view.Status, CryptoPaymentStatusReview)
			connect.AssertEqual(t, view.ReviewReason, model.CryptoReviewReasonUnderpaid)
		})
	}

	t.Run(model.CryptoAssetUsdcSolana, func(t *testing.T) {
		policy := cryptoTestPolicy(model.CryptoAssetUsdcSolana)
		intent := cryptoTestIntent(model.CryptoAssetUsdcSolana, 60, quotedAt)
		connect.AssertEqual(t, cryptoEvaluateIntent(intent, nil, policy, intent.ExpiresAt).Status, CryptoPaymentStatusPending)
		connect.AssertEqual(t, cryptoEvaluateIntent(intent, nil, policy, intent.ExpiresAt.Add(time.Second)).Status, CryptoPaymentStatusExpired)
		// the webhook's own judgment: the price by the expiry, what arrived after
		usd, late := cryptoUsdcCreditUsd(intent, 39, intent.ExpiresAt)
		connect.AssertEqual(t, usd, intent.PriceUsdNanoCents)
		connect.AssertEqual(t, late, false)
		usd, late = cryptoUsdcCreditUsd(intent, 39, intent.ExpiresAt.Add(time.Second))
		connect.AssertEqual(t, usd, model.UsdToNanoCents(39))
		connect.AssertEqual(t, late, true)
	})
}
