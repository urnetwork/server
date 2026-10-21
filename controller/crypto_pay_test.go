package controller

// Hermetic tests of the crypto payment rail: no database and no network. The
// rate sources and the wallet daemons are fakes (crypto_pay_fake_test.go);
// the daemon clients run against local httptest servers that speak their wire
// formats. The database-backed paths are in crypto_pay_db_test.go.

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mr-tron/base58"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// ----- quote math -----

// The coin amount is price / rate x (1 + buffer), rounded UP to 6 decimals:
// ZCASH.md §4.5's own example, a $40 year at Kraken's 2026-10-05 prices.
func TestCryptoQuoteAmountRoundsUpToSixDecimals(t *testing.T) {
	for _, test := range []struct {
		name     string
		priceUsd float64
		rateUsd  float64
		asset    string
		want     int64
	}{
		{"xmr year", 40, 555.36, model.CryptoAssetXmr, 72_386_000_000},
		{"zec year", 40, 1304.60, model.CryptoAssetZec, 3_081_500},
		{"xmr regional year", 4, 555.36, model.CryptoAssetXmr, 7_239_000_000},
		{"zec 1 TiB", 3, 1304.60, model.CryptoAssetZec, 231_200},
		{"xmr 10 TiB", 20, 555.36, model.CryptoAssetXmr, 36_193_000_000},
		// exactly 6 decimals: nothing to round
		{"exact", 40, 400, model.CryptoAssetXmr, 100_500_000_000},
	} {
		got, ok := cryptoQuoteAmountAtomic(test.priceUsd, test.rateUsd, 0.5, cryptoAssetDecimals(test.asset))
		if !ok || got != test.want {
			t.Errorf("%s: amount = %d (%t), want %d", test.name, got, ok, test.want)
		}
		// at most 6 decimals, in coin units
		units := int64(1)
		for range cryptoAssetDecimals(test.asset) - cryptoQuoteDecimals {
			units *= 10
		}
		if got%units != 0 {
			t.Errorf("%s: %d has more than 6 decimals", test.name, got)
		}
	}
	// the buffer is what keeps a quote whole through its expiry: without it the
	// same year is less
	withoutBuffer, ok := cryptoQuoteAmountAtomic(40, 555.36, 0, 12)
	connect.AssertEqual(t, ok, true)
	connect.AssertEqual(t, withoutBuffer < 72_386_000_000, true)

	for _, bad := range []struct{ price, rate, buffer float64 }{
		{0, 555.36, 0.5}, {40, 0, 0.5}, {-40, 555.36, 0.5}, {40, -1, 0.5}, {40, 555.36, -1},
	} {
		if _, ok := cryptoQuoteAmountAtomic(bad.price, bad.rate, bad.buffer, 12); ok {
			t.Errorf("quoted %v at %v with buffer %v", bad.price, bad.rate, bad.buffer)
		}
	}
}

// A payment of 99.5% of the quote is paid (a wallet's rounding), rounded up.
func TestCryptoAcceptThreshold(t *testing.T) {
	connect.AssertEqual(t, cryptoAcceptThresholdAtomic(72_386_000_000, 99.5), int64(72_024_070_000))
	connect.AssertEqual(t, cryptoAcceptThresholdAtomic(1001, 99.5), int64(996))
	connect.AssertEqual(t, cryptoAcceptThresholdAtomic(1000, 100), int64(1000))
	connect.AssertEqual(t, cryptoAcceptThresholdAtomic(0, 99.5), int64(0))
}

// Display amounts are exact decimal strings without trailing zeros.
func TestCryptoFormatAtomic(t *testing.T) {
	for _, test := range []struct {
		atomic   int64
		decimals int
		want     string
	}{
		{72_386_000_000, 12, "0.072386"},
		{3_081_500, 8, "0.030815"},
		{40_000_202, 6, "40.000202"},
		{40_000_000, 6, "40"},
		{100_000_000_000, 12, "0.1"},
		{1, 12, "0.000000000001"},
		{0, 8, "0"},
		{2_500_000_000_000, 12, "2.5"},
	} {
		connect.AssertEqual(t, cryptoFormatAtomic(test.atomic, test.decimals), test.want)
	}
	// $40.0040 of XMR at $555.36, rounded down to nano cents
	value := cryptoAtomicUsdValueNanoCents(72_386_000_000, 12, 555.36)
	connect.AssertEqual(t, value > model.UsdToNanoCents(40.19), true)
	connect.AssertEqual(t, value < model.UsdToNanoCents(40.21), true)
}

// ----- fail-closed rates -----

func cryptoTestAnswer(source string, rate float64, fetchedAt time.Time) *cryptoRateAnswer {
	return &cryptoRateAnswer{Source: source, RateUsd: rate, FetchedAt: fetchedAt}
}

// A quote needs two fresh answers within 3% of each other, and is made at
// their median. One source, a stale one or a divergent pair refuses.
func TestCryptoRatesFailClosed(t *testing.T) {
	now := time.Date(2026, 10, 20, 18, 0, 0, 0, time.UTC)
	stale := 2 * time.Minute

	quote, reason := cryptoDecideRate([]*cryptoRateAnswer{
		cryptoTestAnswer("kraken", 555.0, now.Add(-10*time.Second)),
		cryptoTestAnswer("coingecko", 556.0, now.Add(-30*time.Second)),
	}, now, stale, 3)
	connect.AssertEqual(t, reason, "")
	connect.AssertEqual(t, quote.RateUsd, 555.5)
	connect.AssertEqual(t, len(quote.Answers), 2)

	// three: the median
	quote, reason = cryptoDecideRate([]*cryptoRateAnswer{
		cryptoTestAnswer("kraken", 1300, now),
		cryptoTestAnswer("coingecko", 1310, now),
		cryptoTestAnswer("coinbase", 1305, now),
	}, now, stale, 3)
	connect.AssertEqual(t, reason, "")
	connect.AssertEqual(t, quote.RateUsd, 1305.0)

	// one source
	_, reason = cryptoDecideRate([]*cryptoRateAnswer{cryptoTestAnswer("kraken", 555, now), nil}, now, stale, 3)
	connect.AssertEqual(t, reason, cryptoQuoteRefusedTooFew)

	// a stale answer does not count
	_, reason = cryptoDecideRate([]*cryptoRateAnswer{
		cryptoTestAnswer("kraken", 555, now),
		cryptoTestAnswer("coingecko", 555, now.Add(-121*time.Second)),
	}, now, stale, 3)
	connect.AssertEqual(t, reason, cryptoQuoteRefusedTooFew)

	// a source whose own time is old is stale, however recently it was asked
	// (a frozen CoinGecko price)
	sourceTime := now.Add(-5 * time.Minute)
	frozen := cryptoTestAnswer("coingecko", 555, now)
	frozen.SourceTime = &sourceTime
	_, reason = cryptoDecideRate([]*cryptoRateAnswer{cryptoTestAnswer("kraken", 555, now), frozen}, now, stale, 3)
	connect.AssertEqual(t, reason, cryptoQuoteRefusedTooFew)

	// divergent: 4% apart
	_, reason = cryptoDecideRate([]*cryptoRateAnswer{
		cryptoTestAnswer("kraken", 555, now),
		cryptoTestAnswer("coingecko", 577.2, now),
	}, now, stale, 3)
	connect.AssertEqual(t, reason, cryptoQuoteRefusedDivergent)

	// a stale divergent third answer is left out, so the fresh pair quotes
	quote, reason = cryptoDecideRate([]*cryptoRateAnswer{
		cryptoTestAnswer("kraken", 1300, now),
		cryptoTestAnswer("coingecko", 1302, now),
		cryptoTestAnswer("coinbase", 900, now.Add(-time.Hour)),
	}, now, stale, 3)
	connect.AssertEqual(t, reason, "")
	connect.AssertEqual(t, quote.RateUsd, 1301.0)

	// zero and negative rates never count
	_, reason = cryptoDecideRate([]*cryptoRateAnswer{
		cryptoTestAnswer("kraken", 0, now),
		cryptoTestAnswer("coingecko", 555, now),
	}, now, stale, 3)
	connect.AssertEqual(t, reason, cryptoQuoteRefusedTooFew)
}

// Each source is asked at most once a cache interval; a failing source is
// left alone for a backoff and the coin cannot be quoted from one source.
func TestCryptoRateCacheAndSourceFailure(t *testing.T) {
	config := cryptoTestConfig()
	config.normalize()
	var krakenCalls, coingeckoCalls atomic.Int32
	coingeckoFails := atomic.Bool{}
	previous := cryptoRateFetchers
	cryptoRateFetchers = map[string]cryptoRateFetch{
		cryptoRateSourceKraken: func(ctx context.Context, asset string) (float64, *time.Time, error) {
			krakenCalls.Add(1)
			return 555.36, nil, nil
		},
		cryptoRateSourceCoingecko: func(ctx context.Context, asset string) (float64, *time.Time, error) {
			coingeckoCalls.Add(1)
			if coingeckoFails.Load() {
				return 0, nil, errors.New("synthetic outage")
			}
			return 555.40, nil, nil
		},
	}
	now := time.Date(2026, 10, 20, 18, 0, 0, 0, time.UTC)
	previousNow := cryptoRateNow
	cryptoRateNow = func() time.Time { return now }
	cryptoRates.reset()
	t.Cleanup(func() {
		cryptoRateFetchers = previous
		cryptoRateNow = previousNow
		cryptoRates.reset()
	})

	ctx := context.Background()
	quote, reason := cryptoQuoteRate(ctx, config, model.CryptoAssetXmr)
	connect.AssertEqual(t, reason, "")
	connect.AssertEqual(t, quote.RateUsd, (555.36+555.40)/2)
	quote, _ = cryptoQuoteRate(ctx, config, model.CryptoAssetXmr)
	connect.AssertEqual(t, quote != nil, true)
	connect.AssertEqual(t, krakenCalls.Load(), int32(1))
	connect.AssertEqual(t, coingeckoCalls.Load(), int32(1))

	// past the cache interval CoinGecko fails: its last answer still counts
	// until it is two minutes old
	now = now.Add(61 * time.Second)
	coingeckoFails.Store(true)
	_, reason = cryptoQuoteRate(ctx, config, model.CryptoAssetXmr)
	connect.AssertEqual(t, reason, "")
	connect.AssertEqual(t, coingeckoCalls.Load(), int32(2))

	// within the failure backoff it is not asked again
	now = now.Add(5 * time.Second)
	cryptoQuoteRate(ctx, config, model.CryptoAssetXmr)
	connect.AssertEqual(t, coingeckoCalls.Load(), int32(2))

	// once its last answer is stale, XMR has one source and is not quoted
	now = now.Add(2 * time.Minute)
	_, reason = cryptoQuoteRate(ctx, config, model.CryptoAssetXmr)
	connect.AssertEqual(t, reason, cryptoQuoteRefusedTooFew)
}

// ----- the state of an invoice (§2.5 and ZCASH.md §4.10) -----

func cryptoTestPolicy(asset string) *cryptoIntentPolicy {
	config := cryptoTestConfig()
	config.normalize()
	return cryptoIntentPolicyFor(config, asset)
}

func cryptoTestKeys(receipts ...*model.CryptoPaymentReceipt) []model.CryptoPaymentReceiptKey {
	keys := []model.CryptoPaymentReceiptKey{}
	for _, receipt := range receipts {
		keys = append(keys, receipt.Key())
	}
	return keys
}

