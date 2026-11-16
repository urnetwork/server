package controller

// The coins on offer follow the quote path's availability: GET
// /pay/crypto/assets and the balance's crypto_assets never name a coin whose
// quote the server would refuse. Hermetic: the scan cursors are fakes
// (cryptoGetScanCursor), so no database is read.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// cryptoTestAvailabilityClock fixes the availability's clock at `now` and
// returns it, for the test to move.
func cryptoTestAvailabilityClock(t testing.TB, now time.Time) *time.Time {
	clock := now
	previous := cryptoAvailabilityNow
	cryptoAvailabilityNow = func() time.Time {
		return clock
	}
	t.Cleanup(func() {
		cryptoAvailabilityNow = previous
	})
	return &clock
}

// cryptoTestScanCursors answers the availability's cursor reads from
// `cursors` (a coin it does not name has never been scanned) and returns how
// many reads each coin got.
func cryptoTestScanCursors(t testing.TB, cursors map[string]*model.CryptoScanCursor) map[string]int {
	previous := cryptoGetScanCursor
	reads := map[string]int{}
	cryptoGetScanCursor = func(ctx context.Context, asset string) *model.CryptoScanCursor {
		reads[asset] += 1
		return cursors[asset]
	}
	resetCryptoAvailability()
	restoreSeams := cryptoTestHermeticSeams()
	t.Cleanup(func() {
		cryptoGetScanCursor = previous
		restoreSeams()
		resetCryptoAvailability()
	})
	return reads
}

// cryptoTestAssetNames is the coins of a list, in its order.
func cryptoTestAssetNames(assets []*CryptoPaymentAsset) []string {
	names := []string{}
	for _, asset := range assets {
		names = append(names, asset.Asset)
	}
	return names
}

