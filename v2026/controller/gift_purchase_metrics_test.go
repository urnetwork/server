package controller

// urnetwork_gift_purchases_total (gift_purchase_metrics.go): every answer of
// the three gift purchase routes counts once, by rail and result. The
// refusals and the label rules need no database; each rail's success does
// (server.DefaultTestEnv).

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// The answers counted so far for the rail and result.
func giftPurchaseTestCount(rail string, result string) float64 {
	return testutil.ToFloat64(giftPurchases.WithLabelValues(rail, result))
}

// A known refusal code is its own result, any other refusal is other, an
// error (or no answer) is error, and every other answer is ok.
func TestGiftPurchaseResultLabel(t *testing.T) {
	connect.AssertEqual(t, giftPurchaseResultLabel(nil, true, false, ""), giftPurchaseResultOk)
	connect.AssertEqual(t, giftPurchaseResultLabel(nil, true, true, PurchaseErrorCodeRateLimited), PurchaseErrorCodeRateLimited)
	connect.AssertEqual(t, giftPurchaseResultLabel(nil, true, true, GiftErrorCodeDailyLimit), GiftErrorCodeDailyLimit)
	connect.AssertEqual(t, giftPurchaseResultLabel(nil, true, true, GammaErrorCodeInsufficientGamma), GammaErrorCodeInsufficientGamma)
	connect.AssertEqual(t, giftPurchaseResultLabel(nil, true, true, "a code from a future check"), giftPurchaseResultOther)
	connect.AssertEqual(t, giftPurchaseResultLabel(nil, true, true, ""), giftPurchaseResultOther)
	connect.AssertEqual(t, giftPurchaseResultLabel(context.Canceled, true, false, ""), giftPurchaseResultError)
	connect.AssertEqual(t, giftPurchaseResultLabel(nil, false, false, ""), giftPurchaseResultError)

	// a closed label set: every rail's every result is a series from zero
	results := giftPurchaseResults()
	seen := map[string]bool{}
	for _, result := range results {
		if seen[result] {
			t.Fatalf("result %q is listed twice", result)
		}
		seen[result] = true
	}
	connect.AssertEqual(t, len(results), len(giftPurchaseRefusalCodes)+3)
	connect.AssertEqual(t, testutil.CollectAndCount(giftPurchases, "urnetwork_gift_purchases_total") >= len(giftPurchaseRails)*len(results), true)
}

// Each rail counts a refusal under its rail and its code: sales closed, then
// an invalid quantity, both refused before anything is stored.
func TestGiftPurchasesCountRefusalsByRail(t *testing.T) {
	clientSession := giftPurchaseTestSession()
	purchase := func(rail string, args GiftPurchaseArgs) string {
		t.Helper()
		switch rail {
		case giftPurchaseRailCard:
			result, err := GiftCheckout(&GiftCheckoutArgs{GiftPurchaseArgs: args}, clientSession)
			connect.AssertEqual(t, err, nil)
			return result.Error.Code
		case giftPurchaseRailCoin:
			result, err := GiftCryptoIntent(&GiftCryptoIntentArgs{GiftPurchaseArgs: args, Asset: model.CryptoAssetXmr}, clientSession)
			connect.AssertEqual(t, err, nil)
			return result.Error.Code
		default:
			result, err := GiftGammaIntent(&GiftGammaIntentArgs{GiftPurchaseArgs: args}, clientSession)
			connect.AssertEqual(t, err, nil)
			return result.Error.Code
		}
	}

	for _, rail := range giftPurchaseRails {
		closedBefore := giftPurchaseTestCount(rail, GiftErrorCodeSalesClosed)
		quantityBefore := giftPurchaseTestCount(rail, GiftErrorCodeInvalidQuantity)
		okBefore := giftPurchaseTestCount(rail, giftPurchaseResultOk)

		closed := model.Testing_GiftConfig()
		closed.Enabled = false
		restore := model.Testing_SetGiftConfig(closed)
		connect.AssertEqual(t, purchase(rail, giftPurchaseTestArgs()), GiftErrorCodeSalesClosed)
		restore()

		restore = model.Testing_SetGiftConfig(model.Testing_GiftConfig())
		args := giftPurchaseTestArgs()
		args.Quantity = 21
		connect.AssertEqual(t, purchase(rail, args), GiftErrorCodeInvalidQuantity)
		restore()

		connect.AssertEqual(t, giftPurchaseTestCount(rail, GiftErrorCodeSalesClosed)-closedBefore, float64(1))
		connect.AssertEqual(t, giftPurchaseTestCount(rail, GiftErrorCodeInvalidQuantity)-quantityBefore, float64(1))
		connect.AssertEqual(t, giftPurchaseTestCount(rail, giftPurchaseResultOk)-okBefore, float64(0))
	}
}