func TestCryptoEvaluateIntentStatuses(t *testing.T) {
	now := time.Date(2026, 10, 20, 18, 0, 0, 0, time.UTC)
	policy := cryptoTestPolicy(model.CryptoAssetXmr)
	intent := cryptoTestIntent(model.CryptoAssetXmr, 7, now)
	amount := intent.AmountAtomic
	threshold := cryptoAcceptThresholdAtomic(amount, 99.5)
	during := now.Add(10 * time.Minute)
	after := now.Add(40 * time.Minute)

	// pending: nothing yet
	view := cryptoEvaluateIntent(intent, nil, policy, during)
	connect.AssertEqual(t, view.Status, CryptoPaymentStatusPending)
	connect.AssertEqual(t, view.RemainingAtomic, amount)

	// expired: past the quote with nothing received
	view = cryptoEvaluateIntent(intent, nil, policy, after)
	connect.AssertEqual(t, view.Status, CryptoPaymentStatusExpired)
	connect.AssertEqual(t, view.Action, cryptoIntentActionNone)

	// seen: a partial payment in the mempool, with what remains to be sent
	partial := cryptoTestReceipt(intent, "tx-partial", amount/2, 0, now.Add(time.Minute), model.CryptoReceiptStateSeen)
	view = cryptoEvaluateIntent(intent, []*model.CryptoPaymentReceipt{partial}, policy, during)
	connect.AssertEqual(t, view.Status, CryptoPaymentStatusSeen)
	connect.AssertEqual(t, view.ReceivedAtomic, amount/2)
	connect.AssertEqual(t, view.RemainingAtomic, amount-amount/2)
	connect.AssertEqual(t, view.Action, cryptoIntentActionNone)

	// confirming: all of it in the mempool, never credited at 0 conf
	full := cryptoTestReceipt(intent, "tx-full", amount, 0, now.Add(time.Minute), model.CryptoReceiptStateSeen)
	view = cryptoEvaluateIntent(intent, []*model.CryptoPaymentReceipt{full}, policy, during)
	connect.AssertEqual(t, view.Status, CryptoPaymentStatusConfirming)
	connect.AssertEqual(t, view.Confirmations, 0)
	connect.AssertEqual(t, view.Action, cryptoIntentActionNone)

	// at 1 confirmation (xmr): credit at the price
	full = cryptoTestReceipt(intent, "tx-full", amount, 1, now.Add(time.Minute), model.CryptoReceiptStateConfirmed)
	view = cryptoEvaluateIntent(intent, []*model.CryptoPaymentReceipt{full}, policy, during)
	connect.AssertEqual(t, view.Action, cryptoIntentActionCredit)
	connect.AssertEqual(t, view.CreditUsd, intent.PriceUsdNanoCents)
	connect.AssertEqual(t, view.Late, false)
	connect.AssertEqual(t, view.CreditReceipts, cryptoTestKeys(full))

	// confirmations reach credit depth after expiry: still on time, at the price
	view = cryptoEvaluateIntent(intent, []*model.CryptoPaymentReceipt{full}, policy, after)
	connect.AssertEqual(t, view.Action, cryptoIntentActionCredit)
	connect.AssertEqual(t, view.CreditUsd, intent.PriceUsdNanoCents)

	// 99.5% of the quote is paid; a zatoshi less is not
	atThreshold := cryptoTestReceipt(intent, "tx-threshold", threshold, 1, now.Add(time.Minute), model.CryptoReceiptStateConfirmed)
	view = cryptoEvaluateIntent(intent, []*model.CryptoPaymentReceipt{atThreshold}, policy, during)
	connect.AssertEqual(t, view.Action, cryptoIntentActionCredit)
	short := cryptoTestReceipt(intent, "tx-short", threshold-1, 1, now.Add(time.Minute), model.CryptoReceiptStateConfirmed)
	view = cryptoEvaluateIntent(intent, []*model.CryptoPaymentReceipt{short}, policy, during)
	connect.AssertEqual(t, view.Status, CryptoPaymentStatusSeen)
	connect.AssertEqual(t, view.Action, cryptoIntentActionNone)
}

// ZCASH.md §4.10, row by row.
func TestCryptoEvaluateIntentUnderOverLatePartialExtra(t *testing.T) {
	now := time.Date(2026, 10, 20, 18, 0, 0, 0, time.UTC)
	policy := cryptoTestPolicy(model.CryptoAssetXmr)
	during := now.Add(10 * time.Minute)
	after := now.Add(40 * time.Minute)

	t.Run("underpaid within the window, topped up", func(t *testing.T) {
		intent := cryptoTestIntent(model.CryptoAssetXmr, 8, now)
		first := cryptoTestReceipt(intent, "tx-a", intent.AmountAtomic*3/4, 3, now.Add(time.Minute), model.CryptoReceiptStateConfirmed)
		topUp := cryptoTestReceipt(intent, "tx-b", intent.AmountAtomic/4, 0, now.Add(5*time.Minute), model.CryptoReceiptStateSeen)
		view := cryptoEvaluateIntent(intent, []*model.CryptoPaymentReceipt{first, topUp}, policy, during)
		// the top-up is in the mempool: confirming, and the payment's depth is
		// the shallowest receipt it needs
		connect.AssertEqual(t, view.Status, CryptoPaymentStatusConfirming)
		connect.AssertEqual(t, view.Confirmations, 0)
		connect.AssertEqual(t, view.Action, cryptoIntentActionNone)
		topUp = cryptoTestReceipt(intent, "tx-b", intent.AmountAtomic/4, 1, now.Add(5*time.Minute), model.CryptoReceiptStateConfirmed)
		view = cryptoEvaluateIntent(intent, []*model.CryptoPaymentReceipt{first, topUp}, policy, during)
		connect.AssertEqual(t, view.Action, cryptoIntentActionCredit)
		connect.AssertEqual(t, view.Confirmations, 1)
		connect.AssertEqual(t, len(view.CreditReceipts), 2)
	})

	t.Run("still short at expiry", func(t *testing.T) {
		intent := cryptoTestIntent(model.CryptoAssetXmr, 9, now)
		partial := cryptoTestReceipt(intent, "tx-a", intent.AmountAtomic/2, 4, now.Add(time.Minute), model.CryptoReceiptStateConfirmed)
		view := cryptoEvaluateIntent(intent, []*model.CryptoPaymentReceipt{partial}, policy, after)
		connect.AssertEqual(t, view.Status, CryptoPaymentStatusReview)
		connect.AssertEqual(t, view.ReviewReason, model.CryptoReviewReasonUnderpaid)
		connect.AssertEqual(t, view.Action, cryptoIntentActionReview)
		connect.AssertEqual(t, view.ActionReason, model.CryptoReviewReasonUnderpaid)
		connect.AssertEqual(t, view.ReviewReceipts, cryptoTestKeys(partial))
		// once held, judging again moves nothing
		partial.State = model.CryptoReceiptStateReview
		partial.ReviewReason = model.CryptoReviewReasonUnderpaid
		view = cryptoEvaluateIntent(intent, []*model.CryptoPaymentReceipt{partial}, policy, after)
		connect.AssertEqual(t, view.Status, CryptoPaymentStatusReview)
		connect.AssertEqual(t, view.Action, cryptoIntentActionNone)

		// a top-up after expiry that makes it whole at its first-seen rate
		// clears the review: credited at the value, capped at the price
		rate := 555.36
		topUp := cryptoTestReceipt(intent, "tx-b", intent.AmountAtomic, 1, after, model.CryptoReceiptStateConfirmed)
		topUp.FirstSeenRateUsd = &rate
		view = cryptoEvaluateIntent(intent, []*model.CryptoPaymentReceipt{partial, topUp}, policy, after)
		connect.AssertEqual(t, view.Action, cryptoIntentActionCredit)
		connect.AssertEqual(t, view.Late, true)
		connect.AssertEqual(t, view.CreditUsd, intent.PriceUsdNanoCents)
		connect.AssertEqual(t, len(view.CreditReceipts), 2)
	})

	t.Run("overpaid", func(t *testing.T) {
		intent := cryptoTestIntent(model.CryptoAssetXmr, 10, now)
		double := cryptoTestReceipt(intent, "tx-a", 2*intent.AmountAtomic, 1, now.Add(time.Minute), model.CryptoReceiptStateConfirmed)
		view := cryptoEvaluateIntent(intent, []*model.CryptoPaymentReceipt{double}, policy, during)
		connect.AssertEqual(t, view.Action, cryptoIntentActionCredit)
		// credited at the price; the surplus stays on the receipt, no refund
		connect.AssertEqual(t, view.CreditUsd, intent.PriceUsdNanoCents)
		connect.AssertEqual(t, view.ReceivedAtomic, 2*intent.AmountAtomic)
	})

	t.Run("late and enough", func(t *testing.T) {
		intent := cryptoTestIntent(model.CryptoAssetXmr, 11, now)
		// the rate rose: fewer coins than quoted are worth the price
		rate := 600.0
		late := cryptoTestReceipt(intent, "tx-a", intent.AmountAtomic*95/100, 1, after, model.CryptoReceiptStateConfirmed)
		late.FirstSeenRateUsd = &rate
		view := cryptoEvaluateIntent(intent, []*model.CryptoPaymentReceipt{late}, policy, after)
		connect.AssertEqual(t, view.Action, cryptoIntentActionCredit)
		connect.AssertEqual(t, view.Late, true)
		connect.AssertEqual(t, view.CreditUsd, intent.PriceUsdNanoCents)
	})

	t.Run("late and short", func(t *testing.T) {
		intent := cryptoTestIntent(model.CryptoAssetXmr, 12, now)
		// the rate fell 10%: the quoted coins are worth less than the price
		rate := 500.0
		late := cryptoTestReceipt(intent, "tx-a", intent.AmountAtomic, 2, after, model.CryptoReceiptStateConfirmed)
		late.FirstSeenRateUsd = &rate
		view := cryptoEvaluateIntent(intent, []*model.CryptoPaymentReceipt{late}, policy, after)
		connect.AssertEqual(t, view.Status, CryptoPaymentStatusReview)
		connect.AssertEqual(t, view.ReviewReason, model.CryptoReviewReasonLateUnderpaid)
		connect.AssertEqual(t, view.Action, cryptoIntentActionReview)
	})

	t.Run("late without a rate yet", func(t *testing.T) {
		intent := cryptoTestIntent(model.CryptoAssetXmr, 13, now)
		late := cryptoTestReceipt(intent, "tx-a", intent.AmountAtomic, 2, after, model.CryptoReceiptStateConfirmed)
		view := cryptoEvaluateIntent(intent, []*model.CryptoPaymentReceipt{late}, policy, after)
		connect.AssertEqual(t, view.NeedsRate, true)
		connect.AssertEqual(t, view.Status, CryptoPaymentStatusSeen)
		connect.AssertEqual(t, view.Action, cryptoIntentActionNone)
	})

	t.Run("a second payment to a paid invoice", func(t *testing.T) {
		intent := cryptoTestIntent(model.CryptoAssetXmr, 14, now)
		creditedAt := during
		intent.CreditedAt = &creditedAt
		paid := cryptoTestReceipt(intent, "tx-a", intent.AmountAtomic, 3, now.Add(time.Minute), model.CryptoReceiptStateCredited)
		extra := cryptoTestReceipt(intent, "tx-b", intent.AmountAtomic, 0, after, model.CryptoReceiptStateSeen)
		view := cryptoEvaluateIntent(intent, []*model.CryptoPaymentReceipt{paid, extra}, policy, after)
		// the buyer still sees paid; the extra waits for an operator, never a
		// second plan
		connect.AssertEqual(t, view.Status, CryptoPaymentStatusPaid)
		connect.AssertEqual(t, view.Action, cryptoIntentActionReview)
		connect.AssertEqual(t, view.ActionReason, model.CryptoReviewReasonExtra)
		connect.AssertEqual(t, view.ReviewReceipts, cryptoTestKeys(extra))
	})

	t.Run("a credited payment left the chain", func(t *testing.T) {
		intent := cryptoTestIntent(model.CryptoAssetXmr, 15, now)
		creditedAt := during
		intent.CreditedAt = &creditedAt
		orphaned := cryptoTestReceipt(intent, "tx-a", intent.AmountAtomic, 3, now.Add(time.Minute), model.CryptoReceiptStateReview)
		orphaned.ReviewReason = model.CryptoReviewReasonOrphaned
		view := cryptoEvaluateIntent(intent, []*model.CryptoPaymentReceipt{orphaned}, policy, after)
		connect.AssertEqual(t, view.Status, CryptoPaymentStatusReview)
		connect.AssertEqual(t, view.ReviewReason, model.CryptoReviewReasonOrphaned)
	})

	t.Run("dropped before its credit", func(t *testing.T) {
		intent := cryptoTestIntent(model.CryptoAssetXmr, 16, now)
		dropped := cryptoTestReceipt(intent, "tx-a", intent.AmountAtomic, 0, now.Add(time.Minute), model.CryptoReceiptStateOrphaned)
		view := cryptoEvaluateIntent(intent, []*model.CryptoPaymentReceipt{dropped}, policy, during)
		// not counted
		connect.AssertEqual(t, view.Status, CryptoPaymentStatusPending)
		connect.AssertEqual(t, view.ReceivedAtomic, int64(0))
	})

	t.Run("time-locked, double spend and transparent are not counted", func(t *testing.T) {
		intent := cryptoTestIntent(model.CryptoAssetXmr, 17, now)
		ignored := cryptoTestReceipt(intent, "tx-a", intent.AmountAtomic, 5, now.Add(time.Minute), model.CryptoReceiptStateIgnored)
		ignored.ReviewReason = model.CryptoReviewReasonLocked
		view := cryptoEvaluateIntent(intent, []*model.CryptoPaymentReceipt{ignored}, policy, during)
		connect.AssertEqual(t, view.Status, CryptoPaymentStatusPending)
		connect.AssertEqual(t, view.Action, cryptoIntentActionNone)
	})

	t.Run("voided", func(t *testing.T) {
		intent := cryptoTestIntent(model.CryptoAssetXmr, 18, now)
		voidedAt := during
		intent.VoidedAt = &voidedAt
		view := cryptoEvaluateIntent(intent, nil, policy, during)
		connect.AssertEqual(t, view.Status, CryptoPaymentStatusExpired)
		late := cryptoTestReceipt(intent, "tx-a", intent.AmountAtomic, 1, after, model.CryptoReceiptStateConfirmed)
		view = cryptoEvaluateIntent(intent, []*model.CryptoPaymentReceipt{late}, policy, after)
		connect.AssertEqual(t, view.Status, CryptoPaymentStatusReview)
		connect.AssertEqual(t, view.ReviewReason, model.CryptoReviewReasonVoided)
		connect.AssertEqual(t, view.Action, cryptoIntentActionReview)
		// once the scanner moved it to review/voided it still reads that way,
		// with what was received (mmm/ZCASH-CONTRACT.md §3.13.12)
		late.State = model.CryptoReceiptStateReview
		late.ReviewReason = model.CryptoReviewReasonVoided
		view = cryptoEvaluateIntent(intent, []*model.CryptoPaymentReceipt{late}, policy, after)
		connect.AssertEqual(t, view.Status, CryptoPaymentStatusReview)
		connect.AssertEqual(t, view.ReviewReason, model.CryptoReviewReasonVoided)
		connect.AssertEqual(t, view.ReceivedAtomic, intent.AmountAtomic)
		connect.AssertEqual(t, view.Action, cryptoIntentActionNone)
	})

	t.Run("held for an operator", func(t *testing.T) {
		// a credit taken back because its payment left the chain: the invoice
		// is not credited, its receipt in review/orphaned
		intent := cryptoTestIntent(model.CryptoAssetXmr, 21, now)
		orphaned := cryptoTestReceipt(intent, "tx-a", intent.AmountAtomic, 3, now.Add(time.Minute), model.CryptoReceiptStateReview)
		orphaned.ReviewReason = model.CryptoReviewReasonOrphaned
		view := cryptoEvaluateIntent(intent, []*model.CryptoPaymentReceipt{orphaned}, policy, after)
		connect.AssertEqual(t, view.Status, CryptoPaymentStatusReview)
		connect.AssertEqual(t, view.ReviewReason, model.CryptoReviewReasonOrphaned)
		connect.AssertEqual(t, view.Action, cryptoIntentActionNone)
		connect.AssertEqual(t, view.ReceivedAtomic, intent.AmountAtomic)
		// a credit that kept failing
		failed := cryptoTestReceipt(intent, "tx-b", intent.AmountAtomic, 3, now.Add(time.Minute), model.CryptoReceiptStateReview)
		failed.ReviewReason = model.CryptoReviewReasonCreditFailed
		view = cryptoEvaluateIntent(intent, []*model.CryptoPaymentReceipt{failed}, policy, after)
		connect.AssertEqual(t, view.Status, CryptoPaymentStatusReview)
		connect.AssertEqual(t, view.ReviewReason, model.CryptoReviewReasonCreditFailed)
		connect.AssertEqual(t, view.Action, cryptoIntentActionNone)
		// a payment arriving beside a held one is not credited past the operator
		topUp := cryptoTestReceipt(intent, "tx-c", intent.AmountAtomic, 5, now.Add(time.Minute), model.CryptoReceiptStateConfirmed)
		view = cryptoEvaluateIntent(intent, []*model.CryptoPaymentReceipt{failed, topUp}, policy, during)
		connect.AssertEqual(t, view.Status, CryptoPaymentStatusReview)
		connect.AssertEqual(t, view.Action, cryptoIntentActionNone)
	})

	t.Run("a double spend in the mempool counts once mined", func(t *testing.T) {
		intent := cryptoTestIntent(model.CryptoAssetXmr, 22, now)
		flagged := cryptoTestReceipt(intent, "tx-a", intent.AmountAtomic, 0, now.Add(time.Minute), model.CryptoReceiptStateSeen)
		flagged.ReviewReason = model.CryptoReviewReasonDoubleSpend
		view := cryptoEvaluateIntent(intent, []*model.CryptoPaymentReceipt{flagged}, policy, during)
		connect.AssertEqual(t, view.Status, CryptoPaymentStatusPending)
		connect.AssertEqual(t, view.ReceivedAtomic, int64(0))
		mined := cryptoTestReceipt(intent, "tx-a", intent.AmountAtomic, 1, now.Add(time.Minute), model.CryptoReceiptStateConfirmed)
		view = cryptoEvaluateIntent(intent, []*model.CryptoPaymentReceipt{mined}, policy, during)
		connect.AssertEqual(t, view.Action, cryptoIntentActionCredit)
		// and a flagged sighting is no second payment to a paid invoice
		paid := cryptoTestIntent(model.CryptoAssetXmr, 23, now)
		creditedAt := during
		paid.CreditedAt = &creditedAt
		credited := cryptoTestReceipt(paid, "tx-a", paid.AmountAtomic, 3, now.Add(time.Minute), model.CryptoReceiptStateCredited)
		second := cryptoTestReceipt(paid, "tx-b", paid.AmountAtomic, 0, now.Add(2*time.Minute), model.CryptoReceiptStateSeen)
		second.ReviewReason = model.CryptoReviewReasonDoubleSpend
		view = cryptoEvaluateIntent(paid, []*model.CryptoPaymentReceipt{credited, second}, policy, during)
		connect.AssertEqual(t, view.Status, CryptoPaymentStatusPaid)
		connect.AssertEqual(t, view.Action, cryptoIntentActionNone)
	})

	t.Run("zec credits at 10 confirmations", func(t *testing.T) {
		zecPolicy := cryptoTestPolicy(model.CryptoAssetZec)
		intent := cryptoTestIntent(model.CryptoAssetZec, 19, now)
		receipt := cryptoTestReceipt(intent, "tx-a", intent.AmountAtomic, 9, now.Add(time.Minute), model.CryptoReceiptStateConfirmed)
		view := cryptoEvaluateIntent(intent, []*model.CryptoPaymentReceipt{receipt}, zecPolicy, during)
		connect.AssertEqual(t, view.Status, CryptoPaymentStatusConfirming)
		connect.AssertEqual(t, view.Confirmations, 9)
		connect.AssertEqual(t, view.Action, cryptoIntentActionNone)
		receipt = cryptoTestReceipt(intent, "tx-a", intent.AmountAtomic, 10, now.Add(time.Minute), model.CryptoReceiptStateConfirmed)
		view = cryptoEvaluateIntent(intent, []*model.CryptoPaymentReceipt{receipt}, zecPolicy, during)
		connect.AssertEqual(t, view.Action, cryptoIntentActionCredit)
	})

	t.Run("usdc is pending, paid or expired", func(t *testing.T) {
		intent := cryptoTestIntent(model.CryptoAssetUsdcSolana, 20, now)
		view := cryptoEvaluateIntent(intent, nil, policy, during)
		connect.AssertEqual(t, view.Status, CryptoPaymentStatusPending)
		view = cryptoEvaluateIntent(intent, nil, policy, after)
		connect.AssertEqual(t, view.Status, CryptoPaymentStatusExpired)
		creditedAt := after
		intent.CreditedAt = &creditedAt
		view = cryptoEvaluateIntent(intent, nil, policy, after)
		connect.AssertEqual(t, view.Status, CryptoPaymentStatusPaid)
	})
}