// A coin whose quotes are refused is never offered. On 2026-11-10 GET
// /pay/crypto/assets listed zec while every zec quote was refused with
// scanner_lagging: the zec wallet's scan cursor was about 15,900 blocks
// behind the network. Every client offers each coin the list names (the
// sdk's PaymentMethods), so the checkout offered Zcash and its invoice
// failed. A scanner that lags, has gone stale, or does not know a height
// leaves its coin out of the list and out of the balance's crypto_assets,
// for the same reason the quote path refuses it.
func TestCryptoAssetsOfferOnlyCoinsThatCanBeQuoted(t *testing.T) {
	now := server.NowUtc()
	fresh := now.Add(-5 * time.Second)
	// more than three 20 s scan intervals ago
	stale := now.Add(-61 * time.Second)
	height := func(value int64) *int64 { return &value }
	caughtUp := func(asset string) *model.CryptoScanCursor {
		return &model.CryptoScanCursor{Asset: asset, LastSuccessAt: &fresh, WalletHeight: height(3_100_000), NetworkHeight: height(3_100_000)}
	}
	cases := []struct {
		name   string
		cursor *model.CryptoScanCursor
		reason string
	}{
		{
			name:   "the wallet 15,900 blocks behind the network",
			cursor: &model.CryptoScanCursor{Asset: model.CryptoAssetZec, LastSuccessAt: &fresh, WalletHeight: height(3_100_000), NetworkHeight: height(3_115_900)},
			reason: cryptoQuoteRefusedLagging,
		},
		{
			name:   "no scan succeeded within three scan intervals",
			cursor: &model.CryptoScanCursor{Asset: model.CryptoAssetZec, LastSuccessAt: &stale, WalletHeight: height(3_100_000), NetworkHeight: height(3_100_000)},
			reason: cryptoQuoteRefusedLagging,
		},
		{
			name:   "a scan that never succeeded",
			cursor: &model.CryptoScanCursor{Asset: model.CryptoAssetZec, LastError: "synthetic: the wallet daemon cannot follow NU7"},
			reason: cryptoQuoteRefusedLagging,
		},
		{
			name:   "the network's height is not known",
			cursor: &model.CryptoScanCursor{Asset: model.CryptoAssetZec, LastSuccessAt: &fresh, WalletHeight: height(3_100_000)},
			reason: cryptoQuoteRefusedNoHeight,
		},
		{
			name:   "the wallet's height is not known",
			cursor: &model.CryptoScanCursor{Asset: model.CryptoAssetZec, LastSuccessAt: &fresh, NetworkHeight: height(3_100_000)},
			reason: cryptoQuoteRefusedNoHeight,
		},
		{
			name:   "never scanned",
			cursor: nil,
			reason: cryptoQuoteRefusedLagging,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cryptoTestEnable(t, cryptoTestConfig(), map[string]*fakeCryptoWallet{})
			cryptoTestScanCursors(t, map[string]*model.CryptoScanCursor{
				model.CryptoAssetZec: c.cursor,
				model.CryptoAssetXmr: caughtUp(model.CryptoAssetXmr),
			})
			ctx := context.Background()

			// the quote path refuses zec, and quotes xmr
			available, reason := cryptoAssetAvailable(ctx, cryptoPayConfigFunc(), model.CryptoAssetZec)
			connect.AssertEqual(t, available, false)
			connect.AssertEqual(t, reason, c.reason)
			available, _ = cryptoAssetAvailable(ctx, cryptoPayConfigFunc(), model.CryptoAssetXmr)
			connect.AssertEqual(t, available, true)

			// so neither the list nor the balance offers zec
			result, err := CryptoPaymentAssets(&session.ClientSession{Ctx: ctx})
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, cryptoTestAssetNames(result.Assets), []string{model.CryptoAssetUsdcSolana, model.CryptoAssetXmr})
			balance := &SubscriptionBalanceResult{}
			decorateCryptoPay(balance, &session.ClientSession{Ctx: ctx})
			connect.AssertEqual(t, cryptoTestAssetNames(balance.CryptoAssets), []string{model.CryptoAssetUsdcSolana, model.CryptoAssetXmr})
		})
	}

	// once the scanner is caught up, zec is offered again
	cryptoTestEnable(t, cryptoTestConfig(), map[string]*fakeCryptoWallet{})
	cryptoTestScanCursors(t, map[string]*model.CryptoScanCursor{
		model.CryptoAssetZec: caughtUp(model.CryptoAssetZec),
		model.CryptoAssetXmr: caughtUp(model.CryptoAssetXmr),
	})
	result, err := CryptoPaymentAssets(&session.ClientSession{Ctx: context.Background()})
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, cryptoTestAssetNames(result.Assets), []string{model.CryptoAssetUsdcSolana, model.CryptoAssetZec, model.CryptoAssetXmr})
}