// A card purchase that starts its Checkout session counts as ok, and the
// per-address limit's refusal as rate_limited, under the card rail.
func TestGiftPurchasesCountCardSuccess(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		giftPurchaseTestConfig(t)
		giftPurchaseTestResetLimiters()
		newGiftStripeTestApi(t)
		clientSession := giftPurchaseTestSession()
		okBefore := giftPurchaseTestCount(giftPurchaseRailCard, giftPurchaseResultOk)
		limitedBefore := giftPurchaseTestCount(giftPurchaseRailCard, PurchaseErrorCodeRateLimited)

		result, err := GiftCheckout(&GiftCheckoutArgs{GiftPurchaseArgs: giftPurchaseTestArgs()}, clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Error == nil, true)
		connect.AssertEqual(t, result.OrderId != "", true)
		connect.AssertEqual(t, giftPurchaseTestCount(giftPurchaseRailCard, giftPurchaseResultOk)-okBefore, float64(1))

		// the address's purchases used up (every test session shares one
		// address): refused, and counted as such
		for range giftPurchaseLimiter.limitPerMinute + 1 {
			giftPurchaseLimiter.allow(clientSession)
		}
		result, err = GiftCheckout(&GiftCheckoutArgs{GiftPurchaseArgs: giftPurchaseTestArgs()}, clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Error.Code, PurchaseErrorCodeRateLimited)
		connect.AssertEqual(t, giftPurchaseTestCount(giftPurchaseRailCard, PurchaseErrorCodeRateLimited)-limitedBefore, float64(1))
		connect.AssertEqual(t, giftPurchaseTestCount(giftPurchaseRailCard, giftPurchaseResultOk)-okBefore, float64(1))
	})
}

// A coin purchase that hands out its invoice counts as ok under the coin rail.
func TestGiftPurchasesCountCoinSuccess(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		giftPurchaseTestConfig(t)
		giftPurchaseTestResetLimiters()
		cryptoTestStandardRates(t)
		wallet := newFakeCryptoWallet(model.CryptoAssetZec)
		cryptoTestEnable(t, cryptoTestConfig(), map[string]*fakeCryptoWallet{model.CryptoAssetZec: wallet})
		taskSession := session.Testing_CreateClientSession(ctx, nil)
		_, err := RunCryptoAddressRefill(taskSession, model.CryptoAssetZec)
		connect.AssertEqual(t, err, nil)
		_, err = RunCryptoPaymentScan(taskSession, model.CryptoAssetZec)
		connect.AssertEqual(t, err, nil)
		resetCryptoAvailability()
		okBefore := giftPurchaseTestCount(giftPurchaseRailCoin, giftPurchaseResultOk)

		invoice, err := GiftCryptoIntent(&GiftCryptoIntentArgs{
			GiftPurchaseArgs: giftPurchaseTestArgs(),
			Asset:            model.CryptoAssetZec,
		}, session.Testing_CreateClientSession(ctx, nil))
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, invoice.Error == nil, true)
		connect.AssertEqual(t, invoice.OrderId != "", true)
		connect.AssertEqual(t, giftPurchaseTestCount(giftPurchaseRailCoin, giftPurchaseResultOk)-okBefore, float64(1))
	})
}

// A Gamma purchase that records its intent counts as ok under the gamma rail.
func TestGiftPurchasesCountGammaSuccess(t *testing.T) {
	skipWithoutProYml(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newGammaDbFixture(t)
		giftPurchaseTestConfig(t)
		_, _, clientSession := cryptoDbTestNetwork(t, f.ctx, "giftpurchasegamma")
		connect.AssertEqual(t, f.run().Readiness, gammaReady)
		okBefore := giftPurchaseTestCount(giftPurchaseRailGamma, giftPurchaseResultOk)

		intent := giftGammaTestIntent(t, clientSession, giftGammaTestArgs(1, model.GiftDeliveryMe))
		if intent.Error != nil {
			t.Fatalf("intent refused: %+v", intent.Error)
		}
		connect.AssertEqual(t, intent.OrderId != "", true)
		connect.AssertEqual(t, giftPurchaseTestCount(giftPurchaseRailGamma, giftPurchaseResultOk)-okBefore, float64(1))
	})
}