// The status result carries the error codes of §2.7 with their statuses.
func TestCryptoStatusResultErrorCodes(t *testing.T) {
	now := time.Date(2026, 10, 20, 18, 0, 0, 0, time.UTC)
	config := cryptoTestConfig()
	config.normalize()
	intent := cryptoTestIntent(model.CryptoAssetXmr, 21, now)
	ctx := context.Background()

	result := cryptoStatusResult(ctx, config, intent, nil, "", now.Add(time.Hour))
	connect.AssertEqual(t, result.Status, CryptoPaymentStatusExpired)
	connect.AssertEqual(t, result.Error.Code, PurchaseErrorCodeQuoteExpired)

	partial := cryptoTestReceipt(intent, "tx-a", intent.AmountAtomic/2, 3, now.Add(time.Minute), model.CryptoReceiptStateReview)
	partial.ReviewReason = model.CryptoReviewReasonUnderpaid
	result = cryptoStatusResult(ctx, config, intent, []*model.CryptoPaymentReceipt{partial}, "", now.Add(time.Hour))
	connect.AssertEqual(t, result.Status, CryptoPaymentStatusReview)
	connect.AssertEqual(t, result.ReviewReason, model.CryptoReviewReasonUnderpaid)
	connect.AssertEqual(t, result.Error.Code, PurchaseErrorCodeUnderpaid)
	connect.AssertEqual(t, result.Remaining, cryptoFormatAtomic(intent.AmountAtomic-intent.AmountAtomic/2, 12))

	result = cryptoStatusResult(ctx, config, intent, nil, "", now)
	connect.AssertEqual(t, result.Status, CryptoPaymentStatusPending)
	connect.AssertEqual(t, result.Error, (*CryptoPaymentError)(nil))
	connect.AssertEqual(t, result.Amount, "0.072386")
	connect.AssertEqual(t, result.AmountAtomic, "72386000000")
	connect.AssertEqual(t, result.ConfirmationsRequired, 1)
	connect.AssertEqual(t, result.Plan, model.SolanaPlanYearly)
	connect.AssertEqual(t, result.ItemId, "")

	// the wire form names every field of §2.5
	resultJson, err := json.Marshal(result)
	connect.AssertEqual(t, err, nil)
	fields := map[string]any{}
	connect.AssertEqual(t, json.Unmarshal(resultJson, &fields), nil)
	for _, field := range []string{
		"intent_id", "asset", "plan", "item_id", "delivery", "network_name", "status", "review_reason",
		"amount", "amount_atomic", "received", "received_atomic", "remaining", "remaining_atomic",
		"confirmations", "confirmations_required", "expires_at", "credited_at", "balance_code",
	} {
		if _, ok := fields[field]; !ok {
			t.Errorf("status result lacks %s", field)
		}
	}
}

// ----- receipts as rows -----

func TestCryptoReceiptOfClassifies(t *testing.T) {
	now := time.Date(2026, 10, 20, 18, 0, 0, 0, time.UTC)
	intent := cryptoTestIntent(model.CryptoAssetXmr, 30, now)
	intents := map[int64]*model.CryptoPaymentIntent{30: intent}
	index := uint64(30)
	unknownIndex := uint64(31)

	receipt := cryptoReceiptOf(model.CryptoAssetXmr, &cryptoWalletReceipt{TxId: "a", OutputKey: "0.30", AddressIndex: &index, AmountAtomic: 5}, intents, now)
	connect.AssertEqual(t, receipt.State, model.CryptoReceiptStateSeen)
	connect.AssertEqual(t, *receipt.IntentId, intent.IntentId)

	receipt = cryptoReceiptOf(model.CryptoAssetXmr, &cryptoWalletReceipt{TxId: "a", OutputKey: "0.30", AddressIndex: &index, AmountAtomic: 5, BlockHeight: 900, Confirmations: 3}, intents, now)
	connect.AssertEqual(t, receipt.State, model.CryptoReceiptStateConfirmed)
	connect.AssertEqual(t, *receipt.BlockHeight, int64(900))

	receipt = cryptoReceiptOf(model.CryptoAssetXmr, &cryptoWalletReceipt{TxId: "a", OutputKey: "0.30", AddressIndex: &index, AmountAtomic: 5, UnlockTime: 1234}, intents, now)
	connect.AssertEqual(t, receipt.State, model.CryptoReceiptStateIgnored)
	connect.AssertEqual(t, receipt.ReviewReason, model.CryptoReviewReasonLocked)

	// a pool sighting flagged as a double spend is seen, and not counted
	// until it is mined (mmm/ZCASH-CONTRACT.md §3.13.3); the mined copy is never flagged
	receipt = cryptoReceiptOf(model.CryptoAssetXmr, &cryptoWalletReceipt{TxId: "a", OutputKey: "0.30", AddressIndex: &index, AmountAtomic: 5, DoubleSpend: true}, intents, now)
	connect.AssertEqual(t, receipt.State, model.CryptoReceiptStateSeen)
	connect.AssertEqual(t, receipt.ReviewReason, model.CryptoReviewReasonDoubleSpend)
	connect.AssertEqual(t, cryptoReceiptCounts(receipt), false)
	receipt = cryptoReceiptOf(model.CryptoAssetXmr, &cryptoWalletReceipt{TxId: "a", OutputKey: "0.30", AddressIndex: &index, AmountAtomic: 5, BlockHeight: 901, Confirmations: 2}, intents, now)
	connect.AssertEqual(t, receipt.State, model.CryptoReceiptStateConfirmed)
	connect.AssertEqual(t, receipt.ReviewReason, "")
	connect.AssertEqual(t, cryptoReceiptCounts(receipt), true)

	receipt = cryptoReceiptOf(model.CryptoAssetZec, &cryptoWalletReceipt{TxId: "a", OutputKey: "t:0", AmountAtomic: 5, Pool: "transparent"}, intents, now)
	connect.AssertEqual(t, receipt.State, model.CryptoReceiptStateIgnored)
	connect.AssertEqual(t, receipt.ReviewReason, model.CryptoReviewReasonTransparent)

	// an address no invoice was given, and one no index names (the primary
	// address, another account)
	receipt = cryptoReceiptOf(model.CryptoAssetXmr, &cryptoWalletReceipt{TxId: "a", OutputKey: "0.31", AddressIndex: &unknownIndex, AmountAtomic: 5}, intents, now)
	connect.AssertEqual(t, receipt.State, model.CryptoReceiptStateReview)
	connect.AssertEqual(t, receipt.ReviewReason, model.CryptoReviewReasonUnknownAddress)
	connect.AssertEqual(t, receipt.IntentId, (*server.Id)(nil))
	receipt = cryptoReceiptOf(model.CryptoAssetXmr, &cryptoWalletReceipt{TxId: "a", OutputKey: "0.0", AmountAtomic: 5}, intents, now)
	connect.AssertEqual(t, receipt.ReviewReason, model.CryptoReviewReasonUnknownAddress)
}