// The list also names the coins that are on but cannot be paid with now, in
// unavailable_assets, each with available false and the quote path's reason,
// and what it would sell, so a page can show "temporarily unavailable" where
// the coin would sell its item. They are never in assets, which is all an
// older client reads, nor in the balance's crypto_assets, which every app
// reads. A coin switched off is in neither list: it is not sold at all.
func TestCryptoAssetsNameTheCoinsThatAreTemporarilyUnavailable(t *testing.T) {
	cryptoTestEnable(t, cryptoTestConfig(), map[string]*fakeCryptoWallet{})
	now := server.NowUtc()
	fresh := now.Add(-5 * time.Second)
	height := func(value int64) *int64 { return &value }
	reads := cryptoTestScanCursors(t, map[string]*model.CryptoScanCursor{
		model.CryptoAssetZec: {Asset: model.CryptoAssetZec, LastSuccessAt: &fresh, WalletHeight: height(3_100_000), NetworkHeight: height(3_115_900)},
		model.CryptoAssetXmr: {Asset: model.CryptoAssetXmr, LastSuccessAt: &fresh, WalletHeight: height(3_500_000)},
	})
	ctx := context.Background()

	result, err := CryptoPaymentAssets(&session.ClientSession{Ctx: ctx})
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, cryptoTestAssetNames(result.Assets), []string{model.CryptoAssetUsdcSolana})
	connect.AssertEqual(t, result.Assets[0].Available, true)
	connect.AssertEqual(t, result.Assets[0].UnavailableReason, "")
	connect.AssertEqual(t, cryptoTestAssetNames(result.UnavailableAssets), []string{model.CryptoAssetZec, model.CryptoAssetXmr})
	zec := result.UnavailableAssets[0]
	connect.AssertEqual(t, zec.Available, false)
	connect.AssertEqual(t, zec.UnavailableReason, cryptoQuoteRefusedLagging)
	connect.AssertEqual(t, zec.Plans, []string{model.SolanaPlanYearly, model.SolanaPlanYearlyOnboarding})
	connect.AssertEqual(t, zec.Items, []string{StripeItemData1Tib, StripeItemData10Tib})
	connect.AssertEqual(t, zec.ConfirmationsRequired, 10)
	connect.AssertEqual(t, zec.QuoteTtlSeconds, 1800)
	xmr := result.UnavailableAssets[1]
	connect.AssertEqual(t, xmr.Available, false)
	connect.AssertEqual(t, xmr.UnavailableReason, cryptoQuoteRefusedNoHeight)
	// the same reasons the quote path refuses with
	for _, listed := range result.UnavailableAssets {
		available, reason := cryptoAssetAvailable(ctx, cryptoPayConfigFunc(), listed.Asset)
		connect.AssertEqual(t, available, false)
		connect.AssertEqual(t, reason, listed.UnavailableReason)
	}
	// USDC has no scanner, and reads no cursor
	_, usdcRead := reads[model.CryptoAssetUsdcSolana]
	connect.AssertEqual(t, usdcRead, false)

	// the wire: available on every coin, a reason only where it is false
	resultJson, err := json.Marshal(result)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, strings.Contains(string(resultJson), `"quote_ttl_seconds":3600,"available":true}`), true)
	connect.AssertEqual(t, strings.Contains(string(resultJson), `"available":false,"unavailable_reason":"scanner_lagging"}`), true)
	connect.AssertEqual(t, strings.Contains(string(resultJson), `"available":false,"unavailable_reason":"network_height_unknown"}`), true)
	connect.AssertEqual(t, strings.Count(string(resultJson), `"unavailable_reason"`), 2)

	// the balance offers only what can be paid with now; the unavailable
	// coins ride in a list of their own, for an app that shows them greyed
	balance := &SubscriptionBalanceResult{}
	decorateCryptoPay(balance, &session.ClientSession{Ctx: ctx})
	connect.AssertEqual(t, cryptoTestAssetNames(balance.CryptoAssets), []string{model.CryptoAssetUsdcSolana})
	connect.AssertEqual(t, cryptoTestAssetNames(balance.CryptoUnavailableAssets), []string{model.CryptoAssetZec, model.CryptoAssetXmr})
	offeredJson, err := json.Marshal(balance.CryptoAssets)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, strings.Contains(string(offeredJson), `"available":false`), false)
	connect.AssertEqual(t, strings.Contains(string(offeredJson), "unavailable"), false)

	// a coin switched off is in neither list
	cryptoPayConfigFunc().Assets[model.CryptoAssetZec].Enabled = false
	result, err = CryptoPaymentAssets(&session.ClientSession{Ctx: ctx})
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, cryptoTestAssetNames(result.Assets), []string{model.CryptoAssetUsdcSolana})
	connect.AssertEqual(t, cryptoTestAssetNames(result.UnavailableAssets), []string{model.CryptoAssetXmr})

	// nothing unavailable: unavailable_assets is [], never null
	cryptoTestEnable(t, cryptoTestConfig(), map[string]*fakeCryptoWallet{})
	cryptoTestScanCursors(t, map[string]*model.CryptoScanCursor{
		model.CryptoAssetZec: {Asset: model.CryptoAssetZec, LastSuccessAt: &fresh, WalletHeight: height(3_100_000), NetworkHeight: height(3_100_000)},
		model.CryptoAssetXmr: {Asset: model.CryptoAssetXmr, LastSuccessAt: &fresh, WalletHeight: height(3_500_000), NetworkHeight: height(3_500_000)},
	})
	result, err = CryptoPaymentAssets(&session.ClientSession{Ctx: ctx})
	connect.AssertEqual(t, err, nil)
	resultJson, err = json.Marshal(result)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, strings.HasSuffix(string(resultJson), `,"unavailable_assets":[]}`), true)
	connect.AssertEqual(t, strings.Contains(string(resultJson), "null"), false)

	// nothing on: both lists [], never null
	setCryptoPayConfigForTest(&CryptoPayConfig{})
	result, err = CryptoPaymentAssets(&session.ClientSession{Ctx: ctx})
	connect.AssertEqual(t, err, nil)
	resultJson, err = json.Marshal(result)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, string(resultJson), `{"assets":[],"unavailable_assets":[]}`)
}

// The coins on offer read the quote path's cached decision, so the two agree,
// and the cache keeps its TTL: a coin's cursor is read at most once per
// cryptoAvailabilityCacheTtl per process, by whichever of the list, the
// balance, a quote or a re-quote asks first, and the decision holds until the
// TTL has passed even when the scanner moved.
func TestCryptoAvailabilityCacheHoldsForItsTtl(t *testing.T) {
	cryptoTestEnable(t, cryptoTestConfig(), map[string]*fakeCryptoWallet{})
	start := time.Date(2026, 11, 10, 18, 0, 0, 0, time.UTC)
	clock := cryptoTestAvailabilityClock(t, start)
	height := func(value int64) *int64 { return &value }
	lastSuccessAt := start.Add(-5 * time.Second)
	caughtUp := &model.CryptoScanCursor{Asset: model.CryptoAssetZec, LastSuccessAt: &lastSuccessAt, WalletHeight: height(3_100_000), NetworkHeight: height(3_100_000)}
	lagging := &model.CryptoScanCursor{Asset: model.CryptoAssetZec, LastSuccessAt: &lastSuccessAt, WalletHeight: height(3_100_000), NetworkHeight: height(3_115_900)}
	cursors := map[string]*model.CryptoScanCursor{
		model.CryptoAssetZec: caughtUp,
		model.CryptoAssetXmr: {Asset: model.CryptoAssetXmr, LastSuccessAt: &lastSuccessAt, WalletHeight: height(3_500_000), NetworkHeight: height(3_500_000)},
	}
	reads := cryptoTestScanCursors(t, cursors)
	ctx := context.Background()
	listed := func() []string {
		t.Helper()
		result, err := CryptoPaymentAssets(&session.ClientSession{Ctx: ctx})
		connect.AssertEqual(t, err, nil)
		return cryptoTestAssetNames(result.Assets)
	}
	all := []string{model.CryptoAssetUsdcSolana, model.CryptoAssetZec, model.CryptoAssetXmr}
	withoutZec := []string{model.CryptoAssetUsdcSolana, model.CryptoAssetXmr}

	connect.AssertEqual(t, listed(), all)
	connect.AssertEqual(t, reads[model.CryptoAssetZec], 1)

	// the scanner falls behind; until the TTL has passed every reader keeps
	// the decision, and no one reads the cursor again
	cursors[model.CryptoAssetZec] = lagging
	*clock = start.Add(cryptoAvailabilityCacheTtl - time.Millisecond)
	connect.AssertEqual(t, listed(), all)
	available, reason := cryptoAssetAvailable(ctx, cryptoPayConfigFunc(), model.CryptoAssetZec)
	connect.AssertEqual(t, available, true)
	connect.AssertEqual(t, reason, "")
	balance := &SubscriptionBalanceResult{}
	decorateCryptoPay(balance, &session.ClientSession{Ctx: ctx})
	connect.AssertEqual(t, cryptoTestAssetNames(balance.CryptoAssets), all)
	connect.AssertEqual(t, reads[model.CryptoAssetZec], 1)

	// at the TTL the list reads again, and the quote path has its answer
	// without another read
	*clock = start.Add(cryptoAvailabilityCacheTtl)
	connect.AssertEqual(t, listed(), withoutZec)
	connect.AssertEqual(t, reads[model.CryptoAssetZec], 2)
	available, reason = cryptoAssetAvailable(ctx, cryptoPayConfigFunc(), model.CryptoAssetZec)
	connect.AssertEqual(t, available, false)
	connect.AssertEqual(t, reason, cryptoQuoteRefusedLagging)
	connect.AssertEqual(t, reads[model.CryptoAssetZec], 2)

	// caught up again: a quote asks first this time, and the list follows it
	cursors[model.CryptoAssetZec] = caughtUp
	*clock = start.Add(2*cryptoAvailabilityCacheTtl - time.Millisecond)
	connect.AssertEqual(t, listed(), withoutZec)
	connect.AssertEqual(t, reads[model.CryptoAssetZec], 2)
	*clock = start.Add(2 * cryptoAvailabilityCacheTtl)
	available, _ = cryptoAssetAvailable(ctx, cryptoPayConfigFunc(), model.CryptoAssetZec)
	connect.AssertEqual(t, available, true)
	connect.AssertEqual(t, reads[model.CryptoAssetZec], 3)
	connect.AssertEqual(t, listed(), all)
	connect.AssertEqual(t, reads[model.CryptoAssetZec], 3)
	// one read per TTL for xmr too, whoever asked
	connect.AssertEqual(t, reads[model.CryptoAssetXmr], 3)
}