// On time is judged by chain time: a receipt is first seen at the daemon's
// time for it, never after the scan that saw it and never before its quote,
// so a scanner that was down never makes a payment late (mmm/ZCASH-CONTRACT.md
// §3.13.4).
func TestCryptoFirstSeenIsChainTime(t *testing.T) {
	quoted := time.Date(2026, 10, 20, 18, 0, 0, 0, time.UTC)
	intent := cryptoTestIntent(model.CryptoAssetXmr, 40, quoted)
	intents := map[int64]*model.CryptoPaymentIntent{40: intent}
	index := uint64(40)
	// the scanner was down for two hours: the block came 20 minutes into the
	// quote, the scan two hours after it
	scanned := quoted.Add(2 * time.Hour)
	blockTime := quoted.Add(20 * time.Minute)
	receipt := cryptoReceiptOf(model.CryptoAssetXmr, &cryptoWalletReceipt{
		TxId: "a", OutputKey: "0.40", AddressIndex: &index, AmountAtomic: uint64(intent.AmountAtomic),
		BlockHeight: 900, Confirmations: 30, Timestamp: blockTime,
	}, intents, scanned)
	connect.AssertEqual(t, receipt.FirstSeenAt, blockTime)
	view := cryptoEvaluateIntent(intent, []*model.CryptoPaymentReceipt{receipt}, cryptoTestPolicy(model.CryptoAssetXmr), scanned)
	connect.AssertEqual(t, view.Action, cryptoIntentActionCredit)
	connect.AssertEqual(t, view.Late, false)
	connect.AssertEqual(t, view.CreditUsd, intent.PriceUsdNanoCents)

	// never before the quote (a skewed clock), never after the scan (a block
	// time in the future), and the scan's time when the daemon gave none
	connect.AssertEqual(t, cryptoFirstSeenAt(quoted.Add(-time.Hour), intent, scanned), quoted)
	connect.AssertEqual(t, cryptoFirstSeenAt(scanned.Add(time.Hour), intent, scanned), scanned)
	connect.AssertEqual(t, cryptoFirstSeenAt(time.Time{}, intent, scanned), scanned)
	connect.AssertEqual(t, cryptoFirstSeenAt(blockTime, nil, scanned), blockTime)
}

// ----- payment URIs (§3.5) -----

func TestCryptoPaymentUris(t *testing.T) {
	now := time.Date(2026, 10, 20, 18, 0, 0, 0, time.UTC)
	xmr := cryptoTestIntent(model.CryptoAssetXmr, 40, now)
	connect.AssertEqual(t, cryptoPaymentUri(xmr), "monero:"+xmr.Address+"?tx_amount=0.072386&tx_description=URnetwork%20Pro%20yearly")

	zec := cryptoTestIntent(model.CryptoAssetZec, 41, now)
	zec.AmountAtomic = 3_081_500
	zec.Item = model.SolanaPlanYearlyOnboarding
	connect.AssertEqual(t, cryptoPaymentUri(zec), "zcash:"+zec.Address+"?amount=0.030815&message=URnetwork%20Pro%20yearly%20(welcome%20offer)")

	usdc := cryptoTestIntent(model.CryptoAssetUsdcSolana, 42, now)
	reference := cryptoNewSolanaReference()
	usdc.SolanaReference = &reference
	usdc.Address = solanaPaymentRecipient()
	usdc.AmountAtomic = 3_000_202
	usdc.Item = StripeItemData1Tib
	connect.AssertEqual(
		t,
		cryptoPaymentUri(usdc),
		"solana:"+solanaPaymentRecipient()+"?amount=3.000202&spl-token="+solanaUsdcMint+"&reference="+reference+"&label=URnetwork&message=URnetwork%201%20TiB%20data",
	)
	connect.AssertEqual(t, cryptoItemDescription(StripeItemData10Tib), "URnetwork 10 TiB data")

	// each scheme's escaping: ZIP-321 keeps ' + ~ ( ) literal and never writes
	// a space as '+'; Monero follows wallet2; Solana Pay is RFC 3986
	special := "a b+c&d=e?f#g'h(i)~j%k/l\"mé"
	connect.AssertEqual(t, cryptoZip321Escape(special), "a%20b+c%26d%3De%3Ff%23g'h(i)~j%25k%2Fl%22m%C3%A9")
	connect.AssertEqual(t, cryptoMoneroEscape(special), "a%20b%2Bc%26d=e?f%23g'h(i)%7Ej%25k/l%22m%C3%A9")
	connect.AssertEqual(t, cryptoUriEscape(special), "a%20b%2Bc%26d%3De%3Ff%23g%27h%28i%29~j%25k%2Fl%22m%C3%A9")

	// an address that could break out of the URI is never put in one
	broken := cryptoTestIntent(model.CryptoAssetXmr, 43, now)
	broken.Address = "8abc?tx_amount=0"
	connect.AssertEqual(t, cryptoPaymentUri(broken), "")
}

// The server's Solana Pay reference is a 32-byte key, base58, like the SDK's.
func TestCryptoSolanaReference(t *testing.T) {
	reference := cryptoNewSolanaReference()
	decoded, err := base58.Decode(reference)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, len(decoded), 32)
	connect.AssertEqual(t, cryptoNewSolanaReference() != reference, true)
}

// ----- status tokens -----

func TestCryptoStatusTokens(t *testing.T) {
	token, tokenHash := cryptoNewStatusToken()
	connect.AssertEqual(t, len(token), 43)
	raw, err := base64.RawURLEncoding.DecodeString(token)
	connect.AssertEqual(t, err, nil)
	sum := sha256.Sum256(raw)
	connect.AssertEqual(t, tokenHash, sum[:])

	presented, ok := cryptoStatusTokenHash(token)
	connect.AssertEqual(t, ok, true)
	intent := &model.CryptoPaymentIntent{StatusTokenHash: tokenHash}
	connect.AssertEqual(t, intent.MatchesStatusToken(presented), true)

	// a token handed to a later caller that reused the invoice works too
	other, otherHash := cryptoNewStatusToken()
	otherPresented, _ := cryptoStatusTokenHash(other)
	connect.AssertEqual(t, intent.MatchesStatusToken(otherPresented), false)
	intent.ExtraStatusTokenHashes = [][]byte{otherHash}
	connect.AssertEqual(t, intent.MatchesStatusToken(otherPresented), true)
	connect.AssertEqual(t, intent.MatchesStatusToken(presented), true)

	for _, bad := range []string{"", "short", token + "x", strings.Repeat("!", 43)} {
		if _, ok := cryptoStatusTokenHash(bad); ok {
			t.Errorf("accepted token %q", bad)
		}
	}
	// a malformed token or id is unknown before anything is read
	connect.AssertEqual(t, cryptoLoadIntentForToken(context.Background(), "not-an-id", token), (*model.CryptoPaymentIntent)(nil))
	connect.AssertEqual(t, cryptoLoadIntentForToken(context.Background(), server.NewId().String(), "bad"), (*model.CryptoPaymentIntent)(nil))
}

// ----- the endpoints' early refusals: the kill switch, the plan rules, the
// guest guard and the limiter -----

func cryptoTestHasAuthMethod(t testing.TB, hasAuth bool) {
	previous := purchaseHasAnyAuthMethod
	purchaseHasAnyAuthMethod = func(ctx context.Context, userId server.Id) bool { return hasAuth }
	t.Cleanup(func() { purchaseHasAnyAuthMethod = previous })
}

func TestCryptoIntentEarlyRefusals(t *testing.T) {
	config := cryptoTestConfig()
	config.Assets[model.CryptoAssetZec].Enabled = false
	cryptoTestEnable(t, config, map[string]*fakeCryptoWallet{model.CryptoAssetXmr: newFakeCryptoWallet(model.CryptoAssetXmr)})
	cryptoTestHasAuthMethod(t, false)
	clientSession := solanaTestPaymentSession(server.NewId())

	refusal := func(args *CreateCryptoPaymentIntentArgs) string {
		t.Helper()
		result, err := CreateCryptoPaymentIntent(args, clientSession)
		connect.AssertEqual(t, err, nil)
		if result.Error == nil {
			t.Fatalf("%+v was not refused", args)
		}
		connect.AssertEqual(t, result.Status, (*CryptoPaymentStatusResult)(nil))
		return result.Error.Code
	}

	connect.AssertEqual(t, refusal(&CreateCryptoPaymentIntentArgs{Asset: "btc", Plan: model.SolanaPlanYearly}), PurchaseErrorCodeInvalidRequest)
	// the kill switch
	connect.AssertEqual(t, refusal(&CreateCryptoPaymentIntentArgs{Asset: model.CryptoAssetZec, Plan: model.SolanaPlanYearly}), PurchaseErrorCodeAssetUnavailable)
	connect.AssertEqual(t, refusal(&CreateCryptoPaymentIntentArgs{Asset: model.CryptoAssetXmr}), PurchaseErrorCodeInvalidRequest)
	connect.AssertEqual(t, refusal(&CreateCryptoPaymentIntentArgs{Asset: model.CryptoAssetXmr, Plan: model.SolanaPlanYearly, ItemId: StripeItemData1Tib}), PurchaseErrorCodeInvalidRequest)
	// monthly is card only, even when the config lists it
	connect.AssertEqual(t, refusal(&CreateCryptoPaymentIntentArgs{Asset: model.CryptoAssetXmr, Plan: model.SolanaPlanMonthly}), PurchaseErrorCodePlanUnavailable)
	connect.AssertEqual(t, refusal(&CreateCryptoPaymentIntentArgs{Asset: model.CryptoAssetXmr, ItemId: "data_2tib"}), PurchaseErrorCodeItemUnavailable)
	// a legacy guest network buys no plan
	connect.AssertEqual(t, refusal(&CreateCryptoPaymentIntentArgs{Asset: model.CryptoAssetXmr, Plan: model.SolanaPlanYearly}), PurchaseErrorCodeGuestSignInRequired)

	// without sign-in: data packs only
	dataRefusal := func(args *CreateDataCryptoPaymentIntentArgs) string {
		t.Helper()
		result, err := CreateDataCryptoPaymentIntent(args, clientSession)
		connect.AssertEqual(t, err, nil)
		if result.Error == nil {
			t.Fatalf("%+v was not refused", args)
		}
		return result.Error.Code
	}
	connect.AssertEqual(t, dataRefusal(&CreateDataCryptoPaymentIntentArgs{Asset: model.CryptoAssetXmr, ItemId: StripeItemData1Tib, Plan: model.SolanaPlanYearly}), PurchaseErrorCodeInvalidRequest)
	connect.AssertEqual(t, dataRefusal(&CreateDataCryptoPaymentIntentArgs{Asset: model.CryptoAssetXmr}), PurchaseErrorCodeInvalidRequest)
	connect.AssertEqual(t, dataRefusal(&CreateDataCryptoPaymentIntentArgs{Asset: model.CryptoAssetXmr, ItemId: StripeItemData1Tib, Email: "not-an-email"}), PurchaseErrorCodeInvalidEmail)
	connect.AssertEqual(t, dataRefusal(&CreateDataCryptoPaymentIntentArgs{Asset: model.CryptoAssetZec, ItemId: StripeItemData1Tib}), PurchaseErrorCodeAssetUnavailable)

	// a coin that does not deliver balance codes refuses a data pack without
	// a network, and still sells one for a named network (mmm/ZCASH-CONTRACT.md §3.13.14)
	codeDelivery := false
	cryptoPayConfigFunc().Assets[model.CryptoAssetXmr].CodeDelivery = &codeDelivery
	connect.AssertEqual(t, dataRefusal(&CreateDataCryptoPaymentIntentArgs{Asset: model.CryptoAssetXmr, ItemId: StripeItemData1Tib}), PurchaseErrorCodeAssetUnavailable)
	connect.AssertEqual(t, dataRefusal(&CreateDataCryptoPaymentIntentArgs{Asset: model.CryptoAssetXmr, ItemId: StripeItemData1Tib, NetworkName: ""}), PurchaseErrorCodeAssetUnavailable)
	cryptoPayConfigFunc().Assets[model.CryptoAssetXmr].CodeDelivery = nil

	// a coin switched on without its daemon's vault file is off
	cryptoWalletSettingsFunc = func(asset string) (*cryptoWalletSettings, error) {
		return nil, os.ErrNotExist
	}
	connect.AssertEqual(t, refusal(&CreateCryptoPaymentIntentArgs{Asset: model.CryptoAssetXmr, Plan: model.SolanaPlanYearly}), PurchaseErrorCodeAssetUnavailable)
	connect.AssertEqual(t, len(cryptoPaymentAssets(cryptoPayConfigFunc())), 1)
}