// A scanner state that cannot be read (a database error) leaves only that
// coin out, of both lists, for that answer: the list and the balance still
// answer with the others, nothing is cached, and the next answer reads again.
func TestCryptoAssetsLeaveOutACoinWhoseScannerCannotBeRead(t *testing.T) {
	cryptoTestEnable(t, cryptoTestConfig(), map[string]*fakeCryptoWallet{})
	lastSuccessAt := server.NowUtc()
	height := int64(3_500_000)
	reads := map[string]int{}
	previous := cryptoGetScanCursor
	cryptoGetScanCursor = func(ctx context.Context, asset string) *model.CryptoScanCursor {
		reads[asset] += 1
		if asset == model.CryptoAssetZec {
			panic(errors.New("synthetic: the database is unavailable"))
		}
		return &model.CryptoScanCursor{Asset: asset, LastSuccessAt: &lastSuccessAt, WalletHeight: &height, NetworkHeight: &height}
	}
	t.Cleanup(func() {
		cryptoGetScanCursor = previous
	})
	ctx := context.Background()

	for i := 1; i <= 2; i += 1 {
		result, err := CryptoPaymentAssets(&session.ClientSession{Ctx: ctx})
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, cryptoTestAssetNames(result.Assets), []string{model.CryptoAssetUsdcSolana, model.CryptoAssetXmr})
		connect.AssertEqual(t, cryptoTestAssetNames(result.UnavailableAssets), []string{})
		connect.AssertEqual(t, reads[model.CryptoAssetZec], i)
	}
	balance := &SubscriptionBalanceResult{}
	decorateCryptoPay(balance, &session.ClientSession{Ctx: ctx})
	connect.AssertEqual(t, cryptoTestAssetNames(balance.CryptoAssets), []string{model.CryptoAssetUsdcSolana, model.CryptoAssetXmr})
	connect.AssertEqual(t, reads[model.CryptoAssetZec], 3)
	connect.AssertEqual(t, reads[model.CryptoAssetXmr], 1)
}