// enabled gates selling only: a coin switched off is not sold but is still
// scanned while its daemon is configured (mmm/ZCASH-CONTRACT.md §3.13.1). Selling xmr
// needs the wallet file's password and monerod (§3.13.9, §3.13.10), selling
// zec needs zebrad except on regtest.
func TestCryptoKillSwitchGatesSellingOnly(t *testing.T) {
	config := cryptoTestConfig()
	cryptoTestEnable(t, config, map[string]*fakeCryptoWallet{})
	settings := map[string]*cryptoWalletSettings{}
	cryptoWalletSettingsFunc = func(asset string) (*cryptoWalletSettings, error) {
		if found, ok := settings[asset]; ok {
			return found, nil
		}
		return nil, os.ErrNotExist
	}
	full := func(asset string) *cryptoWalletSettings {
		return &cryptoWalletSettings{
			Asset: asset, WalletRpcUrl: "http://127.0.0.1:1/", WalletRpcUser: "u", WalletRpcPass: "p",
			PrimaryAddress: "5synthetic", WalletPassword: "w", NodeRpcUrl: "http://127.0.0.1:2/",
		}
	}
	settings[model.CryptoAssetXmr] = full(model.CryptoAssetXmr)
	settings[model.CryptoAssetZec] = full(model.CryptoAssetZec)
	connect.AssertEqual(t, cryptoAssetEnabled(cryptoPayConfigFunc(), model.CryptoAssetXmr), true)
	connect.AssertEqual(t, cryptoAssetEnabled(cryptoPayConfigFunc(), model.CryptoAssetZec), true)

	// switched off: not sold, still scanned
	cryptoPayConfigFunc().Assets[model.CryptoAssetXmr].Enabled = false
	connect.AssertEqual(t, cryptoAssetEnabled(cryptoPayConfigFunc(), model.CryptoAssetXmr), false)
	connect.AssertEqual(t, cryptoAssetScannable(cryptoPayConfigFunc(), model.CryptoAssetXmr), nil)
	cryptoPayConfigFunc().Assets[model.CryptoAssetXmr].Enabled = true

	// no wallet file password, no monerod: scanned, not sold
	noPassword := full(model.CryptoAssetXmr)
	noPassword.WalletPassword = ""
	settings[model.CryptoAssetXmr] = noPassword
	connect.AssertEqual(t, cryptoAssetEnabled(cryptoPayConfigFunc(), model.CryptoAssetXmr), false)
	connect.AssertEqual(t, cryptoAssetScannable(cryptoPayConfigFunc(), model.CryptoAssetXmr), nil)
	noDaemon := full(model.CryptoAssetXmr)
	noDaemon.NodeRpcUrl = ""
	settings[model.CryptoAssetXmr] = noDaemon
	connect.AssertEqual(t, cryptoAssetEnabled(cryptoPayConfigFunc(), model.CryptoAssetXmr), false)
	connect.AssertEqual(t, cryptoAssetScannable(cryptoPayConfigFunc(), model.CryptoAssetXmr), nil)

	// no zebrad: not sold, except on regtest
	noNode := full(model.CryptoAssetZec)
	noNode.NodeRpcUrl = ""
	settings[model.CryptoAssetZec] = noNode
	connect.AssertEqual(t, cryptoAssetEnabled(cryptoPayConfigFunc(), model.CryptoAssetZec), false)
	cryptoPayConfigFunc().Assets[model.CryptoAssetZec].Network = cryptoNetworkRegtest
	connect.AssertEqual(t, cryptoAssetEnabled(cryptoPayConfigFunc(), model.CryptoAssetZec), true)

	// no vault file: neither sold nor scanned
	delete(settings, model.CryptoAssetXmr)
	connect.AssertEqual(t, cryptoAssetEnabled(cryptoPayConfigFunc(), model.CryptoAssetXmr), false)
	connect.AssertEqual(t, cryptoAssetScannable(cryptoPayConfigFunc(), model.CryptoAssetXmr) != nil, true)
	connect.AssertEqual(t, cryptoAssetScannable(cryptoPayConfigFunc(), model.CryptoAssetUsdcSolana) != nil, true)

	// a config without the scanned coins still scans them with the defaults
	empty := &CryptoPayConfig{}
	empty.normalize()
	connect.AssertEqual(t, empty.Asset(model.CryptoAssetXmr).Enabled, false)
	connect.AssertEqual(t, empty.Asset(model.CryptoAssetXmr).CreditConfirmations, int64(1))
	connect.AssertEqual(t, empty.Asset(model.CryptoAssetZec).MaxLagBlocksAfterNu7, int64(60))
	connect.AssertEqual(t, empty.AddressPoolTarget, 200)
	connect.AssertEqual(t, empty.MaxOpenCodeIntentsPerAsset, 100)
	connect.AssertEqual(t, empty.Asset(model.CryptoAssetXmr).CodeDeliveryEnabled(), true)
}

// The coins on offer: only those that are on, never monthly, never an item the
// config names that does not exist.
func TestCryptoPaymentAssetsList(t *testing.T) {
	config := cryptoTestConfig()
	config.Assets[model.CryptoAssetZec].Items = append(config.Assets[model.CryptoAssetZec].Items, "data_2tib")
	cryptoTestEnable(t, config, map[string]*fakeCryptoWallet{})
	assets := cryptoPaymentAssets(cryptoPayConfigFunc())
	connect.AssertEqual(t, len(assets), 3)
	byAsset := map[string]*CryptoPaymentAsset{}
	for _, asset := range assets {
		byAsset[asset.Asset] = asset
	}
	connect.AssertEqual(t, byAsset[model.CryptoAssetXmr].Plans, []string{model.SolanaPlanYearly, model.SolanaPlanYearlyOnboarding})
	connect.AssertEqual(t, byAsset[model.CryptoAssetZec].Items, []string{StripeItemData1Tib, StripeItemData10Tib})
	connect.AssertEqual(t, byAsset[model.CryptoAssetZec].ConfirmationsRequired, 10)
	connect.AssertEqual(t, byAsset[model.CryptoAssetXmr].QuoteTtlSeconds, 1800)
	connect.AssertEqual(t, byAsset[model.CryptoAssetUsdcSolana].QuoteTtlSeconds, 3600)
	connect.AssertEqual(t, byAsset[model.CryptoAssetUsdcSolana].ConfirmationsRequired, 1)

	// a coin that sells no plan still sends its plans as [], never null
	config.Assets[model.CryptoAssetZec].Plans = nil
	assetsJson, err := json.Marshal(&CryptoPaymentAssetsResult{Assets: cryptoPaymentAssets(cryptoPayConfigFunc())})
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, strings.Contains(string(assetsJson), `"asset":"zec","plans":[],"items":["data_1tib","data_10tib"]`), true)
	connect.AssertEqual(t, strings.Contains(string(assetsJson), "null"), false)
	// a coin that sells nothing is not listed at all
	config.Assets[model.CryptoAssetZec].Items = nil
	connect.AssertEqual(t, len(cryptoPaymentAssets(cryptoPayConfigFunc())), 2)
	config.Assets[model.CryptoAssetZec].Plans = []string{model.SolanaPlanYearly, model.SolanaPlanYearlyOnboarding}
	config.Assets[model.CryptoAssetZec].Items = []string{StripeItemData1Tib, StripeItemData10Tib}

	// the kill switch
	config.Assets[model.CryptoAssetXmr].Enabled = false
	assets = cryptoPaymentAssets(cryptoPayConfigFunc())
	connect.AssertEqual(t, len(assets), 2)

	// the balance result rides along, and leaves it out when nothing is on
	result := &SubscriptionBalanceResult{}
	decorateCryptoPay(result, &session.ClientSession{Ctx: context.Background()})
	connect.AssertEqual(t, len(result.CryptoAssets), 2)
	setCryptoPayConfigForTest(&CryptoPayConfig{})
	result = &SubscriptionBalanceResult{}
	decorateCryptoPay(result, &session.ClientSession{Ctx: context.Background()})
	resultJson, _ := json.Marshal(result)
	connect.AssertEqual(t, strings.Contains(string(resultJson), "crypto_assets"), false)
	connect.AssertEqual(t, strings.Contains(string(resultJson), "crypto_term"), false)
}

// A missing or malformed crypto_pay.yml is every coin off.
func TestCryptoPayConfigAbsentOrMalformedIsOff(t *testing.T) {
	pop := server.Config.PushSimpleResource("crypto_pay.yml", []byte("crypto_pay: [\n"))
	defer pop()
	resource, err := server.Config.SimpleResource("crypto_pay.yml")
	connect.AssertEqual(t, err, nil)
	config := parseCryptoPayConfig(resource)
	connect.AssertEqual(t, len(cryptoEnabledAssets(config)), 0)

	empty := &CryptoPayConfig{}
	empty.normalize()
	connect.AssertEqual(t, len(cryptoEnabledAssets(empty)), 0)
	connect.AssertEqual(t, empty.Quote.Expiry, 30*time.Minute)
	connect.AssertEqual(t, empty.Quote.AcceptPercent, 99.5)
	connect.AssertEqual(t, empty.UnlinkAfter, 720*time.Hour)
}

// The shipped config files parse with USDC on. Main sells Monero and Zcash
// since the owner's launch go of 2026-10-21 (mmm/ZCASH.md §0.7); local keeps
// them off, since a developer machine has no wallet daemons.
func TestCryptoPayConfigFiles(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	for _, env := range []string{"local", "main"} {
		path := filepath.Join(filepath.Dir(thisFile), "..", "..", "config", env, "crypto_pay.yml")
		data, err := os.ReadFile(path)
		if err != nil {
			t.Skipf("no config checkout beside the server: %s", err)
		}
		pop := server.Config.PushSimpleResource("crypto_pay.yml", data)
		resource, err := server.Config.SimpleResource("crypto_pay.yml")
		connect.AssertEqual(t, err, nil)
		config := parseCryptoPayConfig(resource)
		pop()
		connect.AssertEqual(t, config.Asset(model.CryptoAssetUsdcSolana).Enabled, true)
		connect.AssertEqual(t, config.Asset(model.CryptoAssetXmr).Enabled, env == "main")
		connect.AssertEqual(t, config.Asset(model.CryptoAssetZec).Enabled, env == "main")
		connect.AssertEqual(t, config.Asset(model.CryptoAssetXmr).CreditConfirmations, int64(1))
		connect.AssertEqual(t, config.Asset(model.CryptoAssetZec).CreditConfirmations, int64(10))
		connect.AssertEqual(t, config.Quote.MaxInvoiceUsd, float64(100))
		connect.AssertEqual(t, config.ScanInterval, 20*time.Second)
		connect.AssertEqual(t, config.MaxLagBlocks[model.CryptoAssetZec], int64(20))
		connect.AssertEqual(t, config.Asset(model.CryptoAssetZec).MaxLagBlocksAfterNu7, int64(60))
		connect.AssertEqual(t, config.AddressPoolTarget, 200)
		connect.AssertEqual(t, config.MaxOpenCodeIntentsPerAsset, 100)
		if env == "main" {
			connect.AssertEqual(t, config.Asset(model.CryptoAssetXmr).Network, cryptoNetworkMainnet)
			// no USDC codes until every server runs a build with migration 853
			connect.AssertEqual(t, config.Asset(model.CryptoAssetUsdcSolana).CodeDeliveryEnabled(), false)
			connect.AssertEqual(t, config.Asset(model.CryptoAssetXmr).CodeDeliveryEnabled(), true)
		} else {
			connect.AssertEqual(t, config.Asset(model.CryptoAssetUsdcSolana).CodeDeliveryEnabled(), true)
			connect.AssertEqual(t, config.Asset(model.CryptoAssetXmr).Network, cryptoNetworkStagenet)
			connect.AssertEqual(t, config.Asset(model.CryptoAssetZec).FinalConfirmationsAt(4_465_026), int64(72))
			connect.AssertEqual(t, config.Asset(model.CryptoAssetZec).FinalConfirmationsAt(4_465_025), int64(24))
		}
	}
}

// Ten intents a minute per network on the signed-in endpoints; another network
// has its own budget.
func TestCryptoNetworkLimiter(t *testing.T) {
	networkId := server.NewId()
	for i := 0; i < cryptoPayNetworkIntentsPerMinute; i += 1 {
		connect.AssertEqual(t, allowNetworkIntent(networkId), true)
	}
	connect.AssertEqual(t, allowNetworkIntent(networkId), false)
	connect.AssertEqual(t, allowNetworkIntent(server.NewId()), true)

	// the per-address limiter of the no-sign-in endpoints: a fixed window
	limiter := &payDataIpLimiter{limitPerMinute: 2}
	key := sha256.Sum256([]byte("synthetic-address"))
	now := time.Date(2026, 10, 20, 18, 0, 0, 0, time.UTC)
	connect.AssertEqual(t, limiter.allowHash(key, now), true)
	connect.AssertEqual(t, limiter.allowHash(key, now), true)
	connect.AssertEqual(t, limiter.allowHash(key, now), false)
	connect.AssertEqual(t, limiter.allowHash(key, now.Add(time.Minute)), true)

	// code invoices (no sign-in): a few an hour per address, hashed in memory
	// only (mmm/ZCASH-CONTRACT.md §3.13.15)
	codeLimiter := &cryptoWindowLimiter{limit: cryptoCodeIntentsPerAddressPerHour, window: time.Hour}
	other := sha256.Sum256([]byte("synthetic-other-address"))
	for i := 0; i < cryptoCodeIntentsPerAddressPerHour; i += 1 {
		connect.AssertEqual(t, codeLimiter.allowHash(key, now.Add(time.Duration(i)*time.Minute)), true)
	}
	connect.AssertEqual(t, codeLimiter.allowHash(key, now.Add(30*time.Minute)), false)
	connect.AssertEqual(t, codeLimiter.allowHash(other, now.Add(30*time.Minute)), true)
	connect.AssertEqual(t, codeLimiter.allowHash(key, now.Add(time.Hour)), true)
	// nothing kept but hashes
	for stored := range codeLimiter.counts {
		connect.AssertEqual(t, stored == key || stored == other, true)
	}
}

// ----- the USDC adapter -----

// A generic USDC invoice is a Solana Pay intent with the invoice linked to it:
// the merchant address, a fresh 32-byte reference as the memo, the amount in
// micro-USDC, rate 1, and the Solana Pay URI the server builds.
func TestCryptoUsdcAdapterLinksASolanaIntent(t *testing.T) {
	config := cryptoTestConfig()
	cryptoTestEnable(t, config, map[string]*fakeCryptoWallet{})
	var created *model.CryptoPaymentIntent
	var createdPrice float64
	previous := cryptoCreateLinkedSolanaIntent
	cryptoCreateLinkedSolanaIntent = func(ctx context.Context, intent *model.CryptoPaymentIntent, priceUsd float64) error {
		created = intent
		createdPrice = priceUsd
		intent.AmountAtomic = model.SolanaUsdToMicro(priceUsd) + 202
		return nil
	}
	t.Cleanup(func() { cryptoCreateLinkedSolanaIntent = previous })

	networkId := server.NewId()
	clientSession := solanaTestPaymentSession(networkId)
	now := server.NowUtc()
	result := cryptoStartUsdcIntent(clientSession, cryptoPayConfigFunc(), &cryptoIntentRequest{
		asset:      model.CryptoAssetUsdcSolana,
		item:       StripeItemData1Tib,
		delivery:   model.CryptoDeliveryNetwork,
		networkId:  &networkId,
		priceUsd:   3,
		regularUsd: 3,
	}, now)
	connect.AssertEqual(t, result.Error, (*CryptoPaymentError)(nil))
	connect.AssertEqual(t, createdPrice, float64(3))
	connect.AssertEqual(t, created.Address, solanaPaymentRecipient())
	connect.AssertEqual(t, created.RateUsd, float64(1))
	connect.AssertEqual(t, *created.NetworkId, networkId)
	decoded, err := base58.Decode(*created.SolanaReference)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, len(decoded), 32)
	connect.AssertEqual(t, result.Memo, *created.SolanaReference)
	connect.AssertEqual(t, result.Amount, "3.000202")
	connect.AssertEqual(t, result.AmountAtomic, "3000202")
	connect.AssertEqual(t, result.RateUsd, float64(1))
	connect.AssertEqual(t, strings.HasPrefix(result.Uri, "solana:"+solanaPaymentRecipient()+"?amount=3.000202&spl-token="+solanaUsdcMint+"&reference="+*created.SolanaReference), true)
	connect.AssertEqual(t, result.Status.Status, CryptoPaymentStatusPending)
	connect.AssertEqual(t, result.ExpiresAt.Sub(now), time.Hour)
	connect.AssertEqual(t, len(result.StatusToken), 43)
}

// The Helius webhook's credit hands a linked intent to the generic credit and
// keeps its own path for the old endpoints' intents.
func TestCryptoLinkedSolanaIntentHandOff(t *testing.T) {
	previous := cryptoLinkedSolanaIntentLookup
	t.Cleanup(func() { cryptoLinkedSolanaIntentLookup = previous })
	clientSession := solanaTestPaymentSession(server.NewId())
	search := &model.PaymentIntentSearchResult{PaymentReference: "synthetic-reference", ExpectedAmountUsd: 40}

	cryptoLinkedSolanaIntentLookup = func(connOwner server.PgConn, clientSession *session.ClientSession, reference string) *model.CryptoPaymentIntent {
		return nil
	}
	linked, credited, err := cryptoCreditLinkedSolanaIntentInConn(nil, clientSession, search, "sig", 40)
	connect.AssertEqual(t, linked, false)
	connect.AssertEqual(t, credited, false)
	connect.AssertEqual(t, err, nil)

	// a redelivery of the payment that credited it: linked, nothing credited
	creditedAt := server.NowUtc()
	cryptoLinkedSolanaIntentLookup = func(connOwner server.PgConn, clientSession *session.ClientSession, reference string) *model.CryptoPaymentIntent {
		intent := cryptoTestIntent(model.CryptoAssetUsdcSolana, 50, server.NowUtc())
		intent.CreditedAt = &creditedAt
		return intent
	}
	linked, credited, err = cryptoCreditLinkedSolanaIntentInConn(nil, clientSession, search, "sig", 40)
	connect.AssertEqual(t, linked, true)
	connect.AssertEqual(t, credited, false)
	connect.AssertEqual(t, err, nil)
}

// ----- the scanner and the refill against the fake daemon (no database: the
// parts that need none) -----

func TestCryptoAvailabilityFollowsTheScanner(t *testing.T) {
	config := cryptoTestConfig()
	config.normalize()
	now := time.Date(2026, 10, 20, 18, 0, 0, 0, time.UTC)
	fresh := now.Add(-30 * time.Second)
	old := now.Add(-61 * time.Second)
	height := func(value int64) *int64 { return &value }

	available, _ := cryptoDecideAvailability(nil, config, model.CryptoAssetXmr, now)
	connect.AssertEqual(t, available, false)
	// a network height it does not know: no node, or none answered
	// (mmm/ZCASH-CONTRACT.md §3.13.10)
	available, reason := cryptoDecideAvailability(&model.CryptoScanCursor{LastSuccessAt: &fresh}, config, model.CryptoAssetXmr, now)
	connect.AssertEqual(t, available, false)
	connect.AssertEqual(t, reason, cryptoQuoteRefusedNoHeight)
	available, reason = cryptoDecideAvailability(&model.CryptoScanCursor{LastSuccessAt: &fresh, WalletHeight: height(100)}, config, model.CryptoAssetXmr, now)
	connect.AssertEqual(t, available, false)
	connect.AssertEqual(t, reason, cryptoQuoteRefusedNoHeight)
	available, _ = cryptoDecideAvailability(&model.CryptoScanCursor{LastSuccessAt: &fresh, WalletHeight: height(100), NetworkHeight: height(100)}, config, model.CryptoAssetXmr, now)
	connect.AssertEqual(t, available, true)
	// three scan intervals without a success
	available, reason = cryptoDecideAvailability(&model.CryptoScanCursor{LastSuccessAt: &old, WalletHeight: height(100), NetworkHeight: height(100)}, config, model.CryptoAssetXmr, now)
	connect.AssertEqual(t, available, false)
	connect.AssertEqual(t, reason, cryptoQuoteRefusedLagging)
	// the wallet more than max_lag_blocks behind the network
	available, _ = cryptoDecideAvailability(&model.CryptoScanCursor{LastSuccessAt: &fresh, WalletHeight: height(100), NetworkHeight: height(106)}, config, model.CryptoAssetXmr, now)
	connect.AssertEqual(t, available, false)
	available, _ = cryptoDecideAvailability(&model.CryptoScanCursor{LastSuccessAt: &fresh, WalletHeight: height(100), NetworkHeight: height(105)}, config, model.CryptoAssetXmr, now)
	connect.AssertEqual(t, available, true)
	available, _ = cryptoDecideAvailability(&model.CryptoScanCursor{LastSuccessAt: &fresh, WalletHeight: height(100), NetworkHeight: height(120)}, config, model.CryptoAssetZec, now)
	connect.AssertEqual(t, available, true)

	// NU7 makes zec's blocks three times faster: from its activation height
	// the lag allowed is max_lag_blocks_after_nu7 (60), the same 25 minutes
	config.Assets[model.CryptoAssetZec].Nu7ActivationHeight = 1000
	connect.AssertEqual(t, config.MaxLagBlocksAt(model.CryptoAssetZec, 999), int64(20))
	connect.AssertEqual(t, config.MaxLagBlocksAt(model.CryptoAssetZec, 1000), int64(60))
	available, _ = cryptoDecideAvailability(&model.CryptoScanCursor{LastSuccessAt: &fresh, WalletHeight: height(980), NetworkHeight: height(1001)}, config, model.CryptoAssetZec, now)
	connect.AssertEqual(t, available, false)
	available, _ = cryptoDecideAvailability(&model.CryptoScanCursor{LastSuccessAt: &fresh, WalletHeight: height(1100), NetworkHeight: height(1160)}, config, model.CryptoAssetZec, now)
	connect.AssertEqual(t, available, true)
	available, _ = cryptoDecideAvailability(&model.CryptoScanCursor{LastSuccessAt: &fresh, WalletHeight: height(1100), NetworkHeight: height(1161)}, config, model.CryptoAssetZec, now)
	connect.AssertEqual(t, available, false)
	// xmr is unaffected
	connect.AssertEqual(t, config.MaxLagBlocksAt(model.CryptoAssetXmr, 2000), int64(5))
}

// ----- the daemon clients against local servers that speak their wire -----

// fakeMoneroRpc is monero-wallet-rpc's wire: POST /json_rpc, digest auth (MD5,
// qop=auth, two challenges), every rpc error with HTTP 200, empty lists and
// zero confirmations left out.
type fakeMoneroRpc struct {
	user      string
	password  string
	nonce     string
	stateLock sync.Mutex
	calls     map[string]int
	answers   map[string]string
	// the last params each method was called with
	params map[string]map[string]any
}

// answer sets a method's answer while the server runs.
func (self *fakeMoneroRpc) answer(method string, answer string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.answers[method] = answer
}

// called is how often a method was called, and its last params.
func (self *fakeMoneroRpc) called(method string) (int, map[string]any) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.calls[method], self.params[method]
}

func (self *fakeMoneroRpc) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/json_rpc" || r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	authorization := r.Header.Get("Authorization")
	if !self.authorized(authorization) {
		w.Header().Add("WWW-Authenticate", fmt.Sprintf(`Digest qop="auth",algorithm=MD5-sess,realm="monero-rpc",nonce="%s",stale=false`, self.nonce))
		w.Header().Add("WWW-Authenticate", fmt.Sprintf(`Digest qop="auth",algorithm=MD5,realm="monero-rpc",nonce="%s",stale=false`, self.nonce))
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	body, _ := io.ReadAll(r.Body)
	var request struct {
		Method string         `json:"method"`
		Params map[string]any `json:"params"`
	}
	json.Unmarshal(body, &request)
	self.stateLock.Lock()
	self.calls[request.Method] += 1
	if self.params == nil {
		self.params = map[string]map[string]any{}
	}
	self.params[request.Method] = request.Params
	answer, ok := self.answers[request.Method]
	self.stateLock.Unlock()
	if !ok {
		answer = `{"error":{"code":-32601,"message":"Method not found"},"id":"0","jsonrpc":"2.0"}`
	}
	w.Header().Set("Content-Type", "application/json")
	io.WriteString(w, answer)
}

// authorized checks a digest response the way epee does.
func (self *fakeMoneroRpc) authorized(authorization string) bool {
	if !strings.HasPrefix(authorization, "Digest ") {
		return false
	}
	params := map[string]string{}
	for _, part := range splitCryptoDigestParams(authorization[7:]) {
		key, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		if ok {
			params[key] = strings.Trim(value, `"`)
		}
	}
	if params["nonce"] != self.nonce || params["realm"] != "monero-rpc" || params["algorithm"] != "MD5" || params["qop"] != "auth" {
		return false
	}
	ha1 := md5Hex(self.user + ":monero-rpc:" + self.password)
	ha2 := md5Hex("POST:" + params["uri"])
	want := md5Hex(ha1 + ":" + params["nonce"] + ":" + params["nc"] + ":" + params["cnonce"] + ":auth:" + ha2)
	return params["response"] == want && params["uri"] == "/json_rpc"
}

func TestMoneroWalletClientOverTheWire(t *testing.T) {
	rpc := &fakeMoneroRpc{
		user:     "synthetic-user",
		password: "synthetic-password",
		nonce:    "c3ludGhldGljLW5vbmNlLTE2Yg==",
		calls:    map[string]int{},
		answers: map[string]string{
			"get_height": `{"id":"0","jsonrpc":"2.0","result":{"height":2001}}`,
			// one mined receipt (confirmations 3), its pool copy, a pool receipt
			// without the confirmations key, change to the primary address,
			// another account, a time-locked coinbase, and our own spend
			"get_transfers": `{"id":"0","jsonrpc":"2.0","result":{
				"in":[
					{"txid":"AA","height":1998,"timestamp":1792531200,"amount":72386000000,"amounts":[72386000000],"type":"in","unlock_time":0,"subaddr_index":{"major":0,"minor":5},"address":"7sub5","double_spend_seen":false,"confirmations":3,"locked":true,"note":"","payment_id":"0000000000000000","fee":30000},
					{"txid":"cc","height":1999,"timestamp":1792531300,"amount":5,"type":"in","unlock_time":0,"subaddr_index":{"major":0,"minor":0},"address":"5primary","confirmations":2},
					{"txid":"dd","height":1999,"timestamp":1792531300,"amount":7,"type":"in","unlock_time":0,"subaddr_index":{"major":1,"minor":3},"address":"7other","confirmations":2},
					{"txid":"ee","height":1999,"timestamp":1792531300,"amount":600000000000,"type":"block","unlock_time":2059,"subaddr_index":{"major":0,"minor":0},"address":"5primary","confirmations":2}
				],
				"pool":[
					{"txid":"aa","height":0,"timestamp":1792531190,"amount":72386000000,"type":"pool","unlock_time":0,"subaddr_index":{"major":0,"minor":5},"address":"7sub5","double_spend_seen":false,"locked":true},
					{"txid":"bb","height":0,"timestamp":1792531190,"amount":150000000000,"amounts":[100000000000,50000000000],"type":"pool","unlock_time":0,"subaddr_index":{"major":0,"minor":6},"address":"7sub6","double_spend_seen":true,"locked":true}
				]}}`,
			"get_transfer_by_txid": `{"error":{"code":-8,"message":"Transaction not found."},"id":"0","jsonrpc":"2.0"}`,
			"create_address":       `{"id":"0","jsonrpc":"2.0","result":{"address":"7` + strings.Repeat("a", 94) + `","address_index":12,"addresses":["7` + strings.Repeat("a", 94) + `"],"address_indices":[12]}}`,
			"store":                `{"id":"0","jsonrpc":"2.0","result":{}}`,
			"get_address":          `{"id":"0","jsonrpc":"2.0","result":{"address":"5primaryaddress","addresses":[]}}`,
		},
	}
	httpServer := httptest.NewServer(rpc)
	defer httpServer.Close()

	wallet := newMoneroWallet(&cryptoWalletSettings{
		WalletRpcUrl:   httpServer.URL + "/json_rpc",
		WalletRpcUser:  "synthetic-user",
		WalletRpcPass:  "synthetic-password",
		PrimaryAddress: "5primaryaddress",
	}, &CryptoPayAssetConfig{Network: cryptoNetworkStagenet})
	ctx := context.Background()

	receipts, walletHeight, err := wallet.IncomingSince(ctx, 1990)
	connect.AssertEqual(t, err, nil)
	// get_height is the block count: the top block is one less
	connect.AssertEqual(t, walletHeight, uint64(2000))
	byKey := map[string]*cryptoWalletReceipt{}
	for _, receipt := range receipts {
		byKey[receipt.TxId+"/"+receipt.OutputKey] = receipt
	}
	// the mined copy wins over the pool copy
	connect.AssertEqual(t, len(receipts), 5)
	mined := byKey["aa/0.5"]
	connect.AssertEqual(t, mined.BlockHeight, uint64(1998))
	connect.AssertEqual(t, mined.Confirmations, uint64(3))
	connect.AssertEqual(t, *mined.AddressIndex, uint64(5))
	connect.AssertEqual(t, mined.AmountAtomic, uint64(72386000000))
	// a pool entry has no confirmations key: zero
	pool := byKey["bb/0.6"]
	connect.AssertEqual(t, pool.BlockHeight, uint64(0))
	connect.AssertEqual(t, pool.DoubleSpend, true)
	connect.AssertEqual(t, pool.AmountAtomic, uint64(150000000000))
	// the primary address and another account name no invoice
	connect.AssertEqual(t, byKey["cc/0.0"].AddressIndex, (*uint64)(nil))
	connect.AssertEqual(t, byKey["dd/1.3"].AddressIndex, (*uint64)(nil))
	connect.AssertEqual(t, byKey["ee/0.0"].UnlockTime, uint64(2059))

	_, found, err := wallet.Transaction(ctx, "ff")
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, found, false)

	// a new address is stored at once, and stays above the pool's high-water mark
	wallet.SetAddressHighWater(11)
	address, index, err := wallet.NewAddress(ctx, cryptoPoolAddressLabel)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, index, uint64(12))
	connect.AssertEqual(t, strings.HasPrefix(address, "7"), true)
	connect.AssertEqual(t, rpc.calls["store"], 1)

	connect.AssertEqual(t, wallet.CheckNetwork(ctx), nil)
	wrongNetwork := newMoneroWallet(&cryptoWalletSettings{
		WalletRpcUrl:   httpServer.URL + "/json_rpc",
		WalletRpcUser:  "synthetic-user",
		WalletRpcPass:  "synthetic-password",
		PrimaryAddress: "5primaryaddress",
	}, &CryptoPayAssetConfig{Network: cryptoNetworkMainnet})
	connect.AssertEqual(t, wrongNetwork.CheckNetwork(ctx) != nil, true)

	// wrong credentials are refused, and the error names no secret
	refused := newMoneroWallet(&cryptoWalletSettings{
		WalletRpcUrl:  httpServer.URL + "/json_rpc",
		WalletRpcUser: "synthetic-user",
		WalletRpcPass: "wrong-password",
	}, &CryptoPayAssetConfig{Network: cryptoNetworkStagenet})
	_, _, err = refused.IncomingSince(ctx, 0)
	connect.AssertEqual(t, err != nil, true)
	connect.AssertEqual(t, strings.Contains(err.Error(), "wrong-password"), false)

	// without monerod the node's and the network's heights are unknown, and
	// the coin is not sold (mmm/ZCASH-CONTRACT.md §3.13.10)
	walletTop, nodeTop, networkTop, err := wallet.Heights(ctx)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, walletTop, uint64(2000))
	connect.AssertEqual(t, nodeTop, uint64(0))
	connect.AssertEqual(t, networkTop, uint64(0))
	// with monerod: its top, and the height it syncs to (from its peers, not
	// from block times)
	monerod := &fakeMoneroRpc{
		user:     "synthetic-user",
		password: "synthetic-password",
		nonce:    "c3ludGhldGljLW5vbmNlLTE2Yg==",
		calls:    map[string]int{},
		answers: map[string]string{
			"get_info": `{"id":"0","jsonrpc":"2.0","result":{"height":2003,"target_height":0,"synchronized":true}}`,
		},
	}
	monerodServer := httptest.NewServer(monerod)
	defer monerodServer.Close()
	withNode := newMoneroWallet(&cryptoWalletSettings{
		WalletRpcUrl:   httpServer.URL + "/json_rpc",
		WalletRpcUser:  "synthetic-user",
		WalletRpcPass:  "synthetic-password",
		PrimaryAddress: "5primaryaddress",
		NodeRpcUrl:     monerodServer.URL + "/json_rpc",
		NodeRpcUser:    "synthetic-user",
		NodeRpcPass:    "synthetic-password",
	}, &CryptoPayAssetConfig{Network: cryptoNetworkStagenet})
	walletTop, nodeTop, networkTop, err = withNode.Heights(ctx)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, []uint64{walletTop, nodeTop, networkTop}, []uint64{2000, 2002, 2002})
	monerod.answer("get_info", `{"id":"0","jsonrpc":"2.0","result":{"height":2003,"target_height":2010,"synchronized":false}}`)
	_, _, networkTop, err = withNode.Heights(ctx)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, networkTop, uint64(2009))
}

// Every refill checks that the Monero wallet owns the pool's highest
// address, and widens the lookahead (with the wallet FILE's password) when a
// wallet rebuilt from keys does not; nothing of it is cached by the process
// (mmm/ZCASH-CONTRACT.md §3.13.9).
func TestMoneroLookaheadFollowsTheWallet(t *testing.T) {
	rpc := &fakeMoneroRpc{
		user:     "synthetic-user",
		password: "synthetic-password",
		nonce:    "c3ludGhldGljLW5vbmNlLTE2Yg==",
		calls:    map[string]int{},
		answers: map[string]string{
			"get_address_index":        `{"id":"0","jsonrpc":"2.0","result":{"index":{"major":0,"minor":600}}}`,
			"set_subaddress_lookahead": `{"id":"0","jsonrpc":"2.0","result":{}}`,
		},
	}
	httpServer := httptest.NewServer(rpc)
	defer httpServer.Close()
	settings := &cryptoWalletSettings{
		WalletRpcUrl:   httpServer.URL + "/json_rpc",
		WalletRpcUser:  "synthetic-user",
		WalletRpcPass:  "synthetic-password",
		PrimaryAddress: "5primaryaddress",
		WalletPassword: "synthetic-wallet-file-password",
	}
	wallet := newMoneroWallet(settings, &CryptoPayAssetConfig{Network: cryptoNetworkStagenet})
	ctx := context.Background()
	highest := "7" + strings.Repeat("h", 94)

	// owned: nothing to widen, and the check runs again on the next refill
	connect.AssertEqual(t, wallet.EnsureLookahead(ctx, 600, highest), nil)
	connect.AssertEqual(t, wallet.EnsureLookahead(ctx, 600, highest), nil)
	calls, params := rpc.called("get_address_index")
	connect.AssertEqual(t, calls, 2)
	connect.AssertEqual(t, params["address"], highest)
	calls, _ = rpc.called("set_subaddress_lookahead")
	connect.AssertEqual(t, calls, 0)

	// a wallet rebuilt from keys does not own it: widened past it with the
	// wallet file's password
	rpc.answer("get_address_index", `{"error":{"code":-2,"message":"Address doesn't belong to the wallet"},"id":"0","jsonrpc":"2.0"}`)
	connect.AssertEqual(t, wallet.EnsureLookahead(ctx, 600, highest), nil)
	calls, params = rpc.called("set_subaddress_lookahead")
	connect.AssertEqual(t, calls, 1)
	connect.AssertEqual(t, params["password"], "synthetic-wallet-file-password")
	connect.AssertEqual(t, params["major_idx"], float64(moneroDefaultMajorLookahead))
	connect.AssertEqual(t, params["minor_idx"], float64(600+moneroLookaheadMargin))
	// and again on the next refill while it still does not own it: no
	// process memory hides a wallet that lost its lookahead again
	connect.AssertEqual(t, wallet.EnsureLookahead(ctx, 600, highest), nil)
	calls, _ = rpc.called("set_subaddress_lookahead")
	connect.AssertEqual(t, calls, 2)

	// without the wallet file's password it cannot widen it: an error
	noPassword := *settings
	noPassword.WalletPassword = ""
	err := newMoneroWallet(&noPassword, &CryptoPayAssetConfig{Network: cryptoNetworkStagenet}).EnsureLookahead(ctx, 600, highest)
	connect.AssertEqual(t, err != nil, true)
	connect.AssertEqual(t, strings.Contains(err.Error(), "wallet_password"), true)

	// another wallet's address at that index is refused
	rpc.answer("get_address_index", `{"id":"0","jsonrpc":"2.0","result":{"index":{"major":0,"minor":601}}}`)
	connect.AssertEqual(t, wallet.EnsureLookahead(ctx, 600, highest) != nil, true)
	// any other failure is the refill's error
	rpc.answer("get_address_index", `{"error":{"code":-2,"message":"Invalid address"},"id":"0","jsonrpc":"2.0"}`)
	connect.AssertEqual(t, wallet.EnsureLookahead(ctx, 600, highest) != nil, true)
}

// fakeZecd is zecd's wire: bitcoind's dialect with positional params, basic
// auth, ZEC amounts with 8 decimals, and rpc errors as 404/500 with the
// envelope.
func fakeZecd(t testing.TB, answers map[string]string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, password, ok := r.BasicAuth()
		if !ok || user != "synthetic-user" || password != "synthetic-password" {
			w.Header().Set("WWW-Authenticate", `Basic realm="jsonrpc"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var request struct {
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		json.Unmarshal(body, &request)
		if 0 < len(request.Params) && request.Params[0] != '[' {
			t.Errorf("%s params are not a positional array: %s", request.Method, request.Params)
		}
		answer, ok := answers[request.Method]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"result":null,"error":{"code":-32601,"message":"Method not found"},"id":"0"}`)
			return
		}
		if strings.Contains(answer, `"error":{`) {
			w.WriteHeader(http.StatusInternalServerError)
		}
		io.WriteString(w, answer)
	}))
}

func TestZcashWalletClientOverTheWire(t *testing.T) {
	httpServer := fakeZecd(t, map[string]string{
		"getblockchaininfo": `{"result":{"chain":"test","blocks":4500000,"headers":4500002},"error":null,"id":"0"}`,
		// two outputs to one address in one transaction (summed), the same
		// output labelled orchard and ironwood (once), a transparent output, an
		// expired unmined one, and a send
		"z_listtransactions": `{"result":[
			{"txid":"AA","status":"mined","confirmations":12,"time":1792531200,"pool":"ironwood","diversifier_index":4609858083,"category":"receive","amount":0.02000000,"amountZat":2000000,"address":"utest1single","outindex":0,"blockheight":4499989,"blocktime":1792531200},
			{"txid":"AA","status":"mined","confirmations":12,"time":1792531200,"pool":"sapling","diversifier_index":4609858083,"category":"receive","amount":0.01081500,"amountZat":1081500,"address":"ztestsapling1x","outindex":0,"blockheight":4499989,"blocktime":1792531200},
			{"txid":"bb","status":"waiting","confirmations":0,"time":1792531300,"pool":"orchard","diversifier_index":4609858084,"category":"receive","amount":0.5,"amountZat":50000000,"address":"utest1other","outindex":1},
			{"txid":"bb","status":"waiting","confirmations":0,"time":1792531300,"pool":"ironwood","diversifier_index":4609858084,"category":"receive","amount":0.5,"amountZat":50000000,"address":"utest1other","outindex":1},
			{"txid":"cc","status":"mined","confirmations":3,"time":1792531400,"pool":"transparent","category":"receive","amount":1,"amountZat":100000000,"address":"tm1","outindex":2,"blockheight":4499998},
			{"txid":"dd","status":"expired","confirmations":-1,"time":1792531400,"pool":"orchard","diversifier_index":4609858085,"category":"receive","amount":1,"amountZat":100000000,"address":"utest1x","outindex":0},
			{"txid":"ee","status":"mined","confirmations":3,"time":1792531400,"pool":"orchard","category":"send","amount":-1,"amountZat":-100000000,"address":"utest1y","outindex":0,"blockheight":4499998}
		],"error":null,"id":"0"}`,
		"gettransaction":         `{"result":null,"error":{"code":-5,"message":"Invalid or non-wallet transaction id"},"id":"0"}`,
		"z_getaddressforaccount": `{"result":{"account":0,"diversifier_index":4609858090,"receiver_types":["sapling","orchard"],"address":"utest1fresh"},"error":null,"id":"0"}`,
	})
	defer httpServer.Close()

	wallet := newZcashWallet(&cryptoWalletSettings{
		WalletRpcUrl:  httpServer.URL + "/",
		WalletRpcUser: "synthetic-user",
		WalletRpcPass: "synthetic-password",
	}, &CryptoPayAssetConfig{Network: cryptoNetworkTestnet})
	ctx := context.Background()

	receipts, walletHeight, err := wallet.IncomingSince(ctx, 4499980)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, walletHeight, uint64(4500000))
	byKey := map[string]*cryptoWalletReceipt{}
	for _, receipt := range receipts {
		byKey[receipt.TxId+"/"+receipt.OutputKey] = receipt
	}
	connect.AssertEqual(t, len(receipts), 3)
	summed := byKey["aa/4609858083"]
	connect.AssertEqual(t, summed.AmountAtomic, uint64(3081500))
	connect.AssertEqual(t, *summed.AddressIndex, uint64(4609858083))
	connect.AssertEqual(t, summed.BlockHeight, uint64(4499989))
	connect.AssertEqual(t, summed.Confirmations, uint64(12))
	connect.AssertEqual(t, summed.Pool, "sapling,ironwood")
	relabelled := byKey["bb/4609858084"]
	connect.AssertEqual(t, relabelled.AmountAtomic, uint64(50000000))
	connect.AssertEqual(t, relabelled.BlockHeight, uint64(0))
	transparent := byKey["cc/t:2"]
	connect.AssertEqual(t, transparent.Pool, "transparent")
	connect.AssertEqual(t, transparent.AddressIndex, (*uint64)(nil))

	_, found, err := wallet.Transaction(ctx, "ff")
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, found, false)

	address, index, err := wallet.NewAddress(ctx, cryptoPoolAddressLabel)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, address, "utest1fresh")
	connect.AssertEqual(t, index, uint64(4609858090))

	connect.AssertEqual(t, wallet.CheckNetwork(ctx), nil)
	mainnetWallet := newZcashWallet(&cryptoWalletSettings{
		WalletRpcUrl:  httpServer.URL + "/",
		WalletRpcUser: "synthetic-user",
		WalletRpcPass: "synthetic-password",
	}, &CryptoPayAssetConfig{Network: cryptoNetworkMainnet})
	connect.AssertEqual(t, mainnetWallet.CheckNetwork(ctx) != nil, true)

	// ZEC amounts convert exactly; an index past int64 names no address of ours
	zatoshi, err := zcashParseZec(json.Number("0.08123456"))
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, zatoshi, int64(8123456))
	_, err = zcashParseZec(json.Number("0.000000001"))
	connect.AssertEqual(t, err != nil, true)
	_, ok := zcashParseIndex(json.Number("309485009821345068724781056"))
	connect.AssertEqual(t, ok, false)

	// without zebrad the network's height is unknown (mmm/ZCASH-CONTRACT.md §3.13.10)
	walletTop, nodeTop, networkTop, err := wallet.Heights(ctx)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, []uint64{walletTop, nodeTop, networkTop}, []uint64{4500000, 0, 0})
}

// zebrad's tip is the network's height. Its estimatedheight runs ahead during
// every slow block (it extrapolates from the tip's time), so it is taken only
// when the tip trails it by more than zcashNodeStaleBlocks: a zebrad stuck or
// syncing. On regtest it is ignored, and without zebrad zecd's view of the tip
// stands in there.
func TestZcashHeightsFollowZebrasTip(t *testing.T) {
	zecd := fakeZecd(t, map[string]string{
		"getblockchaininfo": `{"result":{"chain":"main","blocks":3500000,"headers":3500001},"error":null,"id":"0"}`,
	})
	defer zecd.Close()
	ctx := context.Background()
	heights := func(zebradInfo string, network string) []uint64 {
		settings := &cryptoWalletSettings{
			WalletRpcUrl:  zecd.URL + "/",
			WalletRpcUser: "synthetic-user",
			WalletRpcPass: "synthetic-password",
		}
		if zebradInfo != "" {
			zebrad := fakeZecd(t, map[string]string{"getblockchaininfo": zebradInfo})
			defer zebrad.Close()
			settings.NodeRpcUrl = zebrad.URL + "/"
			settings.NodeRpcUser = "synthetic-user"
			settings.NodeRpcPass = "synthetic-password"
		}
		walletTop, nodeTop, networkTop, err := newZcashWallet(settings, &CryptoPayAssetConfig{Network: network}).Heights(ctx)
		if err != nil {
			return nil
		}
		return []uint64{walletTop, nodeTop, networkTop}
	}
	// a slow block: the estimate is 30 blocks ahead of the tip, and ignored
	connect.AssertEqual(t, heights(`{"result":{"chain":"main","blocks":3500005,"estimatedheight":3500035},"error":null,"id":"0"}`, cryptoNetworkMainnet), []uint64{3500000, 3500005, 3500005})
	// zebrad stuck for hours: the estimate is the network's height, and the lag trips
	connect.AssertEqual(t, heights(`{"result":{"chain":"main","blocks":3500005,"estimatedheight":3500200},"error":null,"id":"0"}`, cryptoNetworkMainnet), []uint64{3500000, 3500005, 3500200})
	// no estimate at all
	connect.AssertEqual(t, heights(`{"result":{"chain":"main","blocks":3500005},"error":null,"id":"0"}`, cryptoNetworkMainnet), []uint64{3500000, 3500005, 3500005})
	// a zebrad with no chain says nothing
	connect.AssertEqual(t, heights(`{"result":{"chain":"main","blocks":0},"error":null,"id":"0"}`, cryptoNetworkMainnet), []uint64{3500000, 0, 0})

	regtestZecd := fakeZecd(t, map[string]string{
		"getblockchaininfo": `{"result":{"chain":"regtest","blocks":180,"headers":181},"error":null,"id":"0"}`,
	})
	defer regtestZecd.Close()
	regtest := func(zebradInfo string) []uint64 {
		settings := &cryptoWalletSettings{
			WalletRpcUrl:  regtestZecd.URL + "/",
			WalletRpcUser: "synthetic-user",
			WalletRpcPass: "synthetic-password",
		}
		if zebradInfo != "" {
			zebrad := fakeZecd(t, map[string]string{"getblockchaininfo": zebradInfo})
			defer zebrad.Close()
			settings.NodeRpcUrl = zebrad.URL + "/"
			settings.NodeRpcUser = "synthetic-user"
			settings.NodeRpcPass = "synthetic-password"
		}
		wallet := newZcashWallet(settings, &CryptoPayAssetConfig{Network: cryptoNetworkRegtest})
		connect.AssertEqual(t, wallet.CheckNetwork(ctx), nil)
		walletTop, nodeTop, networkTop, err := wallet.Heights(ctx)
		connect.AssertEqual(t, err, nil)
		return []uint64{walletTop, nodeTop, networkTop}
	}
	// regtest's blocks keep the genesis time: the estimate is meaningless
	connect.AssertEqual(t, regtest(`{"result":{"chain":"regtest","blocks":181,"estimatedheight":6600000},"error":null,"id":"0"}`), []uint64{180, 181, 181})
	connect.AssertEqual(t, regtest(""), []uint64{180, 181, 181})
	connect.AssertEqual(t, zcashChainName(cryptoNetworkRegtest), "regtest")
	connect.AssertEqual(t, zcashUnifiedAddressPrefix(cryptoNetworkRegtest), "uregtest1")
}

// The Zcash wallet identity check derives a stored pool address again at its
// own index and refuses a zecd that derives another (mmm/ZCASH-CONTRACT.md §3.13.13).
func TestZcashCheckAddressRederives(t *testing.T) {
	var lastParams json.RawMessage
	var stateLock sync.Mutex
	derived := "utest1stored"
	zecd := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var request struct {
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		json.Unmarshal(body, &request)
		stateLock.Lock()
		lastParams = request.Params
		address := derived
		stateLock.Unlock()
		io.WriteString(w, `{"result":{"account":0,"diversifier_index":4609858083,"receiver_types":["sapling","orchard"],"address":"`+address+`"},"error":null,"id":"0"}`)
	}))
	defer zecd.Close()
	wallet := newZcashWallet(&cryptoWalletSettings{
		WalletRpcUrl:  zecd.URL + "/",
		WalletRpcUser: "synthetic-user",
		WalletRpcPass: "synthetic-password",
	}, &CryptoPayAssetConfig{Network: cryptoNetworkTestnet})
	ctx := context.Background()

	connect.AssertEqual(t, wallet.CheckAddress(ctx, 4609858083, "utest1stored"), nil)
	stateLock.Lock()
	connect.AssertEqual(t, string(lastParams), `[0,["sapling","orchard"],4609858083]`)
	derived = "utest1another"
	stateLock.Unlock()
	err := wallet.CheckAddress(ctx, 4609858083, "utest1stored")
	connect.AssertEqual(t, err != nil, true)
	// the error names the index, never an address
	connect.AssertEqual(t, strings.Contains(err.Error(), "utest1"), false)
}

// Two outputs at the same action index in the Orchard and the Ironwood
// bundles of one mined transaction are two outputs; one output read unmined
// on one page and mined on the next is one.
func TestZcashReceiptsCountDistinctBundles(t *testing.T) {
	index := json.Number("4609858083")
	receipts := zcashReceipts([]*zcashOutput{
		{txId: "tx", vout: "0", pool: "orchard", diversifierIndex: &index, amountZatoshi: 1_000_000, confirmations: 12, blockHeight: 100},
		{txId: "tx", vout: "0", pool: "ironwood", diversifierIndex: &index, amountZatoshi: 2_000_000, confirmations: 12, blockHeight: 100},
	})
	connect.AssertEqual(t, len(receipts), 1)
	connect.AssertEqual(t, receipts[0].AmountAtomic, uint64(3_000_000))
	connect.AssertEqual(t, receipts[0].Pool, "orchard,ironwood")

	// a page read before the block, and the next after it
	receipts = zcashReceipts([]*zcashOutput{
		{txId: "tx", vout: "0", pool: "orchard", diversifierIndex: &index, amountZatoshi: 1_000_000, confirmations: 0},
		{txId: "tx", vout: "0", pool: "ironwood", diversifierIndex: &index, amountZatoshi: 1_000_000, confirmations: 1, blockHeight: 100},
	})
	connect.AssertEqual(t, len(receipts), 1)
	connect.AssertEqual(t, receipts[0].AmountAtomic, uint64(1_000_000))
	connect.AssertEqual(t, receipts[0].BlockHeight, uint64(100))
	// the same mined output on two overlapping pages
	receipts = zcashReceipts([]*zcashOutput{
		{txId: "tx", vout: "0", pool: "ironwood", diversifierIndex: &index, amountZatoshi: 1_000_000, confirmations: 2, blockHeight: 100},
		{txId: "tx", vout: "0", pool: "ironwood", diversifierIndex: &index, amountZatoshi: 1_000_000, confirmations: 2, blockHeight: 100},
	})
	connect.AssertEqual(t, receipts[0].AmountAtomic, uint64(1_000_000))
}

// RFC 2617's own example of a digest response.
func TestCryptoDigestAuthorization(t *testing.T) {
	challenge, ok := parseCryptoDigestChallenge(`Digest realm="testrealm@host.com", qop="auth,auth-int", nonce="dcd98b7102dd2f0e8b11d0f600bfb0c093", opaque="5ccc069c403ebaf9f0171e9517f40e41"`)
	connect.AssertEqual(t, ok, true)
	authorization := cryptoDigestAuthorization(challenge, "Mufasa", "Circle Of Life", "GET", "/dir/index.html", 1, "0a4f113b")
	connect.AssertEqual(t, strings.Contains(authorization, `response="6629fae49393a05397450978507c4ef1"`), true)
	connect.AssertEqual(t, strings.Contains(authorization, `nc=00000001`), true)
	connect.AssertEqual(t, strings.Contains(authorization, `opaque="5ccc069c403ebaf9f0171e9517f40e41"`), true)
	// MD5-sess is not answered
	_, ok = parseCryptoDigestChallenge(`Digest realm="monero-rpc", qop="auth", algorithm=MD5-sess, nonce="x"`)
	connect.AssertEqual(t, ok, false)
}
